package traymgr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/webguard"
)

type Notification struct {
	Title   string    `json:"title"`
	Message string    `json:"message"`
	Level   string    `json:"level,omitempty"`
	Time    time.Time `json:"time"`
}

type ManagerLogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
}

type Notifier interface {
	Notify(Notification) error
}

type Server struct {
	configPath  string
	supervisor  *Supervisor
	notifier    Notifier
	restartCh   chan struct{}
	restartLag  time.Duration
	processExit func()
	ready       chan struct{}
	readyOnce   sync.Once
	logPath     string
	mountStatus func(context.Context) WebDAVMountStatus
	setMount    func(context.Context, bool) error
	remount     func(context.Context) error

	mu            sync.Mutex
	notifications []Notification
	managerLog    []ManagerLogEntry
	logLines      int // entries currently in the on-disk manager log file
	startedAt     time.Time
	restartedAt   time.Time
}

// managerLogMaxEntries bounds both the in-memory manager log and the on-disk
// cnc-manager.log file.
const managerLogMaxEntries = 200

func NewServer(configPath string, supervisor *Supervisor, notifier Notifier) *Server {
	now := time.Now()
	logPath := DefaultManagerLogPath(configPath)
	entries, lines := loadManagerLog(logPath)
	return &Server{configPath: configPath, supervisor: supervisor, notifier: notifier, restartCh: make(chan struct{}, 1), restartLag: 100 * time.Millisecond, processExit: func() { os.Exit(0) }, ready: make(chan struct{}), logPath: logPath, managerLog: entries, logLines: lines, startedAt: now, restartedAt: now}
}

func (s *Server) SetManagerProcessExit(fn func()) {
	s.processExit = fn
}

func (s *Server) SetWebDAVMountControls(status func(context.Context) WebDAVMountStatus, setMount func(context.Context, bool) error, remount func(context.Context) error) {
	s.mountStatus = status
	s.setMount = setMount
	s.remount = remount
}

func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	for _, method := range []string{"GET", "OPTIONS", "PROPFIND", "PROPPATCH", "MKCOL", "PUT", "DELETE", "COPY", "MOVE", "LOCK", "UNLOCK"} {
		mux.HandleFunc(method+" /webdav", s.localWebDAVProxy)
		mux.HandleFunc(method+" /webdav/", s.localWebDAVProxy)
	}
	mux.HandleFunc("GET /api/status", s.withAuth(s.status))
	mux.HandleFunc("GET /api/config", s.withAuth(s.getConfig))
	mux.HandleFunc("PUT /api/config", s.withAuth(s.putConfig))
	mux.HandleFunc("PUT /api/manager/config", s.withAuth(s.putManagerConfig))
	mux.HandleFunc("POST /api/manager/restart", s.withAuth(s.restartManager))
	mux.HandleFunc("DELETE /api/manager/log", s.withAuth(s.clearManagerLogHandler))
	mux.HandleFunc("PUT /api/proxy/config", s.withAuth(s.putProxyConfig))
	mux.HandleFunc("POST /api/proxy/start", s.withAuth(s.startProxy))
	mux.HandleFunc("POST /api/proxy/stop", s.withAuth(s.stopProxy))
	mux.HandleFunc("POST /api/proxy/restart", s.withAuth(s.restartProxy))
	mux.HandleFunc("POST /api/proxy/build", s.withAuth(s.buildProxy))
	mux.HandleFunc("GET /api/webdav/mount", s.withAuth(s.webDAVMount))
	mux.HandleFunc("PUT /api/webdav/mount", s.withAuth(s.setWebDAVMount))
	mux.HandleFunc("POST /api/webdav/remount", s.withAuth(s.remountWebDAV))
	mux.HandleFunc("POST /api/notify", s.withAuth(s.notify))
	mux.HandleFunc("POST /api/deploy", s.withAuth(s.deploy))
	return webguard.Handler(mux, webguard.Options{
		RequiresSameOrigin: managerRequiresSameOrigin,
		AllowHost:          webguard.AllowIPLiteralOrLocalhost,
	})
}

