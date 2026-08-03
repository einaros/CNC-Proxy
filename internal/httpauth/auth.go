// Package httpauth provides the proxy's small HTTP authentication wrapper.
package httpauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

const (
	defaultRealm      = "cnc-proxy"
	sessionCookieName = "cnc_proxy_session"
)

// Config controls Basic Auth for HTTP-facing proxy surfaces.
type Config struct {
	User                 string
	Token                string
	Realm                string
	SuppressAPIChallenge bool
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
		if !cfg.Enabled() {
			h.ServeHTTP(w, r)
			return
		}
		if authorizedSession(cfg, r) {
			h.ServeHTTP(w, r)
			return
		}
		if authorized(cfg, r) {
			setSessionCookie(w, cfg, r)
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if !cfg.SuppressAPIChallenge || !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+cfg.Realm+`", charset="UTF-8"`)
		}
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

func sessionValue(cfg Config) string {
	mac := hmac.New(sha256.New, []byte(cfg.Token))
	_, _ = mac.Write([]byte("cnc-proxy-session-v1\x00" + cfg.Realm + "\x00" + cfg.User))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func authorizedSession(cfg Config, r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	want := sessionValue(cfg)
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(want)) == 1
}

func setSessionCookie(w http.ResponseWriter, cfg Config, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionValue(cfg),
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}
