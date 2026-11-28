package phone_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func setupTestServer(t *testing.T) (*httptest.Server, *sqlstore.Store, *phone.SessionStore) {
	t.Helper()
	ctx := context.Background()
	blobDir := t.TempDir()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{
		BlobDir: blobDir,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	handler := phone.Handler(phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: false, // http test server
		ReadOnly:      false,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv, st, sessions
}

func TestPhoneClientEndToEnd(t *testing.T) {
	ctx := context.Background()
	srv, st, sessions := setupTestServer(t)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 1. Static Assets & PWA Shell load without authentication
	for _, asset := range []string{"/", "/manifest.webmanifest", "/sw.js", "/app.js", "/app.css", "/icon.svg"} {
		res, err := client.Get(srv.URL + asset)
		if err != nil {
			t.Fatalf("get asset %s: %v", asset, err)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("asset %s returned %d", asset, res.StatusCode)
		}
		_ = res.Body.Close()
	}

	// 2. Unpaired phone gets 401 Unauthorized for data endpoints
	reqTasks, _ := http.NewRequest("GET", srv.URL+"/api/phone/tasks", nil)
	res, err := client.Do(reqTasks)
	if err != nil {
		t.Fatalf("get tasks: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unpaired client expected 401, got %d", res.StatusCode)
	}
	_ = res.Body.Close()

	// 3. Node issues pairing code
	pairCode, err := sessions.CreatePairingCode(ctx, "Field Technician Dave", 10*time.Minute)
	if err != nil {
		t.Fatalf("create pairing code: %v", err)
	}

	// 4. Phone redeems pairing code
	pairBody, _ := json.Marshal(map[string]string{
		"code":  pairCode.Code,
		"label": "Rugged Phone 01",
	})
	reqPair, _ := http.NewRequest("POST", srv.URL+"/api/phone/pair", bytes.NewReader(pairBody))
	reqPair.Header.Set("Content-Type", "application/json")
	reqPair.Header.Set("X-Offgrid-Client", "phone")

	resPair, err := client.Do(reqPair)
	if err != nil {
		t.Fatalf("pair request failed: %v", err)
	}
	if resPair.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resPair.Body)
		t.Fatalf("pairing failed with %d: %s", resPair.StatusCode, string(b))
	}

	var pairResp struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		Label string `json:"label"`
	}
	_ = json.NewDecoder(resPair.Body).Decode(&pairResp)
	_ = resPair.Body.Close()

	if !pairResp.OK || pairResp.Token == "" {
		t.Fatalf("invalid pair response: %+v", pairResp)
	}

	// 5. Verify session endpoint succeeds
	reqSess, _ := http.NewRequest("GET", srv.URL+"/api/phone/session", nil)
	resSess, err := client.Do(reqSess)
	if err != nil || resSess.StatusCode != http.StatusOK {
		t.Fatalf("session check failed: status=%d, err=%v", resSess.StatusCode, err)
	}
	_ = resSess.Body.Close()

	// 6. Online task creation
	taskBody, _ := json.Marshal(map[string]string{
		"title":    "Inspect main intake manifold",
		"priority": "high",
		"note":     "Audible hissing sound near gasket",
	})
	reqCreate, _ := http.NewRequest("POST", srv.URL+"/api/phone/tasks", bytes.NewReader(taskBody))
	reqCreate.Header.Set("Content-Type", "application/json")
	reqCreate.Header.Set("X-Offgrid-Client", "phone")

	resCreate, err := client.Do(reqCreate)
	if err != nil || resCreate.StatusCode != http.StatusCreated {
		t.Fatalf("task create failed: status=%d, err=%v", resCreate.StatusCode, err)
	}

	var createdTask phone.PhoneTask
	_ = json.NewDecoder(resCreate.Body).Decode(&createdTask)
	_ = resCreate.Body.Close()

	if createdTask.Title != "Inspect main intake manifold" {
		t.Errorf("title mismatch: got %q", createdTask.Title)
	}

	// 7. List tasks
	reqList, _ := http.NewRequest("GET", srv.URL+"/api/phone/tasks", nil)
	resList, err := client.Do(reqList)
	if err != nil || resList.StatusCode != http.StatusOK {
		t.Fatalf("list tasks failed: status=%d, err=%v", resList.StatusCode, err)
	}
	var taskList []phone.PhoneTask
	_ = json.NewDecoder(resList.Body).Decode(&taskList)
	_ = resList.Body.Close()
	if len(taskList) != 1 {
		t.Fatalf("expected 1 task in list, got %d", len(taskList))
	}

	// 8. Upload photo attachment to the task
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, err := w.CreateFormFile("photo", "crack.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	testPhoto := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15c4\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82")
	_, _ = part.Write(testPhoto)
	_ = w.Close()

	reqUpload, _ := http.NewRequest("POST", srv.URL+fmt.Sprintf("/api/phone/tasks/%s/attachments", createdTask.ID), &b)
	reqUpload.Header.Set("Content-Type", w.FormDataContentType())
	reqUpload.Header.Set("X-Offgrid-Client", "phone")

	resUpload, err := client.Do(reqUpload)
	if err != nil || resUpload.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resUpload.Body)
		t.Fatalf("upload attachment failed: status=%d, err=%v, body=%s", resUpload.StatusCode, err, string(b))
	}
	var att phone.PhoneAttachment
	_ = json.NewDecoder(resUpload.Body).Decode(&att)
	_ = resUpload.Body.Close()

	if att.Hash == "" {
		t.Errorf("uploaded attachment hash is empty")
	}

	// 9. Fetch attachment back
	reqAtt, _ := http.NewRequest("GET", srv.URL+att.URL, nil)
	resAtt, err := client.Do(reqAtt)
	if err != nil || resAtt.StatusCode != http.StatusOK {
		t.Fatalf("download attachment failed: status=%d, err=%v", resAtt.StatusCode, err)
	}
	downBytes, _ := io.ReadAll(resAtt.Body)
	_ = resAtt.Body.Close()
	if !bytes.Equal(downBytes, testPhoto) {
		t.Errorf("downloaded bytes differ from uploaded photo")
	}

	// 10. AI Suggestion: create on node, accept from phone
	medPri := task.PriorityMedium
	sug, _, err := st.Suggest(ctx, task.Draft{
		Task:     createdTask.ID,
		Priority: &medPri,
		Note:     "Downgrade to medium after initial pressure check",
		By:       "diagnostics-ai",
	})
	if err != nil {
		t.Fatalf("create suggestion: %v", err)
	}

	reqSugList, _ := http.NewRequest("GET", srv.URL+"/api/phone/suggestions", nil)
	resSugList, err := client.Do(reqSugList)
	if err != nil || resSugList.StatusCode != http.StatusOK {
		t.Fatalf("list suggestions failed: status=%d", resSugList.StatusCode)
	}
	var sugs []phone.PhoneSuggestion
	_ = json.NewDecoder(resSugList.Body).Decode(&sugs)
	_ = resSugList.Body.Close()
	if len(sugs) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(sugs))
	}

	// Accept suggestion
	reqAccept, _ := http.NewRequest("POST", srv.URL+fmt.Sprintf("/api/phone/suggestions/%s/accept", sug.ID), strings.NewReader(`{}`))
	reqAccept.Header.Set("X-Offgrid-Client", "phone")
	resAccept, err := client.Do(reqAccept)
	if err != nil || resAccept.StatusCode != http.StatusOK {
		t.Fatalf("accept suggestion failed: status=%d", resAccept.StatusCode)
	}
	_ = resAccept.Body.Close()

	// 11. PHONE GOES OFFLINE, EDITS, RECONNECTS (The core Day 52 test)
	// Phone queues edits offline: create task, update status, add note
	offlineOps := []phone.QueuedOp{
		{
			OpID:       "op-off-1",
			Action:     "create_task",
			ClientTime: time.Now().Add(-30 * time.Minute),
			Payload:    mustJSON(phone.CreateTaskPayload{Title: "Emergency pump repair", Priority: "high", Note: "Belt snapped"}),
		},
		{
			OpID:       "op-off-2",
			Action:     "update_status",
			TaskID:     createdTask.ID,
			ClientTime: time.Now().Add(-25 * time.Minute),
			Payload:    mustJSON(phone.UpdateStatusPayload{Status: "done"}),
		},
		{
			OpID:       "op-off-3",
			Action:     "add_note",
			TaskID:     createdTask.ID,
			ClientTime: time.Now().Add(-20 * time.Minute),
			Payload:    mustJSON(phone.AddNotePayload{Text: "Replaced clamp and gasket"}),
		},
	}

	replayBody, _ := json.Marshal(phone.ReplayBatch{
		ClientID:   "Dave Field Phone",
		Operations: offlineOps,
	})

	// Phone reconnects: replays batch
	reqReplay, _ := http.NewRequest("POST", srv.URL+"/api/phone/sync/replay", bytes.NewReader(replayBody))
	reqReplay.Header.Set("Content-Type", "application/json")
	reqReplay.Header.Set("X-Offgrid-Client", "phone")

	resReplay, err := client.Do(reqReplay)
	if err != nil || resReplay.StatusCode != http.StatusOK {
		t.Fatalf("replay failed: status=%d, err=%v", resReplay.StatusCode, err)
	}

	var replayResp phone.ReplayResponse
	_ = json.NewDecoder(resReplay.Body).Decode(&replayResp)
	_ = resReplay.Body.Close()

	if replayResp.Applied != 3 || replayResp.Conflicts != 0 || replayResp.Errors != 0 {
		t.Fatalf("unexpected replay results: %+v", replayResp)
	}

	// 12. VERIFY EDITS APPEAR ON THE NODE MARKED LATE
	// Task 1 was marked done, and has late notes
	t1OnNode, err := st.Get(ctx, createdTask.ID)
	if err != nil {
		t.Fatalf("get task 1: %v", err)
	}
	if t1OnNode.Status != task.StatusDone {
		t.Errorf("task status on node was not updated to done: got %v", t1OnNode.Status)
	}

	hasLateStatusNote := false
	hasLateTextNote := false
	for _, n := range t1OnNode.Notes {
		if strings.Contains(n.Text, "[late sync from phone") && strings.Contains(n.Text, "status changed to done") {
			hasLateStatusNote = true
		}
		if strings.Contains(n.Text, "Replaced clamp and gasket [late sync from phone") {
			hasLateTextNote = true
		}
	}
	if !hasLateStatusNote || !hasLateTextNote {
		t.Errorf("late sync notes missing on node task: notes=%+v", t1OnNode.Notes)
	}

	// The offline-created task exists and has late sync marker
	taskListFinal, err := st.List(ctx, task.Filter{})
	if err != nil {
		t.Fatalf("list final: %v", err)
	}
	if len(taskListFinal) != 2 {
		t.Fatalf("expected 2 tasks on node, got %d", len(taskListFinal))
	}

	foundEmergencyTask := false
	for _, taskItem := range taskListFinal {
		if taskItem.Title == "Emergency pump repair" {
			foundEmergencyTask = true
			if len(taskItem.Notes) == 0 || !strings.Contains(taskItem.Notes[0].Text, "[late sync from phone") {
				t.Errorf("emergency task missing late sync note: %+v", taskItem.Notes)
			}
		}
	}
	if !foundEmergencyTask {
		t.Errorf("offline created task not found on node")
	}

	// 13. Revoke session and verify access is immediately denied
	nRevoked, err := sessions.RevokeSession(ctx, pairResp.Token)
	if err != nil || nRevoked != 1 {
		t.Fatalf("revoke failed: n=%d, err=%v", nRevoked, err)
	}

	resRevoked, err := client.Do(reqTasks)
	if err != nil || resRevoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after session revocation, got %d", resRevoked.StatusCode)
	}
	_ = resRevoked.Body.Close()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
