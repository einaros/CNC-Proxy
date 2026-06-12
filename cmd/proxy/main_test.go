package main

import "testing"

func TestIsLoopbackBind(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{addr: "localhost:8420", want: true},
		{addr: "127.0.0.1:8420", want: true},
		{addr: "[::1]:8420", want: true},
		{addr: ":8420", want: false},
		{addr: "0.0.0.0:8420", want: false},
		{addr: "[::]:8420", want: false},
		{addr: "192.168.1.20:8420", want: false},
		{addr: "bad-addr", want: false},
	} {
		if got := isLoopbackBind(tc.addr); got != tc.want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestValidateHTTPExposure(t *testing.T) {
	if err := validateHTTPExposure("127.0.0.1:8420", "localhost:8421", "", false); err != nil {
		t.Fatalf("loopback without token: %v", err)
	}
	if err := validateHTTPExposure(":8420", "127.0.0.1:8421", "", false); err == nil {
		t.Fatal("wildcard API bind without token should fail")
	}
	if err := validateHTTPExposure("127.0.0.1:8420", "0.0.0.0:8421", "", false); err == nil {
		t.Fatal("wildcard WebDAV bind without token should fail")
	}
	if err := validateHTTPExposure(":8420", "0.0.0.0:8421", "secret", false); err != nil {
		t.Fatalf("token should allow non-loopback bind: %v", err)
	}
	if err := validateHTTPExposure(":8420", "0.0.0.0:8421", "", true); err != nil {
		t.Fatalf("explicit insecure override should allow non-loopback bind: %v", err)
	}
}
