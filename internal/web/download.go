package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/ravsii/trackharbor/download"
	"github.com/ravsii/trackharbor/internal/config"
	"github.com/ravsii/trackharbor/internal/store"
)

type downloadState struct {
	ID         int64  `json:"id,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	Running    bool   `json:"running"`
	Stopped    bool   `json:"stopped"`
	Total      int    `json:"total"`
	Finished   int    `json:"finished"`
	Message    string `json:"message"`
	Source     string `json:"source,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Title      string `json:"title,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
}

type downloadJob struct {
	store     *store.Store
	config    config.Config
	logger    *zap.Logger
	mu        sync.Mutex
	state     downloadState
	listeners map[chan []byte]struct{}
	queue     []store.Track
	active    map[string]bool
	tracks    map[string]downloadState
	finished  int
	failed    int
	stopped   bool
	cancel    context.CancelFunc
}

func newDownloadJob(db *store.Store, cfg config.Config, logger *zap.Logger) *downloadJob {
	return &downloadJob{store: db, config: cfg, logger: logger, listeners: make(map[chan []byte]struct{}), active: make(map[string]bool), tracks: make(map[string]downloadState), state: downloadState{Message: "Waiting for a library sync."}}
}

func (d *downloadJob) publish(update downloadState) {
	d.mu.Lock()
	d.publishLocked(update)
	d.mu.Unlock()
}

func (d *downloadJob) publishLocked(update downloadState) {
	update.Stopped = d.stopped
	if update.Running && update.Total < d.state.Total {
		update.Total = d.state.Total
	}
	if update.Running && update.Finished < d.state.Finished {
		update.Finished = d.state.Finished
	}
	d.state = update
	if update.Title != "" {
		d.tracks[trackKey(update.Source, update.SourceID)] = update
	}
	if update.Stage != "done" {
		return
	}
	data, _ := json.Marshal(struct {
		ID       int64  `json:"id"`
		SourceID string `json:"source_id"`
		Source   string `json:"source"`
		Artist   string `json:"artist"`
		Title    string `json:"title"`
	}{update.ID, update.SourceID, update.Source, update.Artist, update.Title})
	for ch := range d.listeners {
		select {
		case ch <- data:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- data:
			default:
			}
		}
	}
}

func trackKey(source, sourceID string) string {
	return source + "\x00" + sourceID
}

func (d *downloadJob) enqueue(tracks []store.Track) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	start := !d.state.Running
	var ctx context.Context
	if start {
		d.finished, d.failed = 0, 0
		d.state = downloadState{Message: "Checking downloaded files."}
		ctx, d.cancel = context.WithCancel(context.Background())
	}
	added := 0
	for _, t := range tracks {
		if t.DownloadFailed {
			continue
		}
		if t.FilePath != "" && download.FileExists(t.FilePath) {
			continue
		}
		key := trackKey(t.Source, t.SourceID)
		if d.active[key] {
			continue
		}
		d.active[key] = true
		d.queue = append(d.queue, t)
		d.tracks[key] = downloadState{ID: t.ID, SourceID: t.SourceID, Source: t.Source, Artist: t.Artist, Title: t.Title, Stage: "queued"}
		added++
	}
	if added == 0 && !d.state.Running {
		d.cancel()
		d.cancel = nil
		d.publishLocked(downloadState{Message: "No tracks to download."})
		d.mu.Unlock()
		return
	}
	d.publishLocked(downloadState{Running: true, Total: d.state.Total + added, Finished: d.finished, Stage: "queued", Message: "Checking downloaded files."})
	d.mu.Unlock()
	if start {
		d.logger.Info("download check started", zap.Int("tracks", added))
		go d.runQueue(ctx)
	}
}

