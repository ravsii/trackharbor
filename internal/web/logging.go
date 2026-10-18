package web

import (
	"bytes"
	"net/http"
	"time"

	"go.uber.org/zap"
)

type loggedResponse struct {
	http.ResponseWriter
	status int
	events int
	stream bool
}

func (w *loggedResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	if w.stream && err == nil && n == len(data) && bytes.HasPrefix(data, []byte("data: ")) {
		w.events++
	}
	return n, err
}

func (w *loggedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type loggedStream struct{ *loggedResponse }

func (w *loggedStream) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func logRequests(logger *zap.Logger, mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, route := mux.Handler(r)
		if route == "" {
			route = "unmatched"
		}
		stream := route == "GET /download/events"
		response := &loggedResponse{ResponseWriter: w, stream: stream}
		var writer http.ResponseWriter = response
		if _, ok := w.(http.Flusher); ok {
			writer = &loggedStream{response}
		}
		started := time.Now()
		if stream {
			logger.Info("sse request started", zap.String("route", route))
		}
		next.ServeHTTP(writer, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if !stream && status < 400 && routineRequest(route) {
			return
		}
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("route", route),
			zap.Int("status", status),
			zap.Duration("duration", time.Since(started)),
		}
		if stream {
			fields = append(fields, zap.Int("events_sent", response.events))
		}
		if status >= 500 {
			logger.Error("http request finished", fields...)
		} else {
			logger.Info("http request finished", fields...)
		}
	})
}

func routineRequest(route string) bool {
	switch route {
	case "GET /assets/", "GET /status", "GET /ping-status", "GET /download/status", "GET /audio", "GET /audio/tracks", "GET /auth/status",
		"GET /webdav", "GET /webdav/", "OPTIONS /webdav", "OPTIONS /webdav/", "PROPFIND /webdav", "PROPFIND /webdav/":
		return true
	}
	return false
}