// managerRequiresSameOrigin guards every mutating method (POST/PUT/DELETE and
// the WebDAV write methods proxied under /webdav) against cross-site browser
// requests and DNS-rebound hosts. Read-only methods stay unguarded so status
// pages, the SPA, and native WebDAV clients (which send no fetch metadata)
// keep working.
func managerRequiresSameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func (s *Server) localWebDAVProxy(w http.ResponseWriter, r *http.Request) {
	if !requestFromLoopback(r) {
		http.Error(w, "webdav proxy is only available from loopback", http.StatusForbidden)
		return
	}
	cfg := s.supervisor.Config()
	target, err := url.Parse(WebDAVBase(cfg))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	user, token := Auth(cfg)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			rewriteWebDAVProxyRequest(req, target)
			if token != "" {
				req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+token)))
			} else {
				req.Header.Del("Authorization")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "webdav proxy: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func rewriteWebDAVProxyRequest(req *http.Request, target *url.URL) {
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path = joinURLPath(target.Path, strings.TrimPrefix(req.URL.Path, "/webdav"))
	req.URL.RawPath = ""
	req.Host = target.Host
	if dst := req.Header.Get("Destination"); dst != "" {
		if rewritten, ok := rewriteWebDAVDestination(dst, target); ok {
			req.Header.Set("Destination", rewritten)
		}
	}
}

func rewriteWebDAVDestination(raw string, target *url.URL) (string, bool) {
	dst, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	dst.Scheme = target.Scheme
	dst.Host = target.Host
	dst.Path = joinURLPath(target.Path, strings.TrimPrefix(dst.Path, "/webdav"))
	dst.RawPath = ""
	return dst.String(), true
}

func joinURLPath(base, suffix string) string {
	if base == "" {
		base = "/"
	}
	if suffix == "" || suffix == "/" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(suffix, "/")
}

func requestFromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	for {
		err := s.listenAndServeOnce(ctx)
		if errors.Is(err, errManagerRestart) {
			continue
		}
		return err
	}
}

var errManagerRestart = errors.New("manager restart requested")

func (s *Server) listenAndServeOnce(ctx context.Context) error {
	cfg := s.supervisor.Config()
	srv := &http.Server{
		Addr:              cfg.AdminListen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Minute,
		WriteTimeout:      15 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	ln, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		return err
	}
	s.markReady()

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return <-errCh
	case <-s.restartCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		s.markManagerRestarted()
		return errManagerRestart
	case err := <-errCh:
		return err
	}
}

func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.supervisor.Config()
		if cfg.AdminToken != "" {
			token := r.Header.Get("X-CNC-Tray-Token")
			if token == "" {
				if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				}
			}
			if token == "" {
				_, pass, ok := r.BasicAuth()
				if ok {
					token = pass
				}
			}
			if token != cfg.AdminToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	startedAt, restartedAt := s.managerTimes()
	s.writeJSON(w, map[string]any{
		"process":              s.supervisor.State(),
		"config":               s.supervisor.Config(),
		"api_base":             APIBase(s.supervisor.Config()),
		"manager_base":         ManagerBase(s.supervisor.Config()),
		"manager_urls":         ManagerURLs(s.supervisor.Config()),
		"manager_started_at":   startedAt,
		"manager_restarted_at": restartedAt,
		"notifications":        s.recentNotifications(),
		"manager_log":          s.recentManagerLog(),
		"webdav_mount":         s.webDAVMountStatus(r.Context()),
	})
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	options, source := ProxyOptionsForConfig(r.Context(), s.supervisor.Config())
	cfg := normalizeConfigWithOptions(s.supervisor.Config(), options)
	s.writeJSON(w, map[string]any{
		"config":        cfg,
		"options":       options,
		"option_source": source,
	})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	old := s.supervisor.Config()
	var cfg Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := SaveConfig(s.configPath, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.supervisor.SetConfig(cfg)
	next := s.supervisor.Config()
	restart := managerSettingsChanged(old, next)
	s.writeJSON(w, map[string]any{"ok": true, "config": next, "manager_restarting": restart, "manager_url": ManagerBase(next)})
	if restart {
		s.addNotification(Notification{Title: "CNC Proxy Manager", Message: "Manager configuration saved; manager listener restarting", Level: "info", Time: time.Now()})
		s.scheduleRestart()
	}
}

