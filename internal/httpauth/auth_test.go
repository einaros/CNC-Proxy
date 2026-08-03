package httpauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareDisabledAllowsRequests(t *testing.T) {
	h := Middleware(Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestMiddlewareBasicAuth(t *testing.T) {
	h := Middleware(Config{User: "operator", Token: "secret"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name string
		user string
		pass string
		want int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong token", user: "operator", pass: "bad", want: http.StatusUnauthorized},
		{name: "wrong user", user: "other", pass: "secret", want: http.StatusUnauthorized},
		{name: "correct", user: "operator", pass: "secret", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
			if tc.user != "" || tc.pass != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				body, _ := io.ReadAll(rec.Result().Body)
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, body)
			}
		})
	}
}

func TestMiddlewareSessionCookieAuthorizesBackgroundRequests(t *testing.T) {
	cfg := Config{User: "operator", Token: "secret", SuppressAPIChallenge: true}
	served := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusNoContent)
	})
	login := Middleware(cfg, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("operator", "secret")
	rec := httptest.NewRecorder()
	login.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want 204", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookieName || cookie.Value == "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("session cookie = %+v", cookie)
	}

	// API and WebDAV use separate middleware instances. The signed cookie must
	// remain valid across both listeners because browser cookies are host-wide.
	background := Middleware(cfg, inner)
	req = httptest.NewRequest(http.MethodGet, "/api/events?scope=control", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	background.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || served != 2 {
		t.Fatalf("cookie request status=%d served=%d, want 204/2", rec.Code, served)
	}

	tampered := *cookie
	tampered.Value += "x"
	req = httptest.NewRequest(http.MethodGet, "/api/events?scope=control", nil)
	req.AddCookie(&tampered)
	rec = httptest.NewRecorder()
	background.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || served != 2 {
		t.Fatalf("tampered cookie status=%d served=%d, want 401/2", rec.Code, served)
	}
}

func TestMiddlewareSuppressesRepeatedAPIChallenges(t *testing.T) {
	h := Middleware(Config{User: "operator", Token: "secret", SuppressAPIChallenge: true}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/jog/ws", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("background challenge status=%d header=%q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("background Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("page challenge status=%d header=%q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMiddlewareHealthzBypassesAuth(t *testing.T) {
	h := Middleware(Config{User: "operator", Token: "secret"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not serve healthz")
	}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz = status %d body %q", rec.Code, rec.Body.String())
	}
}
