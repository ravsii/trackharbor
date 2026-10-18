package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/ravsii/trackharbor/internal/config"
	"github.com/ravsii/trackharbor/internal/library"
	"github.com/ravsii/trackharbor/internal/store"
	"github.com/ravsii/trackharbor/spotify"
	"github.com/ravsii/trackharbor/vk"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	store       *store.Store
	logger      *zap.Logger
	mu          sync.Mutex
	settingsMu  sync.Mutex
	importMu    sync.Mutex
	proxyImport proxyImportState
	auth        *authentication
	status      map[string]string
	pingStatus  map[string]string
	proxyPings  map[int64]bool
	downloads   *downloadJob
}

func New(db *store.Store, logger *zap.Logger, configs ...config.Config) http.Handler {
	var cfg config.Config
	if len(configs) != 0 {
		cfg = configs[0]
	}
	if err := cfg.ValidateJWTSigningKey(); err != nil {
		panic(err)
	}
	s := &Server{store: db, logger: logger, status: map[string]string{}, pingStatus: map[string]string{}}
	s.auth = newAuthentication(db, cfg.JWTSigningKey)
	if len(configs) != 0 {
		s.downloads = newDownloadJob(db, configs[0], logger)
		stopped, err := db.DownloadsStopped()
		if err != nil {
			logger.Error("read saved download setting failed")
			stopped = true
		}
		if stopped {
			s.downloads.stopped = true
			s.downloads.state = downloadState{Stopped: true, Stage: "stopped", Message: "Downloads stopped."}
		} else {
			go func() {
				tracks, err := db.Downloadable()
				if err != nil {
					logger.Error("load library for startup download check failed")
					return
				}
				s.downloads.enqueue(tracks)
			}()
		}
	}
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(static)))
	mux.HandleFunc("GET /", s.page)
	mux.HandleFunc("GET /status", s.currentStatus)
	mux.HandleFunc("GET /ping-status", s.currentPingStatus)
	mux.HandleFunc("POST /connect", s.connect)
	mux.HandleFunc("POST /ping", s.pingSource)
	mux.HandleFunc("POST /sync", s.syncSource)
	mux.HandleFunc("POST /youtube/add", s.addYouTubeVideo)
	mux.HandleFunc("GET /download/status", s.downloadStatus)
	mux.HandleFunc("POST /download/start", s.startDownloads)
	mux.HandleFunc("POST /download/stop", s.stopDownloads)
	mux.HandleFunc("GET /download/events", s.downloadEvents)
	mux.HandleFunc("POST /settings/proxy-sources", s.saveProxySources)
	mux.HandleFunc("POST /settings/downloads", s.saveDownloadSettings)
	mux.HandleFunc("POST /settings/public/import", s.importPublicProxies)
	mux.HandleFunc("GET /settings/public/import/status", s.publicProxyImportStatus)
	mux.HandleFunc("POST /settings/personal", s.addPersonalProxy)
	mux.HandleFunc("POST /settings/personal/{id}/delete", s.deletePersonalProxy)
	mux.HandleFunc("POST /settings/personal/{id}/ping", s.pingPersonalProxy)
	mux.HandleFunc("GET /settings/personal/{id}/address", s.personalProxyAddress)
	mux.HandleFunc("GET /audio", s.playAudio)
	mux.HandleFunc("GET /audio/tracks", s.playableTracks)
	for _, method := range []string{"GET", "OPTIONS", "PROPFIND"} {
		mux.HandleFunc(method+" /webdav", s.serveWebDAV)
		mux.HandleFunc(method+" /webdav/", s.serveWebDAV)
	}
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/status", s.authStatus)
	mux.HandleFunc("POST /settings/users", s.createUser)
	mux.HandleFunc("POST /settings/users/{id}/password", s.changePassword)
	mux.HandleFunc("POST /settings/users/{id}/delete", s.deleteUser)
	return logRequests(logger, mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https:; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		if r.URL.Path == "/webdav" || strings.HasPrefix(r.URL.Path, "/webdav/") {
			s.serveWebDAV(w, r)
			return
		}
		public := r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/assets/")
		if !public {
			user, sid, err := s.auth.authenticate(r)
			if err != nil {
				w.Header().Set("HX-Redirect", "/login")
				if r.Method == "GET" && r.URL.Path == "/" && strings.Contains(r.Header.Get("Accept"), "text/html") && r.Header.Get("HX-Request") == "" {
					http.Redirect(w, r, "/login", http.StatusSeeOther)
				} else {
					http.Error(w, "authentication required", http.StatusUnauthorized)
				}
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, authenticatedUser{User: user, SessionID: sid}))
			if expected := r.Header.Get("X-TrackHarbor-User"); expected != "" && expected != fmt.Sprint(user.ID) {
				w.Header().Set("HX-Redirect", "/login")
				http.Error(w, "Account changed. Reload before submitting.", http.StatusConflict)
				return
			}
		}
		if r.Method == http.MethodPost {
			origin := r.Header.Get("Origin")
			u, err := url.Parse(origin)
			if origin == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			if !public {
				expected := r.Header.Get("X-TrackHarbor-User")
				if expected == "" {
					expected = r.URL.Query().Get("account_id")
				}
				if expected != fmt.Sprint(currentUser(r).ID) {
					w.Header().Set("HX-Redirect", "/login")
					http.Error(w, "Account changed. Reload before submitting.", http.StatusConflict)
					return
				}
			}
		}
		if !public && r.URL.Path != "/logout" {
			if err := s.auth.renew(w, r); err != nil {
				http.Error(w, "could not renew session", http.StatusInternalServerError)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
}

type playableTrack struct {
	ID       int64  `json:"id"`
	Source   string `json:"source"`
	SourceID string `json:"source_id"`
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Cover    string `json:"cover"`
}

func playableItems(tracks []store.Track) []playableTrack {
	items := make([]playableTrack, 0, len(tracks))
	for _, t := range tracks {
		items = append(items, playableTrack{t.ID, t.Source, t.SourceID, t.Artist, t.Title, coverURL(t.CoverURL)})
	}
	return items
}

func (s *Server) playableTracks(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.NotFound(w, r)
		return
	}
	tracks, err := s.store.ListPlayable(currentUser(r).ID)
	if err != nil {
		http.Error(w, "unable to read saved tracks", http.StatusInternalServerError)
		return
	}
	items := playableItems(tracks)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(items); err != nil {
		s.logger.Error("write saved tracks failed")
	}
}

