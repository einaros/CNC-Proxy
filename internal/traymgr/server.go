package traymgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Notification struct {
	Title   string    `json:"title"`
	Message string    `json:"message"`
	Level   string    `json:"level,omitempty"`
	Time    time.Time `json:"time"`
}

type Notifier interface {
	Notify(Notification) error
}

type Server struct {
	configPath string
	supervisor *Supervisor
	notifier   Notifier
	restartCh  chan struct{}
	restartLag time.Duration

	mu            sync.Mutex
	notifications []Notification
	startedAt     time.Time
	restartedAt   time.Time
}

func NewServer(configPath string, supervisor *Supervisor, notifier Notifier) *Server {
	now := time.Now()
	return &Server{configPath: configPath, supervisor: supervisor, notifier: notifier, restartCh: make(chan struct{}, 1), restartLag: 100 * time.Millisecond, startedAt: now, restartedAt: now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/status", s.withAuth(s.status))
	mux.HandleFunc("GET /api/config", s.withAuth(s.getConfig))
	mux.HandleFunc("PUT /api/config", s.withAuth(s.putConfig))
	mux.HandleFunc("PUT /api/manager/config", s.withAuth(s.putManagerConfig))
	mux.HandleFunc("POST /api/manager/restart", s.withAuth(s.restartManager))
	mux.HandleFunc("PUT /api/proxy/config", s.withAuth(s.putProxyConfig))
	mux.HandleFunc("POST /api/proxy/start", s.withAuth(s.startProxy))
	mux.HandleFunc("POST /api/proxy/stop", s.withAuth(s.stopProxy))
	mux.HandleFunc("POST /api/proxy/restart", s.withAuth(s.restartProxy))
	mux.HandleFunc("POST /api/proxy/build", s.withAuth(s.buildProxy))
	mux.HandleFunc("POST /api/notify", s.withAuth(s.notify))
	mux.HandleFunc("POST /api/deploy", s.withAuth(s.deploy))
	return mux
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

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
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
	res, err := s.supervisor.DeployZip(ctx, tmpName, restart)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		s.addNotification(Notification{Title: "CNC Proxy", Message: "Deployment failed: " + err.Error(), Level: "error", Time: time.Now()})
		s.writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "result": res})
		return
	}
	msg := "Deployment completed"
	if res.Restarted {
		msg += "; proxy restarted"
	}
	s.addNotification(Notification{Title: "CNC Proxy", Message: msg, Level: "success", Time: time.Now()})
	s.writeJSON(w, map[string]any{"ok": true, "result": res})
}

func (s *Server) addNotification(n Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, n)
	if len(s.notifications) > 50 {
		s.notifications = append([]Notification(nil), s.notifications[len(s.notifications)-50:]...)
	}
}

func (s *Server) recentNotifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.notifications...)
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
