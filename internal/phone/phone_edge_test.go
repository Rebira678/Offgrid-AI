package phone_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func TestPairingCodeEdgeCases(t *testing.T) {
	ctx := context.Background()
	baseTime := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	now := func() time.Time { return currentTime }

	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), now)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}

	code, err := sessions.CreatePairingCode(ctx, "Test Phone", 10*time.Minute)
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	// 1. Malformed code variations
	malformed := []string{
		"",
		"12",
		"12345",
		"1234567",
		"abcdef",
		"12-34-56",
		"   ",
		"123456\x00",
		"123-456-789",
	}
	for _, m := range malformed {
		if _, err := sessions.RedeemPairingCode(ctx, m, "client"); err != phone.ErrCodeInvalid {
			t.Errorf("expected ErrCodeInvalid for %q, got %v", m, err)
		}
	}

	// 2. Normalization: spaces and hyphenated codes match
	codeNoHyphen := strings.ReplaceAll(code.Code, "-", "")
	codeSpaces := " " + codeNoHyphen[:3] + " " + codeNoHyphen[3:] + " "

	if phone.NormalizeCode(codeSpaces) != codeNoHyphen {
		t.Errorf("NormalizeCode mismatch: got %q, want %q", phone.NormalizeCode(codeSpaces), codeNoHyphen)
	}

	// 3. Exact expiration boundary: 1ns before expiry succeeds, 1ns after expiry fails
	codeBoundary, _ := sessions.CreatePairingCode(ctx, "Boundary Phone", 5*time.Minute)
	// Set clock to 1ns before expiry
	currentTime = baseTime.Add(5*time.Minute - 1*time.Nanosecond)
	sess1, err := sessions.RedeemPairingCode(ctx, codeBoundary.Code, "Boundary Phone Client")
	if err != nil || sess1 == nil {
		t.Errorf("expected success right before expiry, got: %v", err)
	}

	// Another code that expires
	currentTime = baseTime
	codeExpired, _ := sessions.CreatePairingCode(ctx, "Expired Phone", 5*time.Minute)
	currentTime = baseTime.Add(5*time.Minute + 1*time.Second)
	if _, err := sessions.RedeemPairingCode(ctx, codeExpired.Code, "Expired Phone Client"); err != phone.ErrCodeExpired {
		t.Errorf("expected ErrCodeExpired after deadline, got: %v", err)
	}

	// 4. Simultaneous concurrent redemption of same code: only 1 must succeed
	targetCode, _ := sessions.CreatePairingCode(ctx, "Race Target", 15*time.Minute)
	const concurrentAttempts = 20
	var successes int64
	var errUsed int64
	var wg sync.WaitGroup

	for i := 0; i < concurrentAttempts; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			_, err := sessions.RedeemPairingCode(ctx, targetCode.Code, fmt.Sprintf("worker-%d", worker))
			if err == nil {
				atomic.AddInt64(&successes, 1)
			} else if err == phone.ErrCodeUsed {
				atomic.AddInt64(&errUsed, 1)
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Fatalf("race condition: expected exactly 1 successful redemption, got %d", successes)
	}
	if errUsed != concurrentAttempts-1 {
		t.Fatalf("expected %d ErrCodeUsed, got %d", concurrentAttempts-1, errUsed)
	}
}

func TestSessionRevocationAndPrefixMatching(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		t.Fatalf("new session store: %v", err)
	}

	code1, _ := sessions.CreatePairingCode(ctx, "Phone 1", 10*time.Minute)
	sess1, _ := sessions.RedeemPairingCode(ctx, code1.Code, "Client 1")

	code2, _ := sessions.CreatePairingCode(ctx, "Phone 2", 10*time.Minute)
	sess2, _ := sessions.RedeemPairingCode(ctx, code2.Code, "Client 2")

	// Revoke empty string should fail
	if _, err := sessions.RevokeSession(ctx, ""); err == nil {
		t.Errorf("expected error revoking empty string")
	}

	// Revoke by prefix (first 10 chars)
	revoked, err := sessions.RevokeSession(ctx, sess1.Token[:10])
	if err != nil || revoked != 1 {
		t.Errorf("expected 1 revoked session by prefix, got %d (err: %v)", revoked, err)
	}

	// Validating sess1 should now fail
	if _, err := sessions.ValidateSession(ctx, sess1.Token); err != phone.ErrSessionInvalid {
		t.Errorf("expected ErrSessionInvalid for sess1, got %v", err)
	}

	// Validating sess2 should still succeed
	if _, err := sessions.ValidateSession(ctx, sess2.Token); err != nil {
		t.Errorf("sess2 should still be valid, got %v", err)
	}

	// Revoke already revoked session: affected rows should be 0
	revokedAgain, err := sessions.RevokeSession(ctx, sess1.Token)
	if err != nil || revokedAgain != 0 {
		t.Errorf("expected 0 revoked rows for already-revoked session, got %d", revokedAgain)
	}
}

