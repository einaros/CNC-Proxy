// Package api exposes the service over HTTP/REST plus a Server-Sent Events
// stream for the web UI. It is a thin transport layer: all behavior lives in
// the service, so the API never blocks on the machine.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/jog"
	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// Server holds the HTTP handlers.
type Server struct {
	svc            *service.Service
	jog            *jog.Manager
	maxUploadBytes int64
	maxJSONBytes   int64
	maxBackupBytes int64
}

// Options configures optional API surfaces.
type Options struct {
	Jog            *jog.Manager
	MaxUploadBytes int64
	MaxJSONBytes   int64
	MaxBackupBytes int64
}

// New creates an API server.
func New(svc *service.Service) *Server { return NewWithOptions(svc, Options{}) }

// NewWithOptions creates an API server with optional feature managers.
func NewWithOptions(svc *service.Service, opts Options) *Server {
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = 512 << 20
	}
	if opts.MaxJSONBytes <= 0 {
		opts.MaxJSONBytes = 1 << 20
	}
	if opts.MaxBackupBytes <= 0 {
		opts.MaxBackupBytes = 64 << 20
	}
	return &Server{
		svc:            svc,
		jog:            opts.Jog,
		maxUploadBytes: opts.MaxUploadBytes,
		maxJSONBytes:   opts.MaxJSONBytes,
		maxBackupBytes: opts.MaxBackupBytes,
	}
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/machine", s.getMachine)
	mux.HandleFunc("GET /api/machine/status", s.getMachine)
	mux.HandleFunc("GET /api/files", s.getFiles)
	mux.HandleFunc("POST /api/files", s.postFile)
	mux.HandleFunc("GET /api/files/", s.getFileContent)    // GET /api/files/{path...}
	mux.HandleFunc("DELETE /api/files/", s.deleteFile)     // DELETE /api/files/{path...}
	mux.HandleFunc("POST /api/files/rename", s.renameFile) // body: {from,to}
	mux.HandleFunc("POST /api/dirs", s.postDir)            // body: {path}
	mux.HandleFunc("GET /api/jobs", s.getJobs)
	mux.HandleFunc("GET /api/runs", s.getRuns)
	mux.HandleFunc("POST /api/gcode", s.postGcode)      // body: {line}
	mux.HandleFunc("GET /api/gcode/log", s.getGcodeLog) // recent gcode I/O lines
	mux.HandleFunc("POST /api/control", s.postControl)  // body: {action: hold|resume|halt|recover|unlock|home|reset}
	mux.HandleFunc("GET /api/ui/settings", s.getUISettings)
	mux.HandleFunc("PUT /api/ui/settings", s.putUISettings)
	mux.HandleFunc("GET /api/backup", s.getBackup)
	mux.HandleFunc("POST /api/backup/import", s.postBackupImport)
	mux.HandleFunc("GET /api/jog/capabilities", s.getJogCapabilities)
	mux.HandleFunc("GET /api/jog/ws", s.jogWS)
	mux.HandleFunc("GET /api/events", s.events)
	// Everything not under /api/ is the embedded web UI.
	mux.Handle("/", webHandler())
	return sameOriginGuard(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxJSONBytes)).Decode(dst)
	if err == nil {
		return true
	}
	if requestBodyTooLarge(err) {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
	} else {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	return false
}

func requestBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http: request body too large")
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

func (s *Server) getRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.RunHistory())
}

// postFile accepts either a multipart upload (field "file", path from form
// "path" or the filename) or a raw body with the path in the "X-Path" header /
// "path" query parameter.
func (s *Server) postFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		f, hdr, err := r.FormFile("file")
		if err != nil {
			if requestBodyTooLarge(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, "upload too large")
				return
			}
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
		if requestBodyTooLarge(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
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
	if !s.decodeJSON(w, r, &body) {
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
	if !s.decodeJSON(w, r, &body) {
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
	case errors.Is(err, service.ErrRecoveryUnavailable):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrMachineStatusStale):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
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
	if !s.decodeJSON(w, r, &body) {
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

// getGcodeLog returns the retained gcode I/O lines (oldest first), so a client
// can backfill history before following the live SSE stream.
func (s *Server) getGcodeLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.GcodeLog().Recent())
}

func (s *Server) getUISettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.UISettings())
}

func (s *Server) putUISettings(w http.ResponseWriter, r *http.Request) {
	var body store.UISettings
	if !s.decodeJSON(w, r, &body) {
		return
	}
	ui, err := s.svc.SetUISettings(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ui)
}