func (s *Server) putManagerConfig(w http.ResponseWriter, r *http.Request) {
	var in Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	next := s.supervisor.Config()
	next.ProxyBinary = in.ProxyBinary
	next.SourceDir = in.SourceDir
	next.BuildCommand = in.BuildCommand
	next.AutoStart = in.AutoStart
	next.AdminListen = in.AdminListen
	next.AdminToken = in.AdminToken
	if err := SaveManagerConfig(s.configPath, next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.supervisor.SetConfig(next)
	next = s.supervisor.Config()
	s.writeJSON(w, map[string]any{"ok": true, "config": next, "manager_restarting": true, "manager_url": ManagerBase(next), "manager_urls": ManagerURLs(next)})
	s.addNotification(Notification{Title: "CNC Proxy Manager", Message: "Manager settings saved; manager listener restarting", Level: "info", Time: time.Now()})
	s.scheduleRestart()
}

func (s *Server) putProxyConfig(w http.ResponseWriter, r *http.Request) {
	old := s.supervisor.Config()
	options, optionSource := ProxyOptionsForConfig(r.Context(), old)
	var in struct {
		Flags map[string]string `json:"flags"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.Flags == nil {
		http.Error(w, "proxy flags are required", http.StatusBadRequest)
		return
	}
	next := s.supervisor.Config()
	flags := map[string]string{}
	for k, v := range next.Flags {
		flags[k] = v
	}
	for k, v := range in.Flags {
		flags[k] = v
	}
	next.Flags = flags
	next = normalizeConfigWithOptions(next, options)
	if err := SaveProxyConfigWithOptions(s.configPath, next, options); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wasRunning := s.supervisor.State().Running
	changed := !reflect.DeepEqual(normalizeConfigWithOptions(old, options).Flags, next.Flags)
	s.supervisor.SetConfig(next)
	restarted := false
	if changed && wasRunning {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := s.supervisor.Restart(ctx); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			s.writeJSON(w, map[string]any{"ok": false, "error": "proxy settings saved, but restart failed: " + err.Error(), "config": s.supervisor.Config(), "proxy_restart_failed": true, "process": s.supervisor.State(), "option_source": optionSource})
			return
		}
		restarted = true
		s.addNotification(Notification{Title: "CNC Proxy", Message: "Proxy settings saved and proxy restarted", Level: "success", Time: time.Now()})
	} else if changed {
		s.addNotification(Notification{Title: "CNC Proxy", Message: "Proxy settings saved; proxy is stopped", Level: "info", Time: time.Now()})
	} else {
		s.addNotification(Notification{Title: "CNC Proxy", Message: "Proxy settings saved; no restart needed", Level: "info", Time: time.Now()})
	}
	s.writeJSON(w, map[string]any{"ok": true, "config": s.supervisor.Config(), "proxy_restarted": restarted, "proxy_was_running": wasRunning, "proxy_changed": changed, "process": s.supervisor.State(), "option_source": optionSource})
}

func (s *Server) restartManager(w http.ResponseWriter, r *http.Request) {
	cfg := s.supervisor.Config()
	s.writeJSON(w, map[string]any{"ok": true, "manager_restarting": true, "manager_url": ManagerBase(cfg), "manager_urls": ManagerURLs(cfg)})
	s.addNotification(Notification{Title: "CNC Proxy Manager", Message: "Manager listener restart requested", Level: "info", Time: time.Now()})
	s.scheduleRestart()
}

func (s *Server) clearManagerLogHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.clearManagerLog(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "manager_log": []ManagerLogEntry{}})
}

func (s *Server) startProxy(w http.ResponseWriter, r *http.Request) {
	if err := s.supervisor.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "process": s.supervisor.State()})
}

func (s *Server) stopProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.supervisor.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "process": s.supervisor.State()})
}

func (s *Server) restartProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.supervisor.Restart(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "process": s.supervisor.State()})
}

func (s *Server) buildProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := s.supervisor.Build(ctx)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "output": out})
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "output": out})
}

func (s *Server) webDAVMount(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, map[string]any{"ok": true, "mount": s.webDAVMountStatus(r.Context())})
}

func (s *Server) setWebDAVMount(w http.ResponseWriter, r *http.Request) {
	if s.setMount == nil {
		http.Error(w, "webdav mount control is unavailable", http.StatusNotImplemented)
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if in.Enabled == nil {
		http.Error(w, "enabled is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	err := s.setMount(ctx, *in.Enabled)
	mount := s.webDAVMountStatus(context.Background())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "mount": mount})
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "mount": mount})
}

func (s *Server) remountWebDAV(w http.ResponseWriter, r *http.Request) {
	if s.remount == nil {
		http.Error(w, "webdav remount is unavailable", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := s.remount(ctx); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "mount": s.webDAVMountStatus(r.Context())})
		return
	}
	s.writeJSON(w, map[string]any{"ok": true, "mount": s.webDAVMountStatus(r.Context())})
}

func (s *Server) webDAVMountStatus(ctx context.Context) WebDAVMountStatus {
	if s.mountStatus == nil {
		return WebDAVMountStatus{}
	}
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.mountStatus(statusCtx)
}

func (s *Server) notify(w http.ResponseWriter, r *http.Request) {
	var n Notification
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&n); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(n.Title) == "" {
		n.Title = "CNC Proxy"
	}
	if strings.TrimSpace(n.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	n.Time = time.Now()
	s.addNotification(n)
	if s.notifier != nil {
		_ = s.notifier.Notify(n)
	}
	s.writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	restart := r.URL.Query().Get("restart") != "false"
	component, err := deployComponent(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tmp, err := os.CreateTemp("", "cnc-deploy-*.zip")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 1024<<20)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(1024 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		f, _, err := r.FormFile("source")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		if _, err := io.Copy(tmp, f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if _, err := io.Copy(tmp, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := tmp.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	targetBinary := ""
	if component.BuildManager {
		targetBinary, err = managerBinaryPath("")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	res, err := s.supervisor.DeployZipWithOptions(ctx, tmpName, DeployOptions{
		BuildProxy:    component.BuildProxy,
		BuildManager:  component.BuildManager,
		RestartProxy:  restart,
		ManagerBinary: targetBinary,
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.addNotification(Notification{Title: "CNC Proxy", Message: "Deployment failed: " + err.Error(), Level: "error", Time: time.Now()})
		s.writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "result": res})
		return
	}
	managerRestart := false
	if res.ManagerUpgrade != nil {
		if err := LaunchManagerUpgradeFinalizer(*res.ManagerUpgrade, s.configPath); err != nil {
			cleanupManagerUpgrade(res.ManagerUpgrade)
			if res.ManagerUpgrade.ProxyStartOnRelaunch {
				if startErr := s.supervisor.Start(); startErr == nil {
					res.Restarted = true
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
			msg := "Manager upgrade was built, but finalizer launch failed: " + err.Error()
			s.addNotification(Notification{Title: "CNC Proxy Manager", Message: msg, Level: "error", Time: time.Now()})
			s.writeJSON(w, map[string]any{"ok": false, "error": msg, "result": res})
			return
		}
		res.ManagerUpgrade.RestartScheduled = true
		managerRestart = true
	}
	msg := "Deployment completed"
	if res.Restarted {
		msg += "; proxy restarted"
	}
	if managerRestart {
		msg += "; manager restart scheduled"
	}
	s.addNotification(Notification{Title: "CNC Proxy", Message: msg, Level: "success", Time: time.Now()})
	s.writeJSON(w, map[string]any{"ok": true, "result": res})
	if managerRestart {
		s.scheduleManagerProcessExit()
	}
}

type deployComponentSelection struct {
	BuildProxy   bool
	BuildManager bool
}

func deployComponent(r *http.Request) (deployComponentSelection, error) {
	component := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("component")))
	if component == "" {
		component = "proxy"
	}
	switch component {
	case "proxy":
		return deployComponentSelection{BuildProxy: true}, nil
	case "manager", "tray":
		return deployComponentSelection{BuildManager: true}, nil
	case "all", "both":
		return deployComponentSelection{BuildProxy: true, BuildManager: true}, nil
	default:
		return deployComponentSelection{}, fmt.Errorf("component must be proxy, manager, or all")
	}
}

func (s *Server) addNotification(n Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, n)
	if len(s.notifications) > 50 {
		s.notifications = append([]Notification(nil), s.notifications[len(s.notifications)-50:]...)
	}
}

func (s *Server) addManagerLog(level, source, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	entry := ManagerLogEntry{
		Time:    time.Now(),
		Level:   strings.TrimSpace(level),
		Source:  strings.TrimSpace(source),
		Message: message,
	}
	if entry.Level == "" {
		entry.Level = "info"
	}
	if entry.Source == "" {
		entry.Source = "manager"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managerLog = append(s.managerLog, entry)
	if len(s.managerLog) > managerLogMaxEntries {
		s.managerLog = append([]ManagerLogEntry(nil), s.managerLog[len(s.managerLog)-managerLogMaxEntries:]...)
	}
	if s.logPath != "" {
		// Keep the on-disk file at the same bound as the in-memory log:
		// append while under the cap, otherwise rewrite the file from the
		// already-capped in-memory entries (which include this entry).
		if s.logLines >= managerLogMaxEntries {
			if rewriteManagerLogFile(s.logPath, s.managerLog) == nil {
				s.logLines = len(s.managerLog)
			}
		} else if appendManagerLogEntry(s.logPath, entry) == nil {
			s.logLines++
		}
	}
}

func (s *Server) recentNotifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.notifications...)
}

func (s *Server) recentManagerLog() []ManagerLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ManagerLogEntry(nil), s.managerLog...)
}

func (s *Server) clearManagerLog() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logPath != "" {
		if err := clearManagerLogFile(s.logPath); err != nil {
			return err
		}
	}
	s.managerLog = nil
	s.logLines = 0
	return nil
}

func (s *Server) managerTimes() (time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt, s.restartedAt
}

func (s *Server) markManagerRestarted() {
	s.mu.Lock()
	s.restartedAt = time.Now()
	s.mu.Unlock()
	s.addNotification(Notification{Title: "CNC Proxy Manager", Message: "Manager listener restarted", Level: "success", Time: time.Now()})
}

func (s *Server) markReady() {
	s.readyOnce.Do(func() {
		close(s.ready)
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintln(w, err)
	}
}

func (s *Server) scheduleRestart() {
	go func() {
		if s.restartLag > 0 {
			time.Sleep(s.restartLag)
		}
		select {
		case s.restartCh <- struct{}{}:
		default:
		}
	}()
}

func (s *Server) scheduleManagerProcessExit() {
	go func() {
		if s.restartLag > 0 {
			time.Sleep(s.restartLag)
		}
		if s.processExit != nil {
			s.processExit()
		}
	}()
}

func managerSettingsChanged(a, b Config) bool {
	a = normalizeConfig(a)
	b = normalizeConfig(b)
	return a.ProxyBinary != b.ProxyBinary ||
		a.SourceDir != b.SourceDir ||
		a.BuildCommand != b.BuildCommand ||
		a.AutoStart != b.AutoStart ||
		a.AdminListen != b.AdminListen ||
		a.AdminToken != b.AdminToken
}

func DefaultLogPath(configPath string) string {
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "cnc-proxy.log")
}

func DefaultManagerLogPath(configPath string) string {
	dir := filepath.Dir(configPath)
	return filepath.Join(dir, "cnc-manager.log")
}

func appendManagerLogEntry(path string, entry ManagerLogEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

// loadManagerLog reads the on-disk manager log, returning the most recent
// entries (bounded to managerLogMaxEntries) and the total number of entries
// currently stored in the file.
func loadManagerLog(path string) ([]ManagerLogEntry, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	var out []ManagerLogEntry
	total := 0
	dec := json.NewDecoder(f)
	for {
		var entry ManagerLogEntry
		if err := dec.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, total
		}
		out = append(out, entry)
		total++
		if len(out) > managerLogMaxEntries {
			out = append([]ManagerLogEntry(nil), out[len(out)-managerLogMaxEntries:]...)
		}
	}
	return out, total
}

// rewriteManagerLogFile atomically replaces the on-disk manager log with the
// given (already bounded) entries, using the same stage-then-rename discipline
// as the store.
func rewriteManagerLogFile(path string, entries []ManagerLogEntry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cnc-manager-log-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	enc := json.NewEncoder(tmp)
	for _, entry := range entries {
		if err := enc.Encode(entry); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func clearManagerLogFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}