func (d *downloadJob) runQueue(ctx context.Context) {
	for {
		d.mu.Lock()
		if d.stopped {
			for _, t := range d.queue {
				key := trackKey(t.Source, t.SourceID)
				d.tracks[key] = downloadState{ID: t.ID, SourceID: t.SourceID, Source: t.Source, Artist: t.Artist, Title: t.Title, Stage: "stopped", Message: "Stopped"}
			}
			d.queue = nil
			clear(d.active)
			d.cancel = nil
			d.publishLocked(downloadState{Total: d.state.Total, Finished: d.finished, Stage: "stopped", Message: "Downloads stopped."})
			d.mu.Unlock()
			d.logger.Info("downloads stopped")
			return
		}
		if len(d.queue) == 0 {
			d.cancel()
			d.cancel = nil
			message := fmt.Sprintf("Checked %d tracks; %d failed.", d.finished, d.failed)
			d.publishLocked(downloadState{Total: d.finished, Finished: d.finished, Stage: "finished", Message: message})
			failed, total := d.failed, d.finished
			d.mu.Unlock()
			if failed > 0 {
				d.logger.Warn("download check finished with failures", zap.Int("total", total), zap.Int("failed", failed))
			} else {
				d.logger.Info("download check finished", zap.Int("total", total))
			}
			return
		}
		tracks := d.queue
		d.queue = nil
		d.mu.Unlock()
		d.runBatch(ctx, tracks)
	}
}

func (d *downloadJob) runBatch(ctx context.Context, tracks []store.Track) {
	retryLimit, settingsErr := d.store.DownloadRetries()
	if settingsErr != nil {
		d.logger.Error("unable to read download_retries setting; downloads will not start")
	}
	d.mu.Lock()
	routes := &download.Routes{
		Personal: append([]string(nil), d.config.PersonalProxy...), Public: append([]string(nil), d.config.PublicProxy...),
	}
	d.mu.Unlock()
	process := func(t store.Track) {
		state := downloadState{Running: true}
		state.ID, state.SourceID = t.ID, t.SourceID
		// A different user's sync may have queued an older snapshot. Read the
		// latest source URL and check the deterministic file path again.
		latest, trackErr := d.store.Track(t.ID)
		if trackErr == nil {
			t = latest
		}
		state.Source, state.Artist, state.Title = t.Source, t.Artist, t.Title
		if ctx.Err() != nil {
			state.Stage, state.Message = "stopped", "Stopped"
			d.mu.Lock()
			d.publishLocked(state)
			delete(d.active, trackKey(t.Source, t.SourceID))
			d.mu.Unlock()
			return
		}
		state.Bytes, state.TotalBytes = 0, 0
		state.Stage = "checking"
		state.Message = "Checking saved file"
		d.publish(state)
		valid, err := d.saved(t)
		if ctx.Err() != nil {
			state.Stage, state.Message = "stopped", "Stopped"
		} else if trackErr != nil {
			state.Stage, state.Message = "failed", "Could not read saved download state"
			d.logger.Error("unable to read track download state", zap.Int64("track_id", t.ID))
		} else if err != nil {
			state.Stage, state.Message = "failed", "Could not inspect saved file; check file permissions"
		} else if valid {
			state.Stage = "skipped"
			state.Message = "Already downloaded"
		} else if t.DownloadFailed {
			state.Stage, state.Message = "failed", "Download retry limit reached"
		} else if settingsErr != nil {
			state.Stage, state.Message = "failed", "Could not read download_retries setting; expected a positive integer"
		} else if t.DownloadAttempts >= retryLimit {
			state.Stage, state.Message = "failed", "Download retry limit reached"
			if err := d.store.SetDownloadFailed(t.ID); err != nil {
				d.logger.Error("unable to save failed download state", zap.Int64("track_id", t.ID))
			}
		} else {
			fields := []zap.Field{zap.String("source", t.Source), zap.String("artist", t.Artist), zap.String("title", t.Title)}
			d.logger.Info("track download started", fields...)
			event := func(e download.Event) {
				state.Stage, state.Bytes, state.TotalBytes = e.Stage, e.Bytes, e.Total
				state.Message = "Downloading " + t.Source
				d.publish(state)
			}
			candidate := download.Track{Source: t.Source, SourceID: t.SourceID, Artist: t.Artist, Title: t.Title, AudioURL: t.AudioURL, CoverURL: t.CoverURL}
			var saveErr error
			for attempt := t.DownloadAttempts; attempt < retryLimit; attempt++ {
				if saveErr = ctx.Err(); saveErr != nil {
					break
				}
				sourceURL, resolveErr := download.Resolve(ctx, candidate, routes, event)
				saveErr = resolveErr
				if saveErr == nil {
					saveErr = d.store.SetSourceURL(t.ID, sourceURL)
					if saveErr != nil {
						d.logger.Error("unable to save resolved download URL", zap.Int64("track_id", t.ID))
						break
					}
					candidate.AudioURL = sourceURL
					_, saveErr = download.Save(ctx, candidate, d.config.DownloadDir, routes, event)
				}
				if saveErr == nil || ctx.Err() != nil {
					break
				}
				// Only completed, unsuccessful cycles consume the persisted budget.
				if err := d.store.RecordDownloadFailure(t.ID, retryLimit); err != nil {
					d.logger.Error("unable to save download attempt", zap.Int64("track_id", t.ID))
					break
				}
				if attempt+1 < retryLimit {
					state.Stage, state.Message = "retrying", "Retrying download"
					state.Bytes, state.TotalBytes = 0, 0
					d.publish(state)
					timer := time.NewTimer(time.Second << min(attempt, 4))
					select {
					case <-ctx.Done():
						timer.Stop()
					case <-timer.C:
					}
				}
			}
			if ctx.Err() != nil && saveErr != nil {
				state.Stage = "stopped"
				state.Message = "Stopped"
			} else if saveErr != nil {
				state.Stage = "failed"
				state.Message = "YouTube download failed: check yt-dlp, FFmpeg and server proxy settings."
				if t.Source == "vk" {
					state.Message = "VK download failed: refresh VK sync to renew audio URLs; check FFmpeg."
				}
				// Signed VK URLs and proxy passwords must never reach logs or browser.
			} else {
				state.Stage = "done"
				state.Message = "Saved MP3"
			}
			switch state.Stage {
			case "done":
				d.logger.Info("track download finished", fields...)
			case "stopped":
				d.logger.Info("track download stopped", fields...)
			case "failed":
				d.logger.Warn("track download failed", fields...)
			}
		}
		d.mu.Lock()
		if state.Stage == "failed" {
			d.failed++
		}
		if state.Stage != "stopped" {
			d.finished++
		}
		state.Finished = d.finished
		state.Total = d.state.Total
		d.publishLocked(state)
		delete(d.active, trackKey(t.Source, t.SourceID))
		d.mu.Unlock()
	}
	var wg sync.WaitGroup
	for _, source := range []string{"vk", "spotify", "youtube"} {
		queue := make(chan store.Track)
		workers := 10
		if source == "spotify" {
			workers = 50
		}
		for range workers {
			wg.Go(func() {
				for t := range queue {
					process(t)
				}
			})
		}
		go func(source string, jobs chan store.Track) {
			defer close(jobs)
			for _, t := range tracks {
				if t.Source == source {
					jobs <- t
				}
			}
		}(source, queue)
	}
	wg.Wait()
}

