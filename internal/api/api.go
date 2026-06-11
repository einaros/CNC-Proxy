// Package api exposes the service over HTTP/REST plus a Server-Sent Events
// stream for the web UI. It is a thin transport layer: all behavior lives in
// the service, so the API never blocks on the machine.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
)

// Server holds the HTTP handlers.
type Server struct {
	svc *service.Service
}

// New creates an API server.
func New(svc *service.Service) *Server { return &Server{svc: svc} }

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/machine", s.getMachine)
	mux.HandleFunc("GET /api/files", s.getFiles)
	mux.HandleFunc("POST /api/files", s.postFile)
	mux.HandleFunc("GET /api/files/", s.getFileContent)    // GET /api/files/{path...}
	mux.HandleFunc("DELETE /api/files/", s.deleteFile)     // DELETE /api/files/{path...}
	mux.HandleFunc("POST /api/files/rename", s.renameFile) // body: {from,to}
	mux.HandleFunc("POST /api/dirs", s.postDir)            // body: {path}
	mux.HandleFunc("GET /api/jobs", s.getJobs)
	mux.HandleFunc("POST /api/gcode", s.postGcode) // body: {line}
	mux.HandleFunc("GET /api/events", s.events)
	// Everything not under /api/ is the embedded web UI.
	mux.Handle("/", webHandler())
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) getMachine(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Status())
}

func (s *Server) getFiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Files())
}

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Jobs())
}

// postFile accepts either a multipart upload (field "file", path from form
// "path" or the filename) or a raw body with the path in the "X-Path" header /
// "path" query parameter.
func (s *Server) postFile(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
			return
		}
		defer f.Close()
		remote := r.FormValue("path")
		if remote == "" {
			remote = hdr.Filename
		}
		s.doUpload(w, remote, f)
		return
	}
	remote := r.URL.Query().Get("path")
	if remote == "" {
		remote = r.Header.Get("X-Path")
	}
	if remote == "" {
		writeErr(w, http.StatusBadRequest, "path required (query ?path= or X-Path header)")
		return
	}
	defer r.Body.Close()
	s.doUpload(w, remote, r.Body)
}

func (s *Server) doUpload(w http.ResponseWriter, remote string, r io.Reader) {
	entry, err := s.svc.Upload(remote, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) getFileContent(w http.ResponseWriter, r *http.Request) {
	remote := strings.TrimPrefix(r.URL.Path, "/api/files/")
	rc, entry, err := s.svc.Open(remote)
	if err != nil {
		s.mapError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.Size))
	io.Copy(w, rc)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	remote := strings.TrimPrefix(r.URL.Path, "/api/files/")
	if err := s.svc.Delete(remote); err != nil {
		s.mapError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) renameFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.svc.Rename(body.From, body.To); err != nil {
		s.mapError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) postDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	entry, err := s.svc.Mkdir(body.Path)
	if err != nil {
		s.mapError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrNotCached):
		writeErr(w, http.StatusConflict, err.Error())
	case session.Retryable(err):
		// Machine busy / controller mid-transfer / not idle: try again later.
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

// postGcode runs a single gcode line on the machine and returns its output. It
// works whether or not a controller is connected (injected during relay mode).
func (s *Server) postGcode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Line string `json:"line"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Line == "" {
		writeErr(w, http.StatusBadRequest, "line required")
		return
	}
	out, err := s.svc.SendGcode(body.Line)
	if err != nil {
		s.mapError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

// events streams catalog/job/machine changes as Server-Sent Events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.svc.Subscribe()
	defer unsub()

	// Send an initial snapshot so a fresh client is immediately consistent.
	sendEvent(w, "snapshot", map[string]any{
		"machine": s.svc.Status(),
		"files":   s.svc.Files(),
		"jobs":    s.svc.Jobs(),
	})
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			sendEvent(w, "change", ev)
			flusher.Flush()
		}
	}
}

func sendEvent(w io.Writer, event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}
