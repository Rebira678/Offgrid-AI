package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestServeAttachmentEdgeCasesAndSecurityHeaders(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "pump")

	// 1. Image attachment (inline)
	jpg := jpegData(2000, 1)
	attJpg, err := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(jpg), int64(len(jpg)), "image/jpeg", "inspect.jpg")
	if err != nil {
		t.Fatal(err)
	}

	code, h, body := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+attJpg.Hash, "", nil)
	if code != 200 || body != string(jpg) {
		t.Fatalf("code=%d body len=%d", code, len(body))
	}
	if csp := h.Get("Content-Security-Policy"); csp != "default-src 'none'; sandbox; frame-ancestors 'none'" {
		t.Fatalf("unexpected CSP: %q", csp)
	}
	if nosniff := h.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("expected nosniff, got %q", nosniff)
	}
	if disp := h.Get("Content-Disposition"); !strings.HasPrefix(disp, "inline") || !strings.Contains(disp, "inspect.jpg") {
		t.Fatalf("expected inline disposition, got %q", disp)
	}
	if cache := h.Get("Cache-Control"); !strings.Contains(cache, "max-age=") {
		t.Fatalf("expected Cache-Control, got %q", cache)
	}

	// 2. PDF attachment (attachment download, not inline)
	pdfData := []byte("%PDF-1.4 sample pdf content")
	attPdf, err := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(pdfData), int64(len(pdfData)), "application/pdf", "manual.pdf")
	if err != nil {
		t.Fatal(err)
	}

	code, h, body = r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+attPdf.Hash, "", nil)
	if code != 200 || body != string(pdfData) {
		t.Fatalf("pdf: code=%d", code)
	}
	if disp := h.Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment") || !strings.Contains(disp, "manual.pdf") {
		t.Fatalf("expected attachment disposition for pdf, got %q", disp)
	}

	// 3. Range request (Partial Content 206)
	code, _, body = r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+attJpg.Hash, "", map[string]string{"Range": "bytes=0-99"})
	if code != 206 || len(body) != 100 {
		t.Fatalf("expected 206 Partial Content with 100 bytes, got %d and len %d", code, len(body))
	}

	// 4. Missing attachment on existing task -> 404
	code, _, body = r.do("GET", "/api/tasks/"+tk.ID+"/attachments/0000000000000000000000000000000000000000000000000000000000000000", "", nil)
	if code != 404 || !strings.Contains(body, "no such attachment") {
		t.Fatalf("missing attachment: code=%d body=%q", code, body)
	}

	// 5. Non-existent task ID -> 404
	code, _, body = r.do("GET", "/api/tasks/nonexistent/attachments/"+attJpg.Hash, "", nil)
	if code != 404 {
		t.Fatalf("missing task: code=%d body=%q", code, body)
	}

	// 6. Attachment metadata exists but bytes not present on device -> 404 with helpful message
	c := &clk{t0}
	stPeer := newStore(t, "peer", c)
	pull(t, stPeer, r.st) // peer pulls the op metadata but doesn't have the blob bytes yet
	rigPeer := newRig(t, func(cfg *Config) { cfg.Store = stPeer; cfg.Now = c.now })

	code, _, body = rigPeer.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+attJpg.Hash, "", nil)
	if code != 404 || !strings.Contains(body, "this file has not reached this device yet") {
		t.Fatalf("expected not-yet-reached message, got code=%d body=%q", code, body)
	}
}

func TestServeAttachmentSecurityDispositionsAndNames(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	tk := mk(t, r.st, "unicode-names")

	jpg := jpegData(1000, 2)
	att, err := r.st.AddAttachment(ctx, tk.ID, bytes.NewReader(jpg), int64(len(jpg)), "image/jpeg", "résumé_report.jpg")
	if err != nil {
		t.Fatal(err)
	}

	code, h, _ := r.do("GET", "/api/tasks/"+tk.ID+"/attachments/"+att.Hash, "", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	cd := h.Get("Content-Disposition")
	if !strings.Contains(cd, "report.jpg") {
		t.Fatalf("expected filename preserved in Content-Disposition, got %q", cd)
	}
}

type dummyStore struct{ Store }

func TestServeAttachmentStoreWithoutAttachments(t *testing.T) {
	r := newRig(t, func(cfg *Config) {
		cfg.Store = dummyStore{}
	})
	code, _, body := r.do("GET", "/api/tasks/1/attachments/0000000000000000000000000000000000000000000000000000000000000000", "", nil)
	if code != 404 || !strings.Contains(body, "this store keeps no attachments") {
		t.Fatalf("expected 404 with keeps no attachments, got %d %s", code, body)
	}
}