func TestReplayBatchEdgeCases(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	now := time.Now
	sess := &phone.Session{Token: "test-token", Label: "Edge Client"}

	// 1. Empty operations batch
	emptyBatch := phone.ReplayBatch{
		ClientID:   "Edge Client",
		Operations: []phone.QueuedOp{},
	}
	resEmpty := phone.ReplayQueuedOps(ctx, st, sess, emptyBatch, now)
	if resEmpty.Applied != 0 || resEmpty.Conflicts != 0 || resEmpty.Errors != 0 {
		t.Errorf("empty batch unexpected result: %+v", resEmpty)
	}

	// 2. Unknown actions
	unknownBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "bad-1", Action: "hack_database", ClientTime: now()},
			{OpID: "bad-2", Action: "", ClientTime: now()},
			{OpID: "bad-3", Action: "format_disk", ClientTime: now()},
		},
	}
	resUnknown := phone.ReplayQueuedOps(ctx, st, sess, unknownBatch, now)
	if resUnknown.Errors != 3 {
		t.Errorf("expected 3 errors for unknown actions, got %d", resUnknown.Errors)
	}

	// 3. Create task with whitespace / empty title
	emptyTitlePayload, _ := json.Marshal(phone.CreateTaskPayload{Title: "   "})
	badCreateBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "create-empty", Action: "create_task", ClientTime: now(), Payload: emptyTitlePayload},
		},
	}
	resBadCreate := phone.ReplayQueuedOps(ctx, st, sess, badCreateBatch, now)
	if resBadCreate.Errors != 1 || resBadCreate.Results[0].Status != "error" {
		t.Errorf("expected error for empty title create, got %+v", resBadCreate)
	}

	// 4. Update status with empty status
	emptyStatusPayload, _ := json.Marshal(phone.UpdateStatusPayload{Status: ""})
	badUpdBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "upd-empty", Action: "update_status", TaskID: "nonexistent", ClientTime: now(), Payload: emptyStatusPayload},
		},
	}
	resBadUpd := phone.ReplayQueuedOps(ctx, st, sess, badUpdBatch, now)
	if resBadUpd.Errors != 1 {
		t.Errorf("expected error for empty status, got %+v", resBadUpd)
	}

	// 5. Add note with empty text
	emptyNotePayload, _ := json.Marshal(phone.AddNotePayload{Text: "  "})
	badNoteBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "note-empty", Action: "add_note", TaskID: "nonexistent", ClientTime: now(), Payload: emptyNotePayload},
		},
	}
	resBadNote := phone.ReplayQueuedOps(ctx, st, sess, badNoteBatch, now)
	if resBadNote.Errors != 1 {
		t.Errorf("expected error for empty note, got %+v", resBadNote)
	}

	// 6. Delete task with empty TaskID
	badDelBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "del-empty", Action: "delete_task", TaskID: "", ClientTime: now()},
		},
	}
	resBadDel := phone.ReplayQueuedOps(ctx, st, sess, badDelBatch, now)
	if resBadDel.Errors != 1 {
		t.Errorf("expected error for empty task ID delete, got %+v", resBadDel)
	}

	// 7. Non-existent suggestion resolution
	badSugBatch := phone.ReplayBatch{
		ClientID: "Edge Client",
		Operations: []phone.QueuedOp{
			{OpID: "sug-ghost", Action: "resolve_suggestion", SuggestionID: "sug-99999", ClientTime: now(), Payload: mustJSON(phone.ResolveSuggestionPayload{Accept: true})},
		},
	}
	resBadSug := phone.ReplayQueuedOps(ctx, st, sess, badSugBatch, now)
	if resBadSug.Conflicts != 1 || resBadSug.Results[0].Status != "conflict_stale" {
		t.Errorf("expected conflict_stale for nonexistent suggestion, got %+v", resBadSug)
	}
}

