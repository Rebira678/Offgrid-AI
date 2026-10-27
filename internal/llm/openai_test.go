package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func oaiServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *OpenAICompat {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(srv.Close)
	return NewOpenAICompat(srv.URL+"/", "sk-secret-123")
}

func okOAI(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(`{"model":"big-1","choices":[{"message":{"role":"assistant","content":"Two pumps are open."}}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`))
}

func TestOpenAICompatSendsTheRightRequestAndReadsTheReply(t *testing.T) {
	var gotAuth, gotPath string
	var body oaiRequest
	c := oaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
		okOAI(w, r)
	})
	var pieces []string
	req := validRequest()
	req.MaxTokens, req.Temperature = 99, 0.2
	resp, err := c.Chat(context.Background(), req, func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-secret-123" {
		t.Fatalf("path %q auth %q", gotPath, gotAuth)
	}
	if body.Stream || body.MaxTokens != 99 || body.Temperature != 0.2 || body.Model != req.Model || len(body.Messages) != len(req.Messages) {
		t.Fatalf("body = %+v", body)
	}
	if resp.Text != "Two pumps are open." || resp.Model != "big-1" || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("resp = %+v", resp)
	}
	if strings.Join(pieces, "") != resp.Text {
		t.Fatalf("pieces %v", pieces)
	}
}

func TestOpenAICompatMapsFailuresToTheSharedErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"bad key", 401, `{"error":{"message":"bad key"}}`, func(e error) bool { return errors.Is(e, ErrRejected) }},
		{"forbidden", 403, `{}`, func(e error) bool { return errors.Is(e, ErrRejected) }},
		{"quota", 429, `{"error":{"message":"no money","code":"insufficient_quota"}}`, func(e error) bool { return errors.Is(e, ErrRejected) }},
		{"rate limit", 429, `{"error":{"message":"slow down"}}`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"server error", 503, `oops`, func(e error) bool { return errors.Is(e, ErrUnavailable) }},
		{"no model", 404, `{"error":{"message":"no such model","code":"model_not_found"}}`, func(e error) bool {
			var nf *ModelNotFoundError
			return errors.As(e, &nf) && nf.Model == validRequest().Model
		}},
		{"no choices", 200, `{"choices":[]}`, func(e error) bool { return e != nil && !errors.Is(e, ErrUnavailable) }},
		{"other 4xx", 400, `{"error":{"message":"bad"}}`, func(e error) bool {
			return e != nil && !errors.Is(e, ErrUnavailable) && !errors.Is(e, ErrRejected)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := oaiServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := c.Chat(context.Background(), validRequest(), nil)
			if !tc.check(err) {
				t.Fatalf("status %d gave %v", tc.status, err)
			}
		})
	}
}

func TestOpenAICompatUnreachableIsUnavailableAndNeverLeaksTheKey(t *testing.T) {
	c := NewOpenAICompat("http://127.0.0.1:1", "sk-secret-123")
	_, err := c.Chat(context.Background(), validRequest(), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-123") {
		t.Fatal("the key leaked into an error")
	}
	if got := scrub(errors.New("boom sk-secret-123 boom"), "sk-secret-123"); strings.Contains(got.Error(), "sk-") {
		t.Fatalf("scrub left the key: %v", got)
	}
}

func TestOpenAICompatSendsNoAuthHeaderWithoutAKey(t *testing.T) {
	var saw bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, saw = r.Header["Authorization"]
		okOAI(w, r)
	}))
	defer srv.Close()
	if _, err := NewOpenAICompat(srv.URL, "").Chat(context.Background(), validRequest(), nil); err != nil || saw {
		t.Fatalf("err=%v authHeaderSent=%v", err, saw)
	}
}

type echoKeyTransport struct{}

func (echoKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("dial failed while sending " + r.Header.Get("Authorization"))
}

func TestOpenAICompatScrubsTheKeyEvenWhenATransportEchoesTheHeader(t *testing.T) {
	c := NewOpenAICompat("http://example.invalid", "sk-secret-123")
	c.HTTP = &http.Client{Transport: echoKeyTransport{}}
	_, err := c.Chat(context.Background(), validRequest(), nil)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "sk-secret-123") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenAICompatCancelledMidRequestReturnsTheContextErrorNotAnOutage(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c := oaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := c.Chat(ctx, validRequest(), nil); errc <- err }()
	<-started
	cancel()
	err := <-errc
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v: a cancelled job must not look like a network outage", err)
	}
}
