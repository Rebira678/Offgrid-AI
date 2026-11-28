package api

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"offgrid/internal/task"
)

func TestAttachmentUploadAndDownloadEdgeCases(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("task with attachments", "site", task.PriorityLow)

	// 1. Upload zero bytes -> 400
	rp := r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", []byte{}, map[string]string{
		ActionHeader:   "1",
		"Content-Type": "image/jpeg",
	})
	if rp.code != 400 {
		t.Fatalf("expected 400 for zero byte attachment, got %d: %s", rp.code, rp.body)
	}

	// 2. Upload to nonexistent task -> 404
	jpegData := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{1}, 50)...)
	rp = r.do("POST", "/v1/tasks/nonexistent_task_id/attachments", jpegData, map[string]string{
		ActionHeader:   "1",
		"Content-Type": "image/jpeg",
	})
	if rp.code != 404 {
		t.Fatalf("expected 404 for nonexistent task, got %d: %s", rp.code, rp.body)
	}

	// 3. Get nonexistent attachment hash -> 404
	rp = r.do("GET", "/v1/tasks/"+tk.ID+"/attachments/0000000000000000000000000000000000000000000000000000000000000000", nil, nil)
	if rp.code != 404 {
		t.Fatalf("expected 404 for nonexistent hash, got %d: %s", rp.code, rp.body)
	}

	// 4. Upload valid attachment with URL-encoded special characters in filename
	encodedName := url.PathEscape("photo [site#1] & pump.jpg")
	rp = r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", jpegData, map[string]string{
		ActionHeader:     "1",
		"Content-Type":   "image/jpeg",
		"X-Offgrid-Name": encodedName,
	})
	if rp.code != 201 {
		t.Fatalf("expected 201, got %d: %s", rp.code, rp.body)
	}
	var att Attachment
	rp.into(t, &att)
	if att.Name != "photo [site#1] & pump.jpg" {
		t.Fatalf("expected decoded name 'photo [site#1] & pump.jpg', got %q", att.Name)
	}

	// 5. Verify security headers on attachment download
	dl := r.do("GET", "/v1/tasks/"+tk.ID+"/attachments/"+att.Hash, nil, nil)
	if dl.code != 200 {
		t.Fatalf("expected 200 on download, got %d", dl.code)
	}
	csp := dl.h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("missing sandbox in CSP: %q", csp)
	}
	if ct := dl.h.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("unexpected Content-Type: %q", ct)
	}
}