func TestAttachmentUploadAndDownloadEdgeCases(t *testing.T) {
	ctx := context.Background()
	blobDir := t.TempDir()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{BlobDir: blobDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	handler := phone.Handler(phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: false,
	})

	code, _ := sessions.CreatePairingCode(ctx, "Test Phone", 15*time.Minute)
	sess, _ := sessions.RedeemPairingCode(ctx, code.Code, "Client")

	// Create a real task to test attachments on
	created, _, err := st.Create(ctx, task.NewTask{Title: "Bearing lubrication", Priority: task.PriorityHigh})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 1. Upload with path traversal in filename -> sanitized or rejected
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, _ := w.CreateFormFile("photo", "../../etc/passwd")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15c4\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"))
	_ = w.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/phone/tasks/%s/attachments", created.ID), &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Offgrid-Client", "phone")
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec, req)

	// Since sqlstore forbids path separators in attachment names, it should be rejected with 400
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for path traversal filename, got %d", rec.Code)
	}

	// 2. Upload unsupported type (HTML / script disguised as file)
	var b2 bytes.Buffer
	w2 := multipart.NewWriter(&b2)
	part2, _ := w2.CreateFormFile("photo", "malicious.html")
	_, _ = part2.Write([]byte("<html><script>alert(1)</script></html>"))
	_ = w2.Close()

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/api/phone/tasks/%s/attachments", created.ID), &b2)
	req2.Header.Set("Content-Type", w2.FormDataContentType())
	req2.Header.Set("X-Offgrid-Client", "phone")
	req2.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unsupported attachment type, got %d", rec2.Code)
	}

	// 3. Upload to non-existent task
	var b3 bytes.Buffer
	w3 := multipart.NewWriter(&b3)
	part3, _ := w3.CreateFormFile("photo", "valid.png")
	_, _ = part3.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15c4\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"))
	_ = w3.Close()

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/api/phone/tasks/nonexistent-task-id/attachments", &b3)
	req3.Header.Set("Content-Type", w3.FormDataContentType())
	req3.Header.Set("X-Offgrid-Client", "phone")
	req3.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest && rec3.Code != http.StatusNotFound {
		t.Errorf("expected 400/404 for attachment on nonexistent task, got %d", rec3.Code)
	}

	// 4. Download non-existent attachment
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("GET", fmt.Sprintf("/api/phone/tasks/%s/attachments/0000000000000000000000000000000000000000000000000000000000000000", created.ID), nil)
	req4.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec4, req4)

	if rec4.Code != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent attachment hash, got %d", rec4.Code)
	}
}

func TestReadOnlyStoreRejection(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	code, _ := sessions.CreatePairingCode(ctx, "Test Phone", 15*time.Minute)
	sess, _ := sessions.RedeemPairingCode(ctx, code.Code, "Client")

	// ReadOnly = true
	handler := phone.Handler(phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: false,
		ReadOnly:      true,
	})

	// 1. Task creation in ReadOnly mode -> 403 Forbidden
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(`{"title":"ReadOnly probe"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Offgrid-Client", "phone")
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for mutation on read-only server, got %d", rec.Code)
	}

	// 2. Queue replay in ReadOnly mode -> 403 Forbidden
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/phone/sync/replay", strings.NewReader(`{"operations":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Offgrid-Client", "phone")
	req2.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for replay on read-only server, got %d", rec2.Code)
	}
}

func TestLargePayloadBounds(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	code, _ := sessions.CreatePairingCode(ctx, "Test Phone", 15*time.Minute)
	sess, _ := sessions.RedeemPairingCode(ctx, code.Code, "Client")

	handler := phone.Handler(phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: false,
	})

	// Oversized JSON payload > 1 MiB
	hugePayload := strings.Repeat("A", (1<<20)+1024)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/phone/tasks", strings.NewReader(fmt.Sprintf(`{"title":"%s"}`, hugePayload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Offgrid-Client", "phone")
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	handler.ServeHTTP(rec, req)

	// MaxBytesReader should reject reading payload and trigger error
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 400 or 413 for payload exceeding MaxJSONBodyBytes, got %d", rec.Code)
	}
}
