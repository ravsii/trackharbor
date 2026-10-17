// Package download contains application-independent file download mechanics.
package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/ravsii/trackharbor/audio"
	"github.com/ravsii/trackharbor/vk"
	"github.com/ravsii/trackharbor/youtube"
)

type Track struct {
	Source, Artist, Title, AudioURL, CoverURL string
	SourceID                                  string // Spotify track ID or VK owner_id_audio_id.
}

type Event struct {
	Stage string `json:"stage"`
	Bytes int64  `json:"bytes,omitempty"`
	Total int64  `json:"total,omitempty"`
}

type Routes struct {
	Personal, Public []string
	mu               sync.Mutex
	cooling          map[string]time.Time
	nextDownload     time.Time
}

var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var spotifyID = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
var vkAudioID = regexp.MustCompile(`^-?[0-9]+_[0-9]+$`)

func (r *Routes) candidates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	addresses := append([]string{""}, r.Personal...)
	return append(addresses, r.Public...)
}

// try makes one pass through all routes. The download queue owns cycle retries.
func (r *Routes) try(ctx context.Context, event func(Event), stage string, request func(string) error) error {
	var last error
	addresses := r.candidates()
	if len(addresses) == 0 {
		return fmt.Errorf("no configured download routes")
	}
	for _, address := range addresses {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		until := r.cooling[address]
		r.mu.Unlock()
		if wait := time.Until(until); wait > 0 {
			event(Event{Stage: "retrying"})
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if stage == "downloading" {
			r.mu.Lock()
			wait := max(0, time.Until(r.nextDownload))
			r.nextDownload = time.Now().Add(wait + 100*time.Millisecond)
			r.mu.Unlock()
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
		event(Event{Stage: stage})
		err := request(address)
		if err == nil {
			return nil
		}
		last = err
		if errors.Is(err, errRateLimit) {
			r.mu.Lock()
			if r.cooling == nil {
				r.cooling = make(map[string]time.Time)
			}
			r.cooling[address] = time.Now().Add(30 * time.Second)
			r.mu.Unlock()
		}
	}
	return last
}

var errRateLimit = errors.New("download rate limited")
var errNoMatch = errors.New("no YouTube match")

// Save publishes a nonempty staged MP3 under its deterministic catalog key.
// Its source URL must already be resolved. No file paths or hashes are persisted.
func Save(ctx context.Context, t Track, directory string, routes *Routes, event func(Event)) (string, error) {
	if event == nil {
		event = func(Event) {}
	}
	if routes == nil {
		routes = &Routes{}
	}
	destination, err := FilePath(directory, t.Source, t.SourceID)
	if err != nil {
		return "", err
	}
	if FileExists(destination) {
		return destination, nil
	}
	directory = filepath.Dir(destination)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", fmt.Errorf("ffmpeg is required: %w", err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	var input *os.File
	var release func() error
	var video string
	if t.Source == "vk" {
		if !vkAudioID.MatchString(t.SourceID) {
			return "", fmt.Errorf("invalid VK audio ID")
		}
		event(Event{Stage: "downloading"})
		f, _, closeFile, err := vk.DownloadMP3(ctx, t.AudioURL)
		if err != nil {
			return "", err
		}
		input, release = f, closeFile
	} else if t.Source == "spotify" || t.Source == "youtube" {
		video = youtubeVideo(t.AudioURL)
		if video == "" {
			return "", fmt.Errorf("YouTube URL has not been resolved")
		}
		executable, err := exec.LookPath("yt-dlp")
		if err != nil {
			return "", fmt.Errorf("yt-dlp is required: %w", err)
		}
		root := filepath.Join(directory, ".sources")
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", err
		}
		// Isolate incomplete yt-dlp output from other workers and from completed MP3s.
		dir, err := os.MkdirTemp(root, ".download-*")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		err = routes.try(ctx, event, "downloading", func(proxy string) error {
			dlCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
			defer stop()
			matches, _ := filepath.Glob(filepath.Join(dir, "audio.*"))
			for _, path := range matches {
				os.Remove(path)
			}
			writer := &progressWriter{event: event}
			_, stderr, runErr := youtube.Run(dlCtx, executable, proxy, `download:trackharbor-progress:{"downloaded":%(progress.downloaded_bytes|0)s,"total":%(progress.total_bytes,progress.total_bytes_estimate|0)s}`, writer,
				"--no-playlist", "--concurrent-fragments", "5", "--format", "bestaudio", "--fixup", "never", "--output", filepath.Join(dir, "audio.%(ext)s"), "--", "https://www.youtube.com/watch?v="+video)
			writer.flush()
			if runErr != nil {
				return ytError(stderr, runErr)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "audio.*"))
		var source string
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				source = path
				break
			}
		}
		if source == "" {
			return "", fmt.Errorf("downloaded YouTube audio is empty")
		}
		event(Event{Stage: "converting"})
		f, _, closeFile, err := audio.ConvertFileMP3(ctx, source)
		if err != nil {
			return "", err
		}
		input, release = f, closeFile
	} else {
		return "", fmt.Errorf("unknown track source")
	}
	defer release()
	stage, err := os.CreateTemp(directory, ".download-*.mp3")
	if err != nil {
		return "", err
	}
	defer os.Remove(stage.Name())
	if _, err := io.Copy(stage, input); err != nil {
		stage.Close()
		return "", err
	}
	if err := stage.Close(); err != nil {
		return "", err
	}
	event(Event{Stage: "metadata"})
	var image []byte
	artworkRoutes := routes
	if t.Source == "vk" {
		artworkRoutes = &Routes{}
	}
	if t.CoverURL != "" {
		image, _ = artwork(ctx, t.CoverURL, artworkRoutes, event)
	}
	if len(image) == 0 && video != "" {
		image, _ = artwork(ctx, "https://i.ytimg.com/vi/"+video+"/hqdefault.jpg", routes, event)
	}
	if err := tag(ctx, stage.Name(), t.Title, t.Artist, image); err != nil {
		if len(image) == 0 {
			return "", err
		}
		if err := tag(ctx, stage.Name(), t.Title, t.Artist, nil); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !FileExists(stage.Name()) {
		return "", fmt.Errorf("downloaded MP3 is empty")
	}
	if err := os.Link(stage.Name(), destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if !FileExists(destination) {
			if err := os.Rename(stage.Name(), destination); err != nil {
				return "", err
			}
			return destination, nil
		}
		// Another worker already published a nonempty file; keep it.
	}
	return destination, nil
}

func ytError(stderr []byte, err error) error {
	if bytes.Contains(stderr, []byte("HTTP Error 429")) {
		return errRateLimit
	}
	return fmt.Errorf("yt-dlp failed (check YouTube access, version and JavaScript runtime): %w", err)
}

type progressWriter struct {
	line  []byte
	event func(Event)
}

func (w *progressWriter) Write(data []byte) (int, error) {
	for _, b := range data {
		if b == '\r' || b == '\n' {
			w.flush()
		} else if len(w.line) < 4096 {
			w.line = append(w.line, b)
		}
	}
	return len(data), nil
}
func (w *progressWriter) flush() {
	var p struct {
		Downloaded int64 `json:"downloaded"`
		Total      int64 `json:"total"`
	}
	if bytes.HasPrefix(w.line, []byte("trackharbor-progress:")) && json.Unmarshal(w.line[len("trackharbor-progress:"):], &p) == nil {
		w.event(Event{Stage: "downloading", Bytes: p.Downloaded, Total: p.Total})
	}
	w.line = w.line[:0]
}

func artwork(ctx context.Context, raw string, routes *Routes, event func(Event)) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("artwork unavailable")
	}
	var data []byte
	requestCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	err = routes.try(requestCtx, event, "artwork", func(proxy string) error {
		transport := &http.Transport{TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
		if proxy != "" {
			p, e := url.Parse(proxy)
			if e != nil {
				return fmt.Errorf("invalid proxy")
			}
			transport.Proxy = http.ProxyURL(p)
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Timeout: 12 * time.Second, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return http.ErrUseLastResponse
			}
			return nil
		}}
		req, e := http.NewRequestWithContext(requestCtx, "GET", raw, nil)
		if e != nil {
			return e
		}
		resp, e := client.Do(req)
		if e != nil {
			return fmt.Errorf("artwork unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode == 429 {
			return errRateLimit
		}
		if resp.StatusCode != 200 || resp.ContentLength > 5<<20 {
			return fmt.Errorf("artwork unavailable")
		}
		data, e = io.ReadAll(io.LimitReader(resp.Body, 5<<20+1))
		if e != nil || len(data) > 5<<20 {
			return fmt.Errorf("artwork unavailable")
		}
		mime := http.DetectContentType(data)
		if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
			return fmt.Errorf("unsupported artwork")
		}
		return nil
	})
	return data, err
}

func tag(ctx context.Context, mp3, title, artist string, image []byte) error {
	args := []string{"-v", "error", "-nostdin", "-y", "-i", mp3}
	if len(image) > 0 {
		f, err := os.CreateTemp(filepath.Dir(mp3), ".cover-*.img")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(image); err != nil {
			f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		args = append(args, "-i", f.Name())
	}
	out, err := os.CreateTemp(filepath.Dir(mp3), ".tagged-*.mp3")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err = out.Close(); err != nil {
		return err
	}
	args = append(args, "-map", "0:a:0", "-c:a", "copy")
	if len(image) > 0 {
		args = append(args, "-map", "1:v:0", "-c:v", "mjpeg", "-disposition:v:0", "attached_pic")
	}
	args = append(args, "-metadata", "title="+title, "-metadata", "artist="+artist, "-id3v2_version", "3", out.Name())
	c, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := exec.CommandContext(c, "ffmpeg", args...).Run(); err != nil {
		return fmt.Errorf("write MP3 metadata: %w", err)
	}
	return os.Rename(out.Name(), mp3)
}
