//go:build windows

package traymgr

import (
	"context"
	"slices"
	"testing"
)

func TestWindowsWebDAVRemotesIncludesURLAndLocalhostFallbacks(t *testing.T) {
	got, err := windowsWebDAVRemotes("http://127.0.0.1:8421/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"http://127.0.0.1:8421/",
		"http://127.0.0.1:8421",
		"http://localhost:8421/",
		"http://localhost:8421",
		`\\localhost@8421\DavWWWRoot`,
		`\\127.0.0.1@8421\DavWWWRoot`,
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("windowsWebDAVRemotes missing %q in %v", want, got)
		}
	}
}

func TestWindowsWebDAVRemotesIncludesManagerProxyPathForms(t *testing.T) {
	got, err := windowsWebDAVRemotes("http://127.0.0.1:8430/webdav/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"http://127.0.0.1:8430/webdav/",
		"http://127.0.0.1:8430/webdav",
		"http://localhost:8430/webdav/",
		"http://localhost:8430/webdav",
		`\\localhost@8430\DavWWWRoot\webdav`,
		`\\localhost@8430\webdav`,
		`\\127.0.0.1@8430\DavWWWRoot\webdav`,
		`\\127.0.0.1@8430\webdav`,
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("windowsWebDAVRemotes missing %q in %v", want, got)
		}
	}
	for _, wrong := range []string{
		`\\localhost@8430\DavWWWRoot`,
		`\\127.0.0.1@8430\DavWWWRoot`,
	} {
		if slices.Contains(got, wrong) {
			t.Fatalf("windowsWebDAVRemotes included root mapping %q in %v", wrong, got)
		}
	}
}

func TestParseWindowsDriveUseExtractsRemoteName(t *testing.T) {
	got := parseWindowsDriveUse("Local name        Z:\r\nRemote name       \\\\127.0.0.1@8430\\DavWWWRoot\r\nStatus            OK\r\n")
	if !got.Mapped {
		t.Fatal("Mapped = false, want true")
	}
	if got.Remote != `\\127.0.0.1@8430\DavWWWRoot` {
		t.Fatalf("Remote = %q", got.Remote)
	}
	if got.Status != "OK" {
		t.Fatalf("Status = %q, want OK", got.Status)
	}
}

func TestParseWindowsDriveUseNoEntriesIsUnmounted(t *testing.T) {
	got := parseWindowsDriveUse("There are no entries in the list.")
	if got.Mapped || got.Remote != "" {
		t.Fatalf("parseWindowsDriveUse = %+v, want empty", got)
	}
}

func TestParseWindowsAllDriveUses(t *testing.T) {
	got := parseWindowsAllDriveUses(`
Status       Local     Remote                    Network
-------------------------------------------------------------------------------
OK           Y:        \\localhost@8430\DavWWWRoot\webdav
             X:        \\localhost@8430\webdav   Web Client Network
Disconnected Z:        \\fileserver\share        Microsoft Windows Network
`)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	if got[0].Local != "Y:" || got[0].Remote != `\\localhost@8430\DavWWWRoot\webdav` {
		t.Fatalf("first mapping = %+v", got[0])
	}
	if got[0].Status != "OK" {
		t.Fatalf("first status = %q, want OK", got[0].Status)
	}
	if got[1].Local != "X:" || got[1].Remote != `\\localhost@8430\webdav` {
		t.Fatalf("second mapping = %+v", got[1])
	}
	if got[1].Status != "" {
		t.Fatalf("second status = %q, want empty", got[1].Status)
	}
	if got[2].Local != "Z:" || got[2].Remote != `\\fileserver\share` {
		t.Fatalf("third mapping = %+v", got[2])
	}
	if got[2].Status != "Disconnected" {
		t.Fatalf("third status = %q, want Disconnected", got[2].Status)
	}
}

func TestWindowsUseStatusOKAllowsBlankConnectedWebDAVStatus(t *testing.T) {
	if !windowsUseStatusOK("") {
		t.Fatal("blank WebDAV status should be accepted")
	}
	if !windowsUseStatusOK("OK") {
		t.Fatal("OK status should be accepted")
	}
	if windowsUseStatusOK("Disconnected") {
		t.Fatal("Disconnected status should be rejected")
	}
}

func TestWindowsMappingUsableRequiresLocalDrive(t *testing.T) {
	remotes := []string{`\\localhost@8430\DavWWWRoot\webdav`}
	if ok, err := windowsMappingUsable(context.Background(), windowsDriveUse{Mapped: true, Status: "OK", Remote: remotes[0]}, remotes); err != nil || ok {
		t.Fatal("mapping without local drive should not be usable")
	}
	if ok, err := windowsMappingUsable(context.Background(), windowsDriveUse{Mapped: true, Status: "OK", Local: "Y:", Remote: ""}, remotes); err != nil || ok {
		t.Fatal("mapping without remote should not be usable")
	}
	if ok, err := windowsMappingUsable(context.Background(), windowsDriveUse{Mapped: false, Status: "OK", Local: "Y:", Remote: remotes[0]}, remotes); err != nil || ok {
		t.Fatal("unmapped drive should not be usable")
	}
	if ok, err := windowsMappingUsable(context.Background(), windowsDriveUse{Mapped: true, Status: "Disconnected", Local: "Y:", Remote: remotes[0]}, remotes); err != nil || ok {
		t.Fatal("disconnected mapping should not be usable")
	}
}

func TestWindowsDriveMatchesConfiguredDrive(t *testing.T) {
	if !windowsDriveMatches("Z:", "Z:") {
		t.Fatal("matching drive was rejected")
	}
	if !windowsDriveMatches("Z:", "*") {
		t.Fatal("wildcard drive should match")
	}
	if windowsDriveMatches("Y:", "Z:") {
		t.Fatal("wrong drive should not match")
	}
}

func TestWindowsRemoteMatchesRequiresExactRemote(t *testing.T) {
	remotes := []string{`\\localhost@8430\DavWWWRoot\webdav`}
	if !windowsRemoteMatches(`\\localhost@8430\DavWWWRoot\webdav\`, remotes) {
		t.Fatal("remote with trailing slash should match")
	}
	if windowsRemoteMatches(`\\localhost@8430\DavWWWRoot`, remotes) {
		t.Fatal("manager WebDAV root should not match the /webdav file view")
	}
	if windowsRemoteMatches(`\\localhost@8430\DavWWWRoot\webdav-other`, remotes) {
		t.Fatal("substring remote should not match")
	}
}

func TestWindowsRemoteLooksLikeCNCWebDAV(t *testing.T) {
	for _, remote := range []string{
		`\\127.0.0.1@8421\DavWWWRoot`,
		`\\localhost@8430\DavWWWRoot\webdav`,
		`http://127.0.0.1:8430/webdav/`,
	} {
		if !windowsRemoteLooksLikeCNCWebDAV(remote) {
			t.Fatalf("windowsRemoteLooksLikeCNCWebDAV(%q) = false", remote)
		}
	}
	if windowsRemoteLooksLikeCNCWebDAV(`\\fileserver\share`) {
		t.Fatal("fileserver share should not look like CNC WebDAV")
	}
}
