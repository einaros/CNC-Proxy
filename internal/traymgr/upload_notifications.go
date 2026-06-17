package traymgr

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uwin/cnc-proxy/internal/apiclient"
)

type CompletedUpload struct {
	ID   int64
	Path string
}

type UploadCompletionTracker struct {
	initialized bool
	seen        map[int64]string
}

func (t *UploadCompletionTracker) Observe(jobs []apiclient.Job) []CompletedUpload {
	current := make(map[int64]string, len(jobs))
	var completed []CompletedUpload
	for _, job := range jobs {
		state := strings.ToLower(job.State)
		current[job.ID] = state
		if !t.initialized {
			continue
		}
		if strings.ToLower(job.Kind) != "upload" || state != "done" {
			continue
		}
		if t.seen[job.ID] != "done" {
			completed = append(completed, CompletedUpload{ID: job.ID, Path: job.Path})
		}
	}
	t.seen = current
	t.initialized = true
	return completed
}

type WebDAVVisibleDeletionTracker struct {
	initialized bool
	visible     map[string]bool
}

func (t *WebDAVVisibleDeletionTracker) Observe(files []apiclient.File) []string {
	current := make(map[string]bool, len(files))
	for _, f := range files {
		if webDAVVisibleFile(f) {
			current[f.Path] = true
		}
	}
	var deleted []string
	if t.initialized {
		for path := range t.visible {
			if !current[path] {
				deleted = append(deleted, path)
			}
		}
		sort.Strings(deleted)
	}
	t.visible = current
	t.initialized = true
	return deleted
}

func webDAVVisibleFile(f apiclient.File) bool {
	if strings.TrimSpace(f.Path) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(f.Sync)) {
	case "pending_delete", "deleting":
		return false
	default:
		return true
	}
}

func (a *App) NotifyUploadCompleted(path string) {
	if a == nil || a.Server == nil {
		return
	}
	n := Notification{Title: "CNC Proxy", Message: uploadCompletedMessage(path), Level: "success", Time: time.Now()}
	a.Server.addNotification(n)
	if a.Server.notifier != nil {
		_ = a.Server.notifier.Notify(n)
	}
}

func uploadCompletedMessage(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "File finished uploading to the machine"
	}
	name := filepath.Base(path)
	if name == "." || name == "/" || name == "" {
		return "File finished uploading to the machine"
	}
	return "Uploaded " + name + " to the machine"
}

func (a *App) NotifyWebDAVRefreshAfterDelete(paths []string) {
	if a == nil || a.Server == nil || len(paths) == 0 {
		return
	}
	msg := "Refreshing WebDAV mount after web delete"
	if suffix := deletedPathsSummary(paths); suffix != "" {
		msg += ": " + suffix
	}
	a.logWebDAVOperation("info", msg)
	n := Notification{Title: "CNC Proxy", Message: msg, Level: "info", Time: time.Now()}
	a.Server.addNotification(n)
	if a.Server.notifier != nil {
		_ = a.Server.notifier.Notify(n)
	}
}

func deletedPathsSummary(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		name := filepath.Base(strings.TrimSpace(p))
		if name == "." || name == "/" || name == "" {
			name = strings.TrimSpace(p)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) == 1 {
		return names[0]
	}
	return names[0] + " and " + strconv.Itoa(len(names)-1) + " more"
}
