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
