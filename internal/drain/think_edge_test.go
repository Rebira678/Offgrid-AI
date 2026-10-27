package drain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"offgrid/internal/jobqueue"
	"offgrid/internal/llm"
)

type memoryBook struct {
	mu      sync.Mutex
	answers map[string]Answer
	lookups int
	records int
}

func newMemoryBook() *memoryBook {
	return &memoryBook{answers: make(map[string]Answer)}
}

func (b *memoryBook) Lookup(ctx context.Context, key string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lookups++
	a, ok := b.answers[key]
	return a.Text, ok, nil
}

func (b *memoryBook) Record(ctx context.Context, a Answer) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records++
	if existing, ok := b.answers[a.Key]; ok {
		return existing.Text, nil
	}
	b.answers[a.Key] = a
	return a.Text, nil
}

func edgeJob(key, text string) jobqueue.Job {
	return jobqueue.Job{
		ID:      1,
		Key:     key,
		Kind:    KindDeepThink,
		Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
	}
}

func TestThinkEdgeCloudFailoverHTTPStatuses(t *testing.T) {
	testErrors := []struct {
		name string
		err  error
	}{
		{"401 Unauthorized", fmt.Errorf("%w (HTTP 401): invalid api key", llm.ErrRejected)},
		{"403 Forbidden", fmt.Errorf("%w (HTTP 403): forbidden", llm.ErrRejected)},
		{"429 Rate Limit", fmt.Errorf("%w: HTTP 429: rate limit exceeded", llm.ErrUnavailable)},
		{"500 Server Error", fmt.Errorf("%w: HTTP 500: internal server error", llm.ErrUnavailable)},
		{"503 Service Unavailable", fmt.Errorf("%w: HTTP 503: service unavailable", llm.ErrUnavailable)},
		{"Network Timeout", errors.New("dial tcp: i/o timeout")},
	}

	for _, tc := range testErrors {
		t.Run(tc.name, func(t *testing.T) {
			cloudCalls := 0
			cloudClient := &llm.Fake{
				Handler: func(ctx context.Context, req llm.Request) (string, error) {
					cloudCalls++
					return "", tc.err
				},
			}

			localCalls := 0
			localClient := &llm.Fake{
				Handler: func(ctx context.Context, req llm.Request) (string, error) {
					localCalls++
					return "Local fallback answer.", nil
				},
			}

			book := newMemoryBook()
			spec := Think(ThinkConfig{
				Cloud:      cloudClient,
				CloudModel: "cloud-r1",
				Online:     func() bool { return true },
				Local:      localClient,
				LocalModel: "qwen2.5:7b",
				Book:       book,
			})

			job := edgeJob("job-failover-"+tc.name, "What is the power output?")

			out, err := spec.Handle(context.Background(), job, nil)
			if err != nil {
				t.Fatalf("expected successful fallback, got error: %v", err)
			}
			if out != "Local fallback answer." {
				t.Fatalf("unexpected answer: %q", out)
			}
			if cloudCalls != 1 {
				t.Fatalf("expected 1 cloud call, got %d", cloudCalls)
			}
			if localCalls != 1 {
				t.Fatalf("expected 1 local call, got %d", localCalls)
			}

			book.mu.Lock()
			saved, ok := book.answers[job.Key]
			book.mu.Unlock()
			if !ok {
				t.Fatal("answer not recorded in book")
			}
			if saved.Source != "local" || saved.Model != "qwen2.5:7b" {
				t.Fatalf("unexpected recorded answer metadata: %+v", saved)
			}
		})
	}
}

func TestThinkEdgeCloudEmptyResponseFallback(t *testing.T) {
	cloudClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "   \n\t  ", nil // 200 OK but blank content
		},
	}

	localClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "Local model stepped in.", nil
		},
	}

	book := newMemoryBook()
	spec := Think(ThinkConfig{
		Cloud:      cloudClient,
		CloudModel: "cloud-gpt",
		Online:     func() bool { return true },
		Local:      localClient,
		LocalModel: "qwen2.5:7b",
		Book:       book,
	})

	job := edgeJob("job-blank-cloud", "Summarize telemetry")

	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Local model stepped in." {
		t.Fatalf("expected local fallback, got: %q", out)
	}
}

func TestThinkEdgeContextCancellationNoLocalFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cloudClient := &llm.Fake{
		Handler: func(c context.Context, req llm.Request) (string, error) {
			cancel() // cancel context while cloud request is running
			return "", c.Err()
		},
	}

	localCalled := false
	localClient := &llm.Fake{
		Handler: func(c context.Context, req llm.Request) (string, error) {
			localCalled = true
			return "Should not be called", nil
		},
	}

	spec := Think(ThinkConfig{
		Cloud:      cloudClient,
		CloudModel: "cloud-r1",
		Online:     func() bool { return true },
		Local:      localClient,
		LocalModel: "qwen2.5:7b",
	})

	job := edgeJob("job-cancelled", "Long query")

	_, err := spec.Handle(ctx, job, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if localCalled {
		t.Fatal("local model was called after context cancellation!")
	}
}

func TestThinkEdgeModelNotFoundErrorDeadLetter(t *testing.T) {
	localClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "", &llm.ModelNotFoundError{Model: "missing-model:7b", Err: errors.New("404 not found")}
		},
	}

	spec := Think(ThinkConfig{
		Local:      localClient,
		LocalModel: "missing-model:7b",
	})

	job := edgeJob("job-missing-model", "Diagnose vibration")

	_, err := spec.Handle(context.Background(), job, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("expected ErrPermanent for missing model, got: %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), "is not installed on this laptop") {
		t.Fatalf("expected helpful installation guidance in error, got: %q", err.Error())
	}
}

func TestThinkEdgeAnswerBookLookupHitNoModelCalled(t *testing.T) {
	book := newMemoryBook()
	book.answers["job-cached"] = Answer{
		Key:      "job-cached",
		Question: "Check water filter",
		Text:     "Water filter replaced yesterday at 14:00.",
		Model:    "cached-model",
		Source:   "local",
	}

	modelCalled := false
	localClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			modelCalled = true
			return "Should not run", nil
		},
	}

	spec := Think(ThinkConfig{
		Local:      localClient,
		LocalModel: "qwen2.5:7b",
		Book:       book,
	})

	job := edgeJob("job-cached", "Check water filter")

	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Water filter replaced yesterday at 14:00." {
		t.Fatalf("unexpected cached answer: %q", out)
	}
	if modelCalled {
		t.Fatal("model was called despite answer already being cached in book!")
	}
	if book.lookups != 1 {
		t.Fatalf("expected 1 lookup, got %d", book.lookups)
	}
	if book.records != 0 {
		t.Fatalf("expected 0 new records, got %d", book.records)
	}
}

func TestThinkEdgeUTF8SafeTruncation(t *testing.T) {
	// Construct a string where byte 8192 falls in the middle of a 4-byte emoji (🧠 = 0xF0 0x9F 0xA7 0xA0)
	// Fill up to 8190 with ASCII 'A', then add the 4-byte emoji.
	// Total length: 8190 + 4 = 8194 bytes.
	// A naive slice s[:8192] would cut the emoji in half at byte 2!
	prefix := strings.Repeat("A", 8190)
	raw := prefix + "🧠" + "EXTRA"

	localClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return raw, nil
		},
	}

	spec := Think(ThinkConfig{
		Local:      localClient,
		LocalModel: "qwen2.5:7b",
	})

	job := edgeJob("job-utf8-trunc", "Generate long text")

	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out) > maxResultBytes {
		t.Fatalf("truncated string length %d exceeds max %d", len(out), maxResultBytes)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("truncated string contains invalid/broken UTF-8 sequences!")
	}
	// It should have safely peeled back the incomplete emoji and left the 8190 ASCII bytes
	if out != prefix {
		t.Fatalf("expected %d bytes ending cleanly before broken rune, got %d bytes", len(prefix), len(out))
	}
}