func sourceName(r *http.Request) string {
	if r.FormValue("source") == "youtube" {
		return "youtube"
	}
	if r.FormValue("source") == "vk" {
		return "vk"
	}
	if r.FormValue("source") == "spotify" {
		return "spotify"
	}
	if r.FormValue("source") == "downloads" {
		return "downloads"
	}
	if r.FormValue("source") == "settings" {
		return "settings"
	}
	if r.FormValue("source") == "all" {
		return "all"
	}
	return "music"
}

func selectedSource(r *http.Request) (string, bool) {
	source := r.PostForm.Get("source")
	return source, source == "spotify" || source == "vk"
}

type pageData struct {
	Library                                   store.LibrarySummary
	LibrarySync                               string
	User                                      store.User
	Users                                     []store.User
	BootstrapWarning                          bool
	Source, Name, Status, PingStatus, AuthURL string
	Connected                                 bool
	Tracks                                    []store.Track
	ProxySources                              string
	PersonalProxies                           []proxyView
	PublicProxyCount                          int
	ProxyImport                               proxyImportState
	SourcesSaved                              bool
	DownloadRetries                           int
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	source := sourceName(r)
	userID := currentUser(r).ID
	data := pageData{Source: source, Name: "Tracks", User: currentUser(r)}
	summary, summaryErr := s.store.LibrarySummary(userID)
	if summaryErr != nil {
		http.Error(w, "unable to read library summary", http.StatusInternalServerError)
		return
	}
	data.Library = summary
	data.LibrarySync = s.librarySyncStatus()
	users, userErr := s.store.Users()
	if userErr != nil {
		http.Error(w, "unable to read users", 500)
		return
	}
	for _, u := range users {
		if u.Username == "admin" {
			data.BootstrapWarning = true
		}
	}
	var err error
	if source == "downloads" {
		data.Name = "Downloads"
	} else if source == "all" {
		data.Name = "Server tracks"
		data.Tracks, err = s.store.ListAll()
	} else if source == "settings" {
		data.Users = users
		data.Name = "Settings"
		data.DownloadRetries, err = s.store.DownloadRetries()
		if err != nil {
			http.Error(w, "unable to read download attempts setting", http.StatusInternalServerError)
			return
		}
		proxySources, settingsErr := s.store.Setting("proxy_sources")
		if settingsErr != nil {
			http.Error(w, "unable to read download settings", http.StatusInternalServerError)
			return
		}
		data.ProxySources = proxySources
		data.PublicProxyCount, err = s.store.ProxyCount("public")
		if err != nil {
			http.Error(w, "unable to count public proxies", http.StatusInternalServerError)
			return
		}
		s.importMu.Lock()
		data.ProxyImport = s.proxyImport
		s.importMu.Unlock()
		proxies, proxyErr := s.store.Proxies("personal")
		if proxyErr != nil {
			http.Error(w, "unable to read personal proxies", http.StatusInternalServerError)
			return
		}
		for _, proxy := range proxies {
			parsed, err := url.Parse(proxy.Address)
			if err != nil {
				continue
			}
			view := proxyView{ID: proxy.ID, Host: parsed.Host, Masked: maskPersonalProxy(proxy.Address, parsed)}
			if proxy.LastCheckedAt.Unix() > 0 {
				view.CheckedAt = proxy.LastCheckedAt.Format("02 Jan 2006, 15:04 UTC")
			}
			data.PersonalProxies = append(data.PersonalProxies, view)
		}
		data.SourcesSaved = r.URL.Query().Get("sources_saved") == "1"
	} else if source == "music" {
		data.Tracks, err = s.store.ListUser(userID)
	} else if source == "youtube" {
		data.Name = "YouTube"
		data.Tracks, err = s.store.List(userID, "youtube")
	} else {
		data.Connected, err = s.store.Connected(userID, source)
		if err == nil {
			data.Tracks, err = s.store.List(userID, source)
		}
		if source == "vk" {
			data.Name = "VK Music"
		} else {
			data.Name = "Spotify"
		}
		s.mu.Lock()
		data.Status = s.status[userSourceKey(r, source)]
		data.PingStatus = s.pingStatus[userSourceKey(r, source)]
		s.mu.Unlock()
		data.AuthURL = vk.AuthorizationURL()
	}
	if err != nil {
		http.Error(w, "unable to read library", 500)
		return
	}
	for i := range data.Tracks {
		if data.Tracks[i].FilePath == "" {
			continue
		}
		info, err := os.Stat(data.Tracks[i].FilePath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			data.Tracks[i].FilePath = ""
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		s.logger.Error("render web page failed")
	}
}

// Return an aggregate state without exposing another user's source details.
func (s *Server) librarySyncStatus() string {
	s.mu.Lock()
	status := "Ready"
	syncing := false
	for _, message := range s.status {
		if message == "Syncing…" {
			syncing = true
		}
		if strings.HasPrefix(message, "Sync failed.") || strings.HasPrefix(message, "Sync timed out.") || strings.HasPrefix(message, "Library synced, but") {
			status = "Attention"
		}
	}
	s.mu.Unlock()
	if syncing {
		return "Syncing"
	}
	if s.downloads != nil {
		s.downloads.mu.Lock()
		running, stopped, failed := s.downloads.state.Running, s.downloads.stopped, s.downloads.failed
		s.downloads.mu.Unlock()
		if running {
			if stopped {
				return "Stopping"
			}
			return "Downloading"
		}
		if failed > 0 {
			status = "Attention"
		}
		if stopped && status == "Ready" {
			return "Stopped"
		}
	}
	return status
}

func (s *Server) currentStatus(w http.ResponseWriter, r *http.Request) {
	source := sourceName(r)
	s.mu.Lock()
	status := s.status[userSourceKey(r, source)]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status == "" {
		status = "Ready to sync."
	}
	template.HTMLEscape(w, []byte(status))
}

func (s *Server) currentPingStatus(w http.ResponseWriter, r *http.Request) {
	source := sourceName(r)
	s.mu.Lock()
	status := s.pingStatus[userSourceKey(r, source)]
	s.mu.Unlock()
	if status == "" {
		status = "Not checked yet."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	template.HTMLEscape(w, []byte(status))
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	source, ok := selectedSource(r)
	if !ok {
		http.Error(w, "unknown source", 400)
		return
	}
	key := userSourceKey(r, source)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[key] == "Syncing…" {
		http.Error(w, "sync in progress", http.StatusConflict)
		return
	}
	var credentials any
	if source == "spotify" {
		authorization, clientToken := spotifyHeaders(r.PostForm.Get("headers"))
		if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) < len("Bearer ")+1 || clientToken == "" {
			http.Error(w, "paste request headers containing authorization: Bearer … and client-token", 400)
			return
		}
		credentials = spotify.Credentials{Authorization: authorization, ClientToken: clientToken}
	} else {
		token, err := vk.ParseToken(r.PostForm.Get("token"))
		if err != nil {
			http.Error(w, "paste a valid VK token or redirect URL", 400)
			return
		}
		credentials = vk.Credentials{Token: token}
	}
	if err := s.store.SaveCredentials(currentUser(r).ID, source, credentials); err != nil {
		http.Error(w, "could not save credentials", 500)
		return
	}
	s.status[key] = "Credentials saved. Click Sync library to import tracks."
	s.pingStatus[key] = "Not checked yet."
	s.logger.Info("source credentials saved", zap.String("source", source))
	http.Redirect(w, r, "/?source="+source, http.StatusSeeOther)
}

func (s *Server) pingSource(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	source, ok := selectedSource(r)
	if !ok {
		http.Error(w, "unknown source", 400)
		return
	}
	key := userSourceKey(r, source)
	var spotifyCredentials spotify.Credentials
	var vkCredentials vk.Credentials
	var err error
	if source == "spotify" {
		err = s.store.Credentials(currentUser(r).ID, source, &spotifyCredentials)
	} else {
		err = s.store.Credentials(currentUser(r).ID, source, &vkCredentials)
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "connect source first", 400)
		return
	}
	if err != nil {
		http.Error(w, "unable to read credentials; reconnect this source", 500)
		return
	}
	s.mu.Lock()
	if s.pingStatus[key] == "Checking…" {
		s.mu.Unlock()
		if r.Header.Get("Accept") == "application/json" {
			w.WriteHeader(http.StatusAccepted)
		} else {
			http.Redirect(w, r, "/?source="+source, http.StatusSeeOther)
		}
		return
	}
	s.pingStatus[key] = "Checking…"
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var pingErr error
		if source == "spotify" {
			pingErr = spotify.Ping(ctx, spotifyCredentials.Authorization, spotifyCredentials.ClientToken)
		} else {
			client := vk.NewClient(vkCredentials.Token)
			defer client.Close()
			pingErr = client.Ping(ctx)
		}
		message := "Connection verified."
		switch {
		case errors.Is(pingErr, spotify.ErrUnauthorized), errors.Is(pingErr, vk.ErrUnauthorized):
			message = "Token rejected. Reconnect this source with fresh credentials."
		case pingErr != nil:
			message = "Could not verify connection. Try again later; the token may still be valid."
		}
		s.mu.Lock()
		if s.pingStatus[key] == "Checking…" {
			s.pingStatus[key] = message
		}
		s.mu.Unlock()
		result := "verified"
		if errors.Is(pingErr, spotify.ErrUnauthorized) || errors.Is(pingErr, vk.ErrUnauthorized) {
			result = "rejected"
		} else if pingErr != nil {
			result = "unavailable"
		}
		s.logger.Info("source connection checked", zap.String("source", source), zap.String("result", result))
	}()
	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusAccepted)
	} else {
		http.Redirect(w, r, "/?source="+source, http.StatusSeeOther)
	}
}

