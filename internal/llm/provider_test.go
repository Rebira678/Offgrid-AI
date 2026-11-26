package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProvider(t *testing.T) {
	cases := []struct {
		spec, def string
		kind, url string
		err       string
	}{
		{"", "http://127.0.0.1:11434", KindOllama, "http://127.0.0.1:11434", ""},
		{"127.0.0.1:11434", "", KindOllama, "http://127.0.0.1:11434", ""},
		{"http://pi.lan:11434/", "", KindOllama, "http://pi.lan:11434", ""},
		{"ollama@http://pi-full.lan:11434", "", KindOllama, "http://pi-full.lan:11434", ""},
		{"llamacpp@192.168.1.20:8080", "", KindLlamaCpp, "http://192.168.1.20:8080", ""},
		{"llama.cpp@http://box:8080", "", KindLlamaCpp, "http://box:8080", ""},
		{"openai@https://api.example.com/", "", KindOpenAI, "https://api.example.com", ""},
		{"vllm@gpu.lan:8000", "", KindOpenAI, "http://gpu.lan:8000", ""},
		{"llamacpp@", "http://d:1", KindLlamaCpp, "http://d:1", ""},
		{"gpt@http://x", "", "", "", "unknown provider"},
		{"ftp://host", "", "", "", "not an http or https"},
		{"", "", "", "", "no server address"},
		{"http://user@host:1", "", KindOllama, "http://user@host:1", ""}, // userinfo is not a kind
	}
	for _, c := range cases {
		p, err := ParseProvider(c.spec, c.def)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v, want %q", c.spec, err, c.err)
			}
			continue
		}
		if err != nil || p.Kind != c.kind || p.URL != c.url {
			t.Errorf("%q: got %+v %v, want %s %s", c.spec, p, err, c.kind, c.url)
		}
	}
}

func TestAKeyIsNeverSentInClearToTheInternet(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"http://127.0.0.1:8080", true}, {"http://localhost:8080", true}, {"http://192.168.1.20:8080", true},
		{"http://10.0.0.5:8080", true}, {"http://169.254.1.1", true}, {"http://gpu.lan:8000", true}, {"http://gpu.local", true},
		{"http://box:8080", true}, {"https://api.example.com", true},
		{"http://api.example.com", false}, {"http://8.8.8.8", false}, {"http://172.32.0.1", false}, {"http://192.169.1.1", false},
	}
	for _, c := range cases {
		_, err := Provider{Kind: KindOpenAI, URL: c.url}.Open("sk-secret", RetryPolicy{})
		if c.ok != (err == nil) || (!c.ok && !errors.Is(err, ErrInsecureKey)) {
			t.Errorf("%s: err %v, ok want %v", c.url, err, c.ok)
		}
	}
	if _, err := (Provider{Kind: KindOpenAI, URL: "http://api.example.com"}).Open("", RetryPolicy{}); err != nil {
		t.Fatalf("no key, nothing to protect: %v", err)
	}
}

func TestOnlyRemoteProvidersRetry(t *testing.T) {
	loc, _ := Provider{Kind: KindOllama, URL: "http://127.0.0.1:1"}.Open("", RetryPolicy{})
	rem, _ := Provider{Kind: KindLlamaCpp, URL: "http://gpu.lan:8080"}.Open("", RetryPolicy{})
	if _, ok := loc.(*retrying); ok {
		t.Fatal("a local server that is down must be reported at once")
	}
	if _, ok := rem.(*retrying); !ok {
		t.Fatal("a node on the LAN is retried")
	}
	if _, err := (Provider{Kind: "x", URL: "http://a"}).Open("", RetryPolicy{}); err == nil {
		t.Fatal("unknown kind")
	}
}

func TestALlamaCppNodeOnTheLANAnswersThroughTheOpenAIProtocol(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		okOAI(w, r)
	}))
	defer srv.Close()
	p, err := ParseProvider("llamacpp@"+srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Open("", RetryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Chat(context.Background(), validRequest(), nil)
	if err != nil || resp.Text == "" || path != "/v1/chat/completions" || auth != "" {
		t.Fatalf("%v %q %q %q", err, resp.Text, path, auth)
	}
}
