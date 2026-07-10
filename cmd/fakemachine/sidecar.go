package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	staticassets "github.com/uwin/cnc-proxy/assets"
	"github.com/uwin/cnc-proxy/internal/carveratest"
)

//go:embed web/index.html web/app.js web/geometry.mjs web/loaders/GLTFLoader.js web/three.module.min.js web/utils/BufferGeometryUtils.js
var sidecarWeb embed.FS

func startSidecar(addr string, m *carveratest.FakeMachine) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           sidecarHandler(m),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("fakemachine sidecar error: %v", err)
		}
	}()
	return srv, ln, nil
}

func sidecarHandler(m *carveratest.FakeMachine) http.Handler {
	sub, err := fs.Sub(sidecarWeb, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(m.Snapshot()); err != nil {
			log.Printf("encode snapshot: %v", err)
		}
	})
	mux.HandleFunc("/api/tool/insert", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		kind, err := readToolInsertKind(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tool, err := m.InsertTool(kind)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(tool); err != nil {
			log.Printf("encode inserted tool: %v", err)
		}
	})
	mux.HandleFunc("/api/tool/lock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		locked, err := readToolLockRequest(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tool, err := m.SetInsertedToolSpindleLocked(locked)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(tool); err != nil {
			log.Printf("encode tool lock: %v", err)
		}
	})
	mux.HandleFunc("/api/tool/stickout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		stickout, err := readToolStickoutRequest(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tool, err := m.SetInsertedToolStickout(stickout)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "locked") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(tool); err != nil {
			log.Printf("encode tool stickout: %v", err)
		}
	})
	mux.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		switch r.Method {
		case http.MethodPost:
			name, data, err := readModelUpload(w, r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := m.LoadProbeModel(name, data); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(m.Snapshot().ProbeModel); err != nil {
				log.Printf("encode probe model: %v", err)
			}
		case http.MethodDelete:
			m.ClearProbeModel()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/model/mesh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		mesh, ok := m.ProbeModelMesh()
		if !ok {
			http.Error(w, "no probe model loaded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(mesh); err != nil {
			log.Printf("encode probe model mesh: %v", err)
		}
	})
	mux.HandleFunc("/api/model/placement", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		update, err := readModelPlacementRequest(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		model, err := m.SetModelPlacement(update)
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "not idle") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(model); err != nil {
			log.Printf("encode model placement: %v", err)
		}
	})
	mux.HandleFunc("/api/simulation/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		update, err := readSimulationSettingsRequest(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sim, err := m.UpdateSimulationSettings(update)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sim); err != nil {
			log.Printf("encode simulation settings: %v", err)
		}
	})
	mux.HandleFunc("/api/simulation/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		sim, err := m.ResetStock()
		if err != nil {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "not idle") {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sim); err != nil {
			log.Printf("encode simulation reset: %v", err)
		}
	})
	mux.HandleFunc("/api/simulation/stock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		stock, ok := m.StockState()
		if !ok {
			http.Error(w, "no stock loaded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(stock); err != nil {
			log.Printf("encode stock: %v", err)
		}
	})
	mux.HandleFunc("/api/simulation/stock.stl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		noStore(w)
		data, ok := m.StockSTL()
		if !ok {
			http.Error(w, "no stock loaded", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "model/stl")
		w.Header().Set("Content-Disposition", `attachment; filename="fakemachine-end-stock.stl"`)
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		noStore(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		writeSnapshotEvent(w, m)
		flusher.Flush()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				writeSnapshotEvent(w, m)
				flusher.Flush()
			}
		}
	})
	mux.Handle("/assets/", noStoreHandler(http.StripPrefix("/assets/", http.FileServer(http.FS(staticassets.FS)))))
	mux.Handle("/app.js", noStoreHandler(files))
	mux.Handle("/geometry.mjs", noStoreHandler(files))
	mux.Handle("/loaders/", noStoreHandler(files))
	mux.Handle("/three.module.min.js", noStoreHandler(files))
	mux.Handle("/utils/", noStoreHandler(files))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		if r.URL.Path == "/" {
			http.ServeFileFS(w, r, sub, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
	return mux
}

func readToolLockRequest(w http.ResponseWriter, r *http.Request) (bool, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	var req struct {
		Locked *bool `json:"locked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return false, err
	}
	if req.Locked == nil {
		return false, errors.New("locked is required")
	}
	return *req.Locked, nil
}

func readToolStickoutRequest(w http.ResponseWriter, r *http.Request) (float64, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	var req struct {
		StickoutMM float64 `json:"stickout_mm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return 0, err
	}
	return req.StickoutMM, nil
}

func readToolInsertKind(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") || ct == "" {
		var req struct {
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return "", err
		}
		if strings.TrimSpace(req.Kind) == "" {
			return "", errors.New("kind is required")
		}
		return req.Kind, nil
	}
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return "", err
		}
		kind := strings.TrimSpace(r.Form.Get("kind"))
		if kind == "" {
			return "", errors.New("kind is required")
		}
		return kind, nil
	}
	return "", errors.New("unsupported content type")
}

func readSimulationSettingsRequest(w http.ResponseWriter, r *http.Request) (carveratest.SimulationSettingsUpdate, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	defer r.Body.Close()
	var req carveratest.SimulationSettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return carveratest.SimulationSettingsUpdate{}, err
	}
	return req, nil
}

func readModelPlacementRequest(w http.ResponseWriter, r *http.Request) (carveratest.ModelPlacementUpdate, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	defer r.Body.Close()
	var req carveratest.ModelPlacementUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return carveratest.ModelPlacementUpdate{}, err
	}
	return req, nil
}

func readModelUpload(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return "", nil, err
		}
		file, hdr, err := r.FormFile("model")
		if err != nil {
			return "", nil, err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return "", nil, err
		}
		name := filepath.Base(hdr.Filename)
		if name == "." || name == "/" || name == "" {
			name = "model"
		}
		return name, data, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", nil, err
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "." || name == "/" || name == "" {
		name = "model"
	}
	return name, data, nil
}

func writeSnapshotEvent(w http.ResponseWriter, m *carveratest.FakeMachine) {
	b, err := json.Marshal(m.Snapshot())
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: state\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

func noStoreHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		next.ServeHTTP(w, r)
	})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func sidecarURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "::" || strings.HasPrefix(host, "0.0.0.0") {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
