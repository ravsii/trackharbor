package web

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/ravsii/trackharbor/internal/store"
)

const webDAVMethods = "OPTIONS, GET, HEAD, PROPFIND"

func (s *Server) serveWebDAV(w http.ResponseWriter, r *http.Request) {
	challenge := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="TrackHarbor", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	if len(r.Header.Get("Authorization")) > 4096 {
		challenge()
		return
	}
	username, password, ok := r.BasicAuth()
	if !ok || len(username) > 64 || len(password) > 72 {
		challenge()
		return
	}
	// Count failed credentials, not successful file requests: a library sync
	// must not hit the browser login's twenty-attempt limit.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	select {
	case s.auth.workers <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	s.auth.mu.Lock()
	attempts := s.auth.attempts[host]
	blocked := time.Since(attempts.start) < time.Minute && attempts.count >= 20
	s.auth.mu.Unlock()
	if blocked {
		<-s.auth.workers
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return
	}
	user, err := s.store.Authenticate(username, password)
	if errors.Is(err, store.ErrAuthentication) {
		s.auth.allowLogin(r.RemoteAddr)
	}
	<-s.auth.workers
	if errors.Is(err, store.ErrAuthentication) {
		challenge()
		return
	}
	if err != nil {
		s.logger.Error("WebDAV authentication unavailable")
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Allow", webDAVMethods)
	w.Header().Set("DAV", "1")
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("MS-Author-Via", "DAV")
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet, http.MethodHead, "PROPFIND":
	default:
		http.Error(w, "music library is read-only", http.StatusMethodNotAllowed)
		return
	}
	root, err := os.OpenRoot(s.downloads.config.DownloadDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Error("unable to open WebDAV music directory")
		http.Error(w, "music directory unavailable", http.StatusInternalServerError)
		return
	}
	if root != nil {
		defer root.Close()
	}
	fs := &webDAVFS{store: s.store, userID: user.ID, root: root}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if r.URL.Path == "/webdav" {
			http.Redirect(w, r, "/webdav/", http.StatusMovedPermanently)
			return
		}
		if r.URL.Path == "/webdav/" {
			// The standard directory listing also makes the same flat library
			// usable in a browser, without exposing the physical download tree.
			http.StripPrefix("/webdav", http.FileServer(webDAVHTTPFS{fs, r.Context()})).ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
			"filename": strings.TrimPrefix(r.URL.Path, "/webdav/"),
		}))
	}
	if r.Method == "PROPFIND" {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if depth := r.Header.Get("Depth"); depth == "" || depth == "infinity" {
			// Our collection has exactly one level; infinity is equivalent to 1.
			r = r.Clone(r.Context())
			r.Header.Set("Depth", "1")
		}
	}
	handler := webdav.Handler{Prefix: "/webdav", FileSystem: fs, LockSystem: webdav.NewMemLS()}
	handler.ServeHTTP(w, r)
}

// A per-request filesystem snapshot keeps PROPFIND linear in library size.
// Individual file requests resolve one name with a current membership check.
type webDAVFS struct {
	store  *store.Store
	userID int64
	root   *os.Root
	tracks map[string]store.ExportTrack
	infos  []os.FileInfo
}

func (fs *webDAVFS) list(ctx context.Context) error {
	if fs.tracks != nil {
		return nil
	}
	tracks, err := fs.store.ExportTracks(ctx, fs.userID)
	if err != nil {
		return err
	}
	fs.tracks = make(map[string]store.ExportTrack, len(tracks))
	for _, track := range tracks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fs.root == nil {
			continue
		}
		info, err := fs.root.Stat(track.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		fs.tracks[track.Name] = track
		fs.infos = append(fs.infos, webDAVInfo{FileInfo: info, name: track.Name})
	}
	sort.Slice(fs.infos, func(i, j int) bool { return fs.infos[i].Name() < fs.infos[j].Name() })
	return nil
}

func (fs *webDAVFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag != os.O_RDONLY {
		return nil, os.ErrPermission
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		if err := fs.list(ctx); err != nil {
			return nil, err
		}
		return &webDAVDirectory{entries: fs.infos}, nil
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) || fs.root == nil {
		return nil, os.ErrNotExist
	}
	var track store.ExportTrack
	if fs.tracks != nil {
		var ok bool
		track, ok = fs.tracks[name]
		if !ok {
			return nil, os.ErrNotExist
		}
	} else {
		var err error
		track, err = fs.store.ExportTrack(ctx, fs.userID, name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
	}
	f, err := fs.root.Open(track.Path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		f.Close()
		return nil, os.ErrNotExist
	}
	return &webDAVFile{File: f, name: name}, nil
}

func (fs *webDAVFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	f, err := fs.OpenFile(ctx, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func (*webDAVFS) Mkdir(context.Context, string, os.FileMode) error { return os.ErrPermission }
func (*webDAVFS) RemoveAll(context.Context, string) error          { return os.ErrPermission }
func (*webDAVFS) Rename(context.Context, string, string) error     { return os.ErrPermission }

type webDAVHTTPFS struct {
	fs  *webDAVFS
	ctx context.Context
}

func (fs webDAVHTTPFS) Open(name string) (http.File, error) {
	return fs.fs.OpenFile(fs.ctx, name, os.O_RDONLY, 0)
}

type webDAVFile struct {
	*os.File
	name string
}

func (f *webDAVFile) Stat() (os.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return webDAVInfo{FileInfo: info, name: f.name}, nil
}

func (*webDAVFile) Write([]byte) (int, error) { return 0, os.ErrPermission }

type webDAVInfo struct {
	os.FileInfo
	name string
}

func (f webDAVInfo) Name() string                              { return f.name }
func (webDAVInfo) ContentType(context.Context) (string, error) { return "audio/mpeg", nil }

type webDAVDirectory struct {
	entries  []os.FileInfo
	position int
}

func (d *webDAVDirectory) Readdir(count int) ([]os.FileInfo, error) {
	if count > 0 && d.position >= len(d.entries) {
		return nil, io.EOF
	}
	end := len(d.entries)
	if count > 0 && count < end-d.position {
		end = d.position + count
	}
	entries := d.entries[d.position:end]
	d.position = end
	return entries, nil
}

func (d *webDAVDirectory) Stat() (os.FileInfo, error)   { return d, nil }
func (*webDAVDirectory) Close() error                   { return nil }
func (*webDAVDirectory) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (*webDAVDirectory) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }
func (*webDAVDirectory) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (*webDAVDirectory) Name() string                   { return "" }
func (*webDAVDirectory) Size() int64                    { return 0 }
func (*webDAVDirectory) Mode() os.FileMode              { return os.ModeDir | 0555 }
func (*webDAVDirectory) ModTime() time.Time             { return time.Unix(0, 0) }
func (*webDAVDirectory) IsDir() bool                    { return true }
func (*webDAVDirectory) Sys() any                       { return nil }
