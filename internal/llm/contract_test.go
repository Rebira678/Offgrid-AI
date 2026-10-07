package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"offgrid/internal/ollama"
)

// chatServer imitates Ollama's /api/chat well enough to drive the real
// adapter end to end.
func chatServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		for _, line := range []string{
			`{"message":{"role":"assistant","content":"Hello"},"done":false}`,
			`{"message":{"role":"assistant","content":" world"},"done":false}`,
			`{"done":true,"total_duration":3000000000,"load_duration":1000000000,` +
				`"prompt_eval_count":10,"prompt_eval_duration":1000000000,` +
				`"eval_count":2,"eval_duration":500000000}`,
		} {
			fmt.Fprintln(w, line)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestClientContract runs the same behavioural checks against every
// implementation. This is what makes them swappable: not that they share a
// method signature (the compiler already checks that), but that they behave
// the same way. Adding a new backend later means adding one line here.
func TestClientContract(t *testing.T) {
	implementations := map[string]func(t *testing.T) Client{
		"Fake": func(t *testing.T) Client { return &Fake{} },
		"Ollama": func(t *testing.T) Client {
			return NewOllama(ollama.NewClient(chatServer(t).URL))
		},
	}

	for name, newClient := range implementations {
		t.Run(name, func(t *testing.T) {
			t.Run("streamed pieces join to the final text", func(t *testing.T) {
				var pieces []string
				resp, err := newClient(t).Chat(context.Background(), validRequest(), func(s string) {
					pieces = append(pieces, s)
				})
				if err != nil {
					t.Fatal(err)
				}
				if resp.Text == "" {
					t.Fatal("empty reply")
				}
				if got := strings.Join(pieces, ""); got != resp.Text {
					t.Errorf("pieces joined = %q, Response.Text = %q", got, resp.Text)
				}
			})

			t.Run("nil onToken is allowed", func(t *testing.T) {
				if _, err := newClient(t).Chat(context.Background(), validRequest(), nil); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("cancelled context returns the context error", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := newClient(t).Chat(ctx, validRequest(), nil)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want an error matching context.Canceled", err)
				}
			})

			t.Run("invalid request is rejected", func(t *testing.T) {
				_, err := newClient(t).Chat(context.Background(), Request{}, nil)
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("got %v, want ErrInvalidRequest", err)
				}
			})

			t.Run("concurrent use is safe", func(t *testing.T) {
				c := newClient(t)
				errs := make(chan error, 8)
				for i := 0; i < cap(errs); i++ {
					go func() {
						_, err := c.Chat(context.Background(), validRequest(), nil)
						errs <- err
					}()
				}
				for i := 0; i < cap(errs); i++ {
					if err := <-errs; err != nil {
						t.Error(err)
					}
				}
			})
		})
	}
}