// postControl injects a realtime control action, or runs an explicit alarm
// recovery action. Hold/resume/halt are out-of-band and intentionally work even
// while the machine is moving. Recover/unlock/home/reset are normal serialized
// firmware commands, but bypass generic idle-gated gcode because they are only
// useful once the machine is in Alarm.
func (s *Server) postControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if !s.decodeJSON(w, r, &body) {
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "recover" || action == "unlock" || action == "home" || action == "reset" {
		result, err := s.svc.RecoverAlarm(action)
		if err != nil {
			s.mapError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	var c byte
	switch action {
	case "hold", "feedhold", "pause":
		c = service.ControlFeedHold
	case "resume":
		c = service.ControlResume
	case "halt", "stop", "estop":
		c = service.ControlHalt
	default:
		writeErr(w, http.StatusBadRequest, "action must be one of: hold, resume, halt, recover, unlock, home, reset")
		return
	}
	if err := s.svc.SendControl(c); err != nil {
		s.mapError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) getBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="cnc-proxy-backup.json"`)
	writeJSON(w, http.StatusOK, s.svc.ExportBackup())
}

func (s *Server) postBackupImport(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var backup service.Backup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxBackupBytes)).Decode(&backup); err != nil {
		if requestBodyTooLarge(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, "backup import body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid backup JSON: "+err.Error())
		}
		return
	}
	if err := s.svc.ImportBackup(backup); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "imported"})
}

// events streams catalog/job/machine/gcode changes as Server-Sent Events.
// Optional scope narrows the stream for UI surfaces that should not depend on
// unrelated data being loaded:
//   - all or empty: machine, files, jobs, and gcode
//   - control: machine and gcode only
//   - files: machine, files, and jobs only
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	scope := strings.ToLower(r.URL.Query().Get("scope"))
	if scope != "" && scope != "all" && scope != "control" && scope != "files" {
		scope = "all"
	}
	includeFiles := scope == "" || scope == "all" || scope == "files"
	includeGcode := scope == "" || scope == "all" || scope == "control"

	var ch <-chan store.Event
	var unsub func()
	if includeFiles {
		ch, unsub = s.svc.Subscribe()
		defer unsub()
	}
	var gch <-chan gcodelog.Line
	var gunsub func()
	if includeGcode {
		gch, gunsub = s.svc.GcodeLog().Subscribe()
		defer gunsub()
	}

	// Send an initial snapshot so a fresh client is immediately consistent.
	// Subscriptions are already active, so lines logged from here on arrive as
	// gcode events; duplicates against the snapshot are detectable by seq.
	snap := map[string]any{
		"machine": s.svc.Status(),
	}
	if includeFiles {
		snap["files"] = s.svc.Files()
		snap["jobs"] = s.svc.Jobs()
	}
	if includeGcode {
		snap["gcode"] = s.svc.GcodeLog().Recent()
		snap["runs"] = s.svc.RunHistory()
	}
	sendEvent(w, "snapshot", snap)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			sendEvent(w, "change", s.svc.EnrichEventJob(ev))
			flusher.Flush()
		case ln, ok := <-gch:
			if !ok {
				return
			}
			sendEvent(w, "gcode", ln)
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

func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresSameOrigin(r) {
			next.ServeHTTP(w, r)
			return
		}
		if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); site == "cross-site" {
			writeErr(w, http.StatusForbidden, "cross-site request rejected")
			return
		}
		if !sameOrigin(r, r.Header.Get("Origin")) {
			writeErr(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		if r.Header.Get("Origin") == "" && !sameOrigin(r, r.Header.Get("Referer")) {
			writeErr(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requiresSameOrigin(r *http.Request) bool {
	if r.URL.Path == "/api/jog/ws" {
		return true
	}
	if r.URL.Path == "/api/backup" {
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func sameOrigin(r *http.Request, raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return strings.EqualFold(normalizeHost(u.Host), normalizeHost(host)) &&
		strings.EqualFold(u.Scheme, requestScheme(r))
}

func requestScheme(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		if i := strings.IndexByte(xf, ','); i >= 0 {
			xf = xf[:i]
		}
		return strings.ToLower(strings.TrimSpace(xf))
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func normalizeHost(host string) string {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return strings.ToLower(host)
	}
	if (p == "80" || p == "443") && h != "" {
		return strings.ToLower(h)
	}
	return strings.ToLower(net.JoinHostPort(h, p))
}
