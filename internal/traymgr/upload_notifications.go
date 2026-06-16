package traymgr

import (
	"path/filepath"
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
