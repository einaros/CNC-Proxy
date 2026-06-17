package traymgr

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/uwin/cnc-proxy/internal/apiclient"
)

func TestUploadCompletionTrackerSkipsInitialSnapshot(t *testing.T) {
	var tracker UploadCompletionTracker
	got := tracker.Observe([]apiclient.Job{
		{ID: 1, Kind: "upload", Path: "/sd/gcodes/old.nc", State: "done"},
	})
	if len(got) != 0 {
		t.Fatalf("initial completed uploads = %+v, want none", got)
	}
}

func TestUploadCompletionTrackerReportsUploadTransitionToDone(t *testing.T) {
	var tracker UploadCompletionTracker
	tracker.Observe([]apiclient.Job{
		{ID: 1, Kind: "upload", Path: "/sd/gcodes/a.nc", State: "queued"},
		{ID: 2, Kind: "delete", Path: "/sd/gcodes/b.nc", State: "queued"},
	})

	got := tracker.Observe([]apiclient.Job{
		{ID: 1, Kind: "upload", Path: "/sd/gcodes/a.nc", State: "done"},
		{ID: 2, Kind: "delete", Path: "/sd/gcodes/b.nc", State: "done"},
	})
	if len(got) != 1 || got[0].ID != 1 || got[0].Path != "/sd/gcodes/a.nc" {
		t.Fatalf("completed uploads = %+v, want only upload job 1", got)
	}
	if again := tracker.Observe([]apiclient.Job{{ID: 1, Kind: "upload", Path: "/sd/gcodes/a.nc", State: "done"}}); len(again) != 0 {
		t.Fatalf("second completed uploads = %+v, want none", again)
	}
}

func TestUploadCompletionTrackerReportsNewDoneUploadsAfterBaseline(t *testing.T) {
	var tracker UploadCompletionTracker
	tracker.Observe(nil)

	got := tracker.Observe([]apiclient.Job{
		{ID: 3, Kind: "upload", Path: "/sd/gcodes/new.nc", State: "done"},
	})
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("completed uploads = %+v, want new done upload", got)
	}
}

func TestWebDAVVisibleDeletionTrackerSkipsInitialSnapshot(t *testing.T) {
	var tracker WebDAVVisibleDeletionTracker
	got := tracker.Observe([]apiclient.File{{Path: "/sd/gcodes/a.nc", Sync: "synced"}})
	if len(got) != 0 {
		t.Fatalf("initial deleted files = %+v, want none", got)
	}
}

func TestWebDAVVisibleDeletionTrackerReportsHiddenOrMissingPaths(t *testing.T) {
	var tracker WebDAVVisibleDeletionTracker
	tracker.Observe([]apiclient.File{
		{Path: "/sd/gcodes/a.nc", Sync: "synced"},
		{Path: "/sd/gcodes/b.nc", Sync: "remote_only"},
		{Path: "/sd/gcodes/c.nc", Sync: "synced"},
	})

	got := tracker.Observe([]apiclient.File{
		{Path: "/sd/gcodes/a.nc", Sync: "pending_delete"},
		{Path: "/sd/gcodes/c.nc", Sync: "synced"},
	})
	if len(got) != 2 || got[0] != "/sd/gcodes/a.nc" || got[1] != "/sd/gcodes/b.nc" {
		t.Fatalf("deleted files = %+v, want a.nc and b.nc", got)
	}
	if again := tracker.Observe([]apiclient.File{{Path: "/sd/gcodes/c.nc", Sync: "synced"}}); len(again) != 0 {
		t.Fatalf("second deleted files = %+v, want none", again)
	}
}

func TestNotifyUploadCompletedRecordsAndSendsNotification(t *testing.T) {
	rec := &recordingNotifier{}
	app, err := NewApp(filepath.Join(t.TempDir(), "tray.json"), rec)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	app.NotifyUploadCompleted("/sd/gcodes/part.nc")

	got := app.Server.recentNotifications()
	if len(got) != 1 {
		t.Fatalf("notifications = %+v, want one", got)
	}
	if got[0].Level != "success" || got[0].Message != "Uploaded part.nc to the machine" {
		t.Fatalf("notification = %+v", got[0])
	}
	if len(rec.got) != 1 || rec.got[0].Message != got[0].Message {
		t.Fatalf("notifier got %+v", rec.got)
	}
}

func TestNotifyWebDAVRefreshAfterDeleteRecordsReason(t *testing.T) {
	rec := &recordingNotifier{}
	app, err := NewApp(filepath.Join(t.TempDir(), "tray.json"), rec)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	app.NotifyWebDAVRefreshAfterDelete([]string{"/sd/gcodes/part.nc"})

	got := app.Server.recentNotifications()
	if len(got) != 1 {
		t.Fatalf("notifications = %+v, want one", got)
	}
	if got[0].Level != "info" || got[0].Message != "Refreshing WebDAV mount after web delete: part.nc" {
		t.Fatalf("notification = %+v", got[0])
	}
	if len(rec.got) != 1 || rec.got[0].Message != got[0].Message {
		t.Fatalf("notifier got %+v", rec.got)
	}
	logText := managerLogText(app.Server.recentManagerLog())
	if !strings.Contains(logText, "Refreshing WebDAV mount after web delete: part.nc") {
		t.Fatalf("manager log = %q", logText)
	}
}
