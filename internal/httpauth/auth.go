// Package httpauth provides the proxy's small HTTP authentication wrapper.
package httpauth

import (
	"crypto/subtle"
	"net/http"
)

const defaultRealm = "cnc-proxy"

// Config controls Basic Auth for HTTP-facing proxy surfaces.
type Config struct {
	User  string
	Token string
	Realm string
}

// Enabled reports whether requests should be authenticated.
func (c Config) Enabled() bool { return c.Token != "" }

// Middleware wraps h with an unauthenticated /healthz endpoint and, when Token
// is set, HTTP Basic Auth for every other path.
func Middleware(cfg Config, h http.Handler) http.Handler {
	if cfg.Realm == "" {
		cfg.Realm = defaultRealm
	}
	if cfg.User == "" {
		cfg.User = "cnc"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		if !cfg.Enabled() || authorized(cfg, r) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="`+cfg.Realm+`", charset="UTF-8"`)
		http.Error(w, "unauthorized\n", http.StatusUnauthorized)
	})
}

func authorized(cfg Config, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.User)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.Token)) == 1
	return userOK && passOK
}