func (s *Server) startDownloads(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	tracks, err := s.store.Downloadable()
	if err != nil {
		http.Error(w, "unable to read library", http.StatusInternalServerError)
		return
	}
	d := s.downloads
	d.mu.Lock()
	if d.state.Running {
		d.mu.Unlock()
		http.Error(w, "downloads are already running or stopping", http.StatusConflict)
		return
	}
	if err := d.store.SetDownloadsStopped(false); err != nil {
		d.mu.Unlock()
		http.Error(w, "unable to save download setting", http.StatusInternalServerError)
		return
	}
	d.stopped = false
	d.mu.Unlock()
	d.enqueue(tracks)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) stopDownloads(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	d := s.downloads
	d.mu.Lock()
	if err := d.store.SetDownloadsStopped(true); err != nil {
		d.mu.Unlock()
		http.Error(w, "unable to save download setting", http.StatusInternalServerError)
		return
	}
	d.stopped = true
	if d.cancel != nil {
		d.cancel()
	}
	stage, message := "stopped", "Downloads stopped."
	if d.state.Running {
		stage, message = "stopping", "Stopping downloads…"
	}
	d.publishLocked(downloadState{Running: d.state.Running, Total: d.state.Total, Finished: d.finished, Stage: stage, Message: message})
	d.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) downloadStatus(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", http.StatusServiceUnavailable)
		return
	}
	tracks, err := s.store.Downloadable()
	if err != nil {
		http.Error(w, "unable to read library", http.StatusInternalServerError)
		return
	}
	type item struct {
		ID         int64  `json:"id"`
		SourceID   string `json:"source_id"`
		Cover      string `json:"cover"`
		Playable   bool   `json:"playable"`
		Source     string `json:"source"`
		Artist     string `json:"artist"`
		Title      string `json:"title"`
		Stage      string `json:"stage"`
		Message    string `json:"message,omitempty"`
		Bytes      int64  `json:"bytes,omitempty"`
		TotalBytes int64  `json:"total_bytes,omitempty"`
	}
	pending := make([]store.Track, 0, len(tracks))
	for _, t := range tracks {
		if t.FilePath != "" {
			info, err := os.Stat(t.FilePath)
			if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				continue
			}
		}
		pending = append(pending, t)
	}
	d := s.downloads
	d.mu.Lock()
	items := make([]item, 0, len(pending))
	for _, t := range pending {
		stage := "waiting"
		if t.FilePath != "" {
			stage = "missing"
		}
		if t.DownloadFailed {
			stage = "failed"
		}
		status := d.tracks[trackKey(t.Source, t.SourceID)]
		if status.Stage != "" && status.Stage != "done" && status.Stage != "skipped" {
			stage = status.Stage
		}
		items = append(items, item{ID: t.ID, SourceID: t.SourceID, Cover: coverURL(t.CoverURL), Source: t.Source, Artist: t.Artist, Title: t.Title, Stage: stage, Message: status.Message, Bytes: status.Bytes, TotalBytes: status.TotalBytes})
	}
	state := d.state
	d.mu.Unlock()
	mine, err := s.store.ListUser(currentUser(r).ID)
	if err != nil {
		http.Error(w, "unable to read personal tracks", 500)
		return
	}
	personal := make([]item, 0, len(mine))
	queuedByID := make(map[int64]item, len(items))
	for i := range items {
		queuedByID[items[i].ID] = items[i]
	}
	for _, t := range mine {
		stage, playable := "waiting", false
		if info, e := os.Stat(t.FilePath); e == nil && info.Mode().IsRegular() && info.Size() > 0 {
			stage, playable = "downloaded", true
		}
		if queued, ok := queuedByID[t.ID]; ok && !playable {
			personal = append(personal, queued)
			continue
		}
		personal = append(personal, item{ID: t.ID, SourceID: t.SourceID, Cover: coverURL(t.CoverURL), Playable: playable, Source: t.Source, Artist: t.Artist, Title: t.Title, Stage: stage})
	}
	available, err := s.store.ListPlayable(currentUser(r).ID)
	if err != nil {
		http.Error(w, "unable to read available tracks", 500)
		return
	}
	s.mu.Lock()
	syncing := s.status[userSourceKey(r, "spotify")] == "Syncing…" || s.status[userSourceKey(r, "vk")] == "Syncing…"
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	library, err := s.store.LibrarySummary(currentUser(r).ID)
	if err != nil {
		http.Error(w, "unable to read library summary", http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(struct {
		Library      store.LibrarySummary `json:"library"`
		LibrarySync  string               `json:"library_sync"`
		State        downloadState        `json:"state"`
		Tracks       []item               `json:"tracks"`
		Mine         []item               `json:"my_tracks"`
		Available    []playableTrack      `json:"available_tracks"`
		LibraryCount int                  `json:"library_count"`
		Syncing      bool                 `json:"syncing"`
	}{library, s.librarySyncStatus(), state, items, personal, playableItems(available), len(tracks), syncing}); err != nil {
		s.logger.Error("write download status failed")
	}
}

func (d *downloadJob) saved(t store.Track) (bool, error) {
	path, err := download.FilePath(d.config.DownloadDir, t.Source, t.SourceID)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Size() > 0, nil
}

func (s *Server) downloadEvents(w http.ResponseWriter, r *http.Request) {
	if s.downloads == nil {
		http.Error(w, "downloads unavailable", 503)
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	d := s.downloads
	ch := make(chan []byte, 256)
	d.mu.Lock()
	d.listeners[ch] = struct{}{}
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.listeners, ch); d.mu.Unlock() }()
	_, _ = io.WriteString(w, ": connected\n\n")
	f.Flush()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			if _, _, err := s.auth.authenticate(r); err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			f.Flush()
		case <-ticker.C:
			if _, _, err := s.auth.authenticate(r); err != nil {
				return
			}
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			f.Flush()
		}
	}
}