func TestThinkEdgeOfflineCloudSkipped(t *testing.T) {
	cloudCalled := false
	cloudClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			cloudCalled = true
			return "Cloud answer", nil
		},
	}

	localCalled := false
	localClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			localCalled = true
			return "Local offline answer", nil
		},
	}

	spec := Think(ThinkConfig{
		Cloud:      cloudClient,
		CloudModel: "cloud-r1",
		Online:     func() bool { return false }, // OFFLINE
		Local:      localClient,
		LocalModel: "qwen2.5:7b",
	})

	job := edgeJob("job-offline-skip", "Offline query")

	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "Local offline answer" {
		t.Fatalf("unexpected answer: %q", out)
	}
	if cloudCalled {
		t.Fatal("cloud client was called while offline!")
	}
	if !localCalled {
		t.Fatal("local client was not called!")
	}
}

func TestThinkEdgeContextCancellationUpfront(t *testing.T) {
	spec := Think(ThinkConfig{
		LocalModel: "qwen2.5:7b",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	job := edgeJob("job-pre-cancelled", "Should not run")
	_, err := spec.Handle(ctx, job, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestThinkEdgeContextReadError(t *testing.T) {
	spec := Think(ThinkConfig{
		Local:      &llm.Fake{},
		LocalModel: "qwen2.5:7b",
		Context: func(ctx context.Context) (string, error) {
			return "", errors.New("corrupt tasks file")
		},
	})

	job := edgeJob("job-ctx-err", "Query")
	_, err := spec.Handle(context.Background(), job, nil)
	if err == nil || !strings.Contains(err.Error(), "reading the task data: corrupt tasks file") {
		t.Fatalf("expected wrapped context read error, got: %v", err)
	}
}

func TestThinkHandlerGiantAnswerBookStorage(t *testing.T) {
	book := newMemoryBook()
	modelCalls := 0

	// Generate 75KB string with multi-byte runes: "ኢትዮጵያ 💡 ⚡ "
	repeats := 3000
	giantOutput := strings.Repeat("ኢትዮጵያ 💡 ⚡ ", repeats)
	if len(giantOutput) < 65000 {
		t.Fatalf("expected giantOutput length >= 65000, got %d", len(giantOutput))
	}

	fakeClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			modelCalls++
			return giantOutput, nil
		},
	}

	spec := Think(ThinkConfig{
		Local:      fakeClient,
		LocalModel: "qwen2.5:7b",
		Book:       book,
	})

	job := edgeJob("job-giant-answer", "How to fix the generator?")
	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("expected clean handling, got err: %v", err)
	}

	// Verify length is capped at maxResultBytes
	if len(out) > maxResultBytes {
		t.Fatalf("out length %d exceeds max %d", len(out), maxResultBytes)
	}
	if !utf8.ValidString(out) {
		t.Fatal("out is not valid UTF-8 string!")
	}

	// Verify book recorded the exact truncated output
	ans, found, err := book.Lookup(context.Background(), "job-giant-answer")
	if err != nil || !found {
		t.Fatalf("book lookup failed: found=%v err=%v", found, err)
	}
	if ans != out {
		t.Fatalf("book recorded text mismatch: len(ans)=%d len(out)=%d", len(ans), len(out))
	}
	if len(book.answers["job-giant-answer"].Text) > maxResultBytes {
		t.Fatalf("book stored answer length exceeds maxResultBytes: %d", len(book.answers["job-giant-answer"].Text))
	}

	// Second execution must hit book cache without calling LLM again
	out2, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("second handle failed: %v", err)
	}
	if out2 != out {
		t.Fatalf("second call mismatch: got %q, want %q", out2, out)
	}
	if modelCalls != 1 {
		t.Fatalf("expected model to be called only once, called %d times", modelCalls)
	}
}

func TestThinkHandlerNilBook(t *testing.T) {
	fakeClient := &llm.Fake{
		Handler: func(ctx context.Context, req llm.Request) (string, error) {
			return "Clean direct answer without book storage", nil
		},
	}

	spec := Think(ThinkConfig{
		Local:      fakeClient,
		LocalModel: "qwen2.5:7b",
		Book:       nil, // Legacy mode: no book configured
	})

	job := edgeJob("job-nil-book", "Is the fuel pump operational?")
	out, err := spec.Handle(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("unexpected error with nil book: %v", err)
	}
	if out != "Clean direct answer without book storage" {
		t.Fatalf("unexpected output: %q", out)
	}
}


