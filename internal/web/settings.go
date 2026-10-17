package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type proxyView struct {
	Host, Masked string
	CheckedAt    string
	ID           int64
}

func (s *Server) pingPersonalProxy(w http.ResponseWriter, r *http.Request) {
	id, err := proxyID(r)
	if err != nil {
		http.Error(w, "invalid proxy ID", http.StatusBadRequest)
		return
	}
	proxy, err := s.store.Proxy(id)
	if err == sql.ErrNoRows || (err == nil && proxy.Type != "personal") {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "unable to read personal proxy", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	if s.proxyPings[id] {
		s.mu.Unlock()
		http.Error(w, "this proxy is already being checked; try again shortly", http.StatusConflict)
		return
	}
	if s.proxyPings == nil {
		s.proxyPings = make(map[int64]bool)
	}
	s.proxyPings[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.proxyPings, id)
		s.mu.Unlock()
	}()

	reachable := probePersonalProxy(r.Context(), proxy.Address) == nil
	if r.Context().Err() != nil {
		return
	}
	checkedAt := time.Now()
	if err = s.store.RecordProxyCheck(proxy.Address, checkedAt, reachable); err != nil {
		http.Error(w, "unable to save proxy check result", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(struct {
		Reachable bool  `json:"reachable"`
		CheckedAt int64 `json:"checked_at"`
	}{reachable, checkedAt.Unix()}); err != nil {
		s.logger.Error("write personal proxy check result failed")
	}
}

func maskPersonalProxy(address string, parsed *url.URL) string {
	if parsed.User == nil {
		return address
	}
	start := strings.Index(address, "://")
	if start < 0 {
		return address
	}
	authority := address[start+3:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return address
	}
	masked := "********"
	if strings.Contains(authority[:at], ":") {
		masked += ":********"
	}
	return address[:start+3] + masked + address[start+3+at:]
}

func proxyID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id <= 0 {
		return 0, fmt.Errorf("invalid proxy ID")
	}
	return id, err
}

func (s *Server) refreshPersonalRoutes(settings []string) {
	s.downloads.mu.Lock()
	s.downloads.config.PersonalProxy = append([]string(nil), settings...)
	s.downloads.mu.Unlock()
}

func (s *Server) addPersonalProxy(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid proxy form", http.StatusBadRequest)
		return
	}
	address := strings.TrimSpace(r.PostForm.Get("address"))
	if proxies, err := settingsProxies(address); err != nil || len(proxies) != 1 {
		http.Error(w, "enter one valid proxy URL", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Port() == "" {
		http.Error(w, "enter a proxy URL with a host and port", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "enter a valid proxy port", http.StatusBadRequest)
		return
	}
	probeErr := probePersonalProxy(r.Context(), address)
	checkedAt := time.Now()
	if err := s.store.RecordProxyCheck(address, checkedAt, probeErr == nil); err != nil {
		http.Error(w, "unable to save proxy check result", http.StatusInternalServerError)
		return
	}
	if probeErr != nil {
		http.Error(w, "proxy could not reach YouTube; check its address, credentials and HTTPS access", http.StatusBadRequest)
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	count, err := s.store.ProxyCount("personal")
	if err != nil {
		http.Error(w, "unable to read personal proxies", http.StatusInternalServerError)
		return
	}
	if count >= 100 {
		http.Error(w, "use no more than 100 personal proxies", http.StatusBadRequest)
		return
	}
	added, err := s.store.AddProxy(address, checkedAt)
	if err != nil {
		http.Error(w, "unable to save personal proxy", http.StatusInternalServerError)
		return
	}
	if !added {
		http.Error(w, "proxy already saved", http.StatusBadRequest)
		return
	}
	settings, err := s.store.DownloadSettings()
	if err != nil {
		http.Error(w, "proxy saved, but unable to refresh download settings", http.StatusInternalServerError)
		return
	}
	s.refreshPersonalRoutes(settings.PersonalProxy)
	s.logger.Info("personal proxy added")
	settingsResponse(w, r)
}

func (s *Server) deletePersonalProxy(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("confirm") != "yes" {
		http.Error(w, "confirm proxy removal before submitting", http.StatusBadRequest)
		return
	}
	id, err := proxyID(r)
	if err != nil {
		http.Error(w, "invalid proxy ID", http.StatusBadRequest)
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	proxy, err := s.store.Proxy(id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "unable to read personal proxy", http.StatusInternalServerError)
		return
	}
	if proxy.Type != "personal" {
		http.NotFound(w, r)
		return
	}
	deleted, err := s.store.DeleteProxy(id)
	if err != nil {
		http.Error(w, "unable to remove personal proxy", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "proxy not found", http.StatusNotFound)
		return
	}
	settings, err := s.store.DownloadSettings()
	if err != nil {
		http.Error(w, "proxy removed, but unable to refresh download settings", http.StatusInternalServerError)
		return
	}
	s.refreshPersonalRoutes(settings.PersonalProxy)
	s.logger.Info("personal proxy removed")
	settingsResponse(w, r)
}

func (s *Server) personalProxyAddress(w http.ResponseWriter, r *http.Request) {
	id, err := proxyID(r)
	if err != nil {
		http.Error(w, "invalid proxy ID", http.StatusBadRequest)
		return
	}
	proxy, err := s.store.Proxy(id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "unable to read personal proxy", http.StatusInternalServerError)
		return
	}
	if proxy.Type != "personal" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Address string `json:"address"`
	}{Address: proxy.Address}); err != nil {
		s.logger.Error("write personal proxy address failed")
	}
}

