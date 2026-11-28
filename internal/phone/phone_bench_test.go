package phone_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"offgrid/internal/phone"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func BenchmarkPairingRedeem(b *testing.B) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		b.Fatalf("session store: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		code, err := sessions.CreatePairingCode(ctx, "Bench Phone", 15*time.Minute)
		if err != nil {
			b.Fatalf("create pairing: %v", err)
		}
		_, err = sessions.RedeemPairingCode(ctx, code.Code, "Bench Client")
		if err != nil {
			b.Fatalf("redeem pairing: %v", err)
		}
		if i%100 == 0 {
			_, _ = st.DB().Exec("DELETE FROM phone_pairings")
		}
	}
}

func BenchmarkAuthMiddleware(b *testing.B) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()

	sessions, err := phone.NewSessionStore(st.DB(), time.Now)
	if err != nil {
		b.Fatalf("session store: %v", err)
	}

	code, _ := sessions.CreatePairingCode(ctx, "Bench Phone", 15*time.Minute)
	sess, _ := sessions.RedeemPairingCode(ctx, code.Code, "Bench Client")

	handler := phone.AuthMiddleware(sessions, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/phone/tasks", nil)
	req.AddCookie(&http.Cookie{
		Name:  phone.SessionCookieName,
		Value: sess.Token,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", rec.Code)
		}
	}
}

func BenchmarkReplayQueue(b *testing.B) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()

	now := time.Now
	sess := &phone.Session{Token: "bench-tok", Label: "Bench Client"}

	createPayload, _ := json.Marshal(phone.CreateTaskPayload{
		Title:    "Bench Task",
		Priority: "medium",
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := phone.ReplayBatch{
			ClientID: "Bench Client",
			Operations: []phone.QueuedOp{
				{
					OpID:       fmt.Sprintf("bench-%d", i),
					Action:     "create_task",
					ClientTime: now(),
					Payload:    createPayload,
				},
			},
		}
		res := phone.ReplayQueuedOps(ctx, st, sess, batch, now)
		if res.Applied != 1 {
			b.Fatalf("expected 1 applied op, got %+v", res)
		}
	}
}

func BenchmarkListTasks(b *testing.B) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, ":memory:", sqlstore.Options{})
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Seed 50 tasks
	for i := 0; i < 50; i++ {
		_, _, _ = st.Create(ctx, task.NewTask{
			Title:    fmt.Sprintf("Seed Task %d", i),
			Priority: task.PriorityMedium,
		})
	}

	sessions, _ := phone.NewSessionStore(st.DB(), time.Now)
	code, _ := sessions.CreatePairingCode(ctx, "Bench Phone", 15*time.Minute)
	sess, _ := sessions.RedeemPairingCode(ctx, code.Code, "Bench Client")

	handler := phone.Handler(phone.Config{
		Store:         st,
		Sessions:      sessions,
		Now:           time.Now,
		SecureCookies: false,
	})

	req := httptest.NewRequest("GET", "/api/phone/tasks", nil)
	req.AddCookie(&http.Cookie{
		Name:  phone.SessionCookieName,
		Value: sess.Token,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", rec.Code)
		}
	}
}