// spotifyHeaders extracts only the two credentials needed for sync from a
// DevTools header dump. Other headers (including cookies) are never persisted.
func spotifyHeaders(input string) (authorization, clientToken string) {
	for line := range strings.SplitSeq(input, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization":
			authorization = value
		case "client-token":
			clientToken = value
		}
	}
	return authorization, clientToken
}

func (s *Server) syncSource(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	source, ok := selectedSource(r)
	if !ok {
		http.Error(w, "unknown source", 400)
		return
	}
	userID := currentUser(r).ID
	key := userSourceKey(r, source)
	connected, err := s.store.Connected(userID, source)
	if err != nil {
		http.Error(w, "unable to read credentials", 500)
		return
	}
	if !connected {
		http.Error(w, "connect source first", 400)
		return
	}
	s.mu.Lock()
	if s.status[key] == "Syncing…" {
		s.mu.Unlock()
		http.Redirect(w, r, "/?source="+source, http.StatusSeeOther)
		return
	}
	s.status[key] = "Syncing…"
	s.mu.Unlock()
	s.logger.Info("library sync started", zap.String("source", source))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		var count int
		var err error
		if source == "spotify" {
			count, err = library.SyncSpotify(ctx, s.store, userID)
		} else {
			count, err = library.SyncVK(ctx, s.store, userID)
		}
		message := fmt.Sprintf("Synced %d tracks. Checking downloads in the background.", count)
		if err != nil {
			// Provider errors may include remote URLs or credentials: never echo them.
			message = "Sync failed. Previous tracks were kept. Check credentials and try again."
			if errors.Is(err, context.DeadlineExceeded) {
				message = "Sync timed out. Previous tracks were kept."
			}
			s.logger.Error("library sync failed", zap.String("source", source), zap.Bool("timed_out", errors.Is(err, context.DeadlineExceeded)))
		} else {
			s.logger.Info("library sync finished", zap.String("source", source), zap.Int("tracks", count))
			if s.downloads != nil {
				tracks, listErr := s.store.List(userID, source)
				if listErr != nil {
					message = "Library synced, but downloads could not be queued. Try syncing again."
					s.logger.Error("queue synced tracks failed", zap.String("source", source))
				} else {
					s.downloads.enqueue(tracks)
					s.downloads.mu.Lock()
					if s.downloads.stopped {
						message = fmt.Sprintf("Synced %d tracks. Downloads are stopped; start them from Downloads.", count)
					}
					s.downloads.mu.Unlock()
				}
			}
		}
		s.mu.Lock()
		s.status[key] = message
		s.mu.Unlock()
	}()
	http.Redirect(w, r, "/?source="+source, http.StatusSeeOther)
}