func settingsResponse(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusNoContent)
	} else {
		http.Redirect(w, r, "/?source=settings", http.StatusSeeOther)
	}
}

func settingsLines(raw string) []string {
	var lines []string
	for line := range strings.SplitSeq(raw, "\n") {
		if value := strings.TrimSpace(line); value != "" {
			lines = append(lines, value)
		}
	}
	return lines
}

func settingsProxies(raw string) ([]string, error) {
	proxies := settingsLines(raw)
	if len(proxies) > 100 {
		return nil, fmt.Errorf("use no more than 100 proxy addresses")
	}
	for _, address := range proxies {
		parsed, err := url.Parse(address)
		if len(address) > 2048 || err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("proxy addresses must be http(s) or SOCKS URLs with a host (one per line)")
		}
		switch parsed.Scheme {
		case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("proxy addresses must be http(s) or SOCKS URLs with a host (one per line)")
		}
	}
	return proxies, nil
}

func (s *Server) saveDownloadSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid download settings form", http.StatusBadRequest)
		return
	}
	retries, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("download_retries")))
	if err != nil || retries < 1 {
		http.Error(w, "enter a whole number of at least 1", http.StatusBadRequest)
		return
	}
	if err = s.store.SetSetting("download_retries", strconv.Itoa(retries)); err != nil {
		http.Error(w, "unable to save download settings; try again", http.StatusInternalServerError)
		return
	}
	s.logger.Info("download settings saved")
	settingsResponse(w, r)
}

func (s *Server) saveProxySources(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid settings form", http.StatusBadRequest)
		return
	}
	sources := settingsLines(r.PostForm.Get("proxy_sources"))
	if len(sources) > 100 {
		http.Error(w, "use no more than 100 source links", http.StatusBadRequest)
		return
	}
	for _, source := range sources {
		if err := validateProxySource(source); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := s.store.SetSetting("proxy_sources", strings.Join(sources, "\n")); err != nil {
		http.Error(w, "unable to save proxy sources", http.StatusInternalServerError)
		return
	}
	s.logger.Info("proxy sources saved")
	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusNoContent)
	} else {
		http.Redirect(w, r, "/?source=settings&sources_saved=1", http.StatusSeeOther)
	}
}
