package llm

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseProviderComprehensiveMatrix(t *testing.T) {
	cases := []struct {
		name       string
		spec       string
		defURL     string
		wantKind   string
		wantURL    string
		wantErrSub string
	}{
		// Bare addresses default to Ollama
		{"bare-ip-port", "127.0.0.1:11434", "", KindOllama, "http://127.0.0.1:11434", ""},
		{"bare-http-url", "http://127.0.0.1:11434", "", KindOllama, "http://127.0.0.1:11434", ""},
		{"bare-https-url", "https://ollama.example.com", "", KindOllama, "https://ollama.example.com", ""},
		{"bare-with-trailing-slashes", "http://localhost:11434///", "", KindOllama, "http://localhost:11434", ""},

		// Ollama prefixes
		{"ollama-prefix", "ollama@http://pi.lan:11434", "", KindOllama, "http://pi.lan:11434", ""},
		{"ollama-prefix-upper", "OLLAMA@http://pi.lan:11434", "", KindOllama, "http://pi.lan:11434", ""},
		{"ollama-prefix-no-scheme", "ollama@pi.lan:11434", "", KindOllama, "http://pi.lan:11434", ""},

		// LlamaCpp prefixes & aliases
		{"llamacpp-standard", "llamacpp@http://192.168.1.50:8080", "", KindLlamaCpp, "http://192.168.1.50:8080", ""},
		{"llamacpp-alias-dot", "llama.cpp@http://192.168.1.50:8080", "", KindLlamaCpp, "http://192.168.1.50:8080", ""},
		{"llamacpp-alias-server", "llama-server@http://192.168.1.50:8080", "", KindLlamaCpp, "http://192.168.1.50:8080", ""},
		{"llamacpp-upper", "LLAMACPP@http://192.168.1.50:8080", "", KindLlamaCpp, "http://192.168.1.50:8080", ""},

		// OpenAI prefixes & aliases
		{"openai-standard", "openai@https://api.openai.com/v1", "", KindOpenAI, "https://api.openai.com/v1", ""},
		{"openai-alias-compat", "openai-compat@https://api.together.xyz/v1", "", KindOpenAI, "https://api.together.xyz/v1", ""},
		{"openai-alias-vllm", "vllm@http://cluster.local:8000/v1", "", KindOpenAI, "http://cluster.local:8000/v1", ""},
		{"openai-alias-lmstudio", "lmstudio@http://localhost:1234/v1", "", KindOpenAI, "http://localhost:1234/v1", ""},
		{"openai-upper", "OPENAI@https://api.openai.com", "", KindOpenAI, "https://api.openai.com", ""},

		// Defaults and fallbacks
		{"empty-spec-with-default", "", "http://127.0.0.1:11434", KindOllama, "http://127.0.0.1:11434", ""},
		{"spaces-spec-with-default", "   ", "http://127.0.0.1:11434", KindOllama, "http://127.0.0.1:11434", ""},
		{"empty-spec-no-default", "", "", "", "", "no server address given"},

		// Unknown provider
		{"unknown-provider-anthropic", "anthropic@https://api.anthropic.com", "", "", "", "unknown provider"},
		{"unknown-provider-custom", "custom@http://127.0.0.1", "", "", "", "unknown provider"},

		// Malformed URLs
		{"invalid-scheme-ftp", "ftp://127.0.0.1:21", "", "", "", "not an http or https address"},
		{"invalid-scheme-ws", "ws://127.0.0.1:8080", "", "", "", "not an http or https address"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseProvider(c.spec, c.defURL)
			if c.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
					t.Fatalf("expected error containing %q, got: %v", c.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", p.Kind, c.wantKind)
			}
			if p.URL != c.wantURL {
				t.Errorf("URL = %q, want %q", p.URL, c.wantURL)
			}
		})
	}
}

func TestProviderLocalDetection(t *testing.T) {
	localAddrs := []string{
		"http://localhost:11434",
		"http://localhost:8080",
		"http://127.0.0.1:11434",
		"http://127.0.0.2:8000",
		"http://127.255.255.254:9000",
		"http://[::1]:11434",
		"https://localhost:443",
	}
	for _, a := range localAddrs {
		p, err := ParseProvider(a, "")
		if err != nil {
			t.Fatal(err)
		}
		if !p.Local() {
			t.Errorf("expected %s to be Local(), got false", a)
		}
	}

	remoteAddrs := []string{
		"http://192.168.1.20:8080",
		"http://10.0.0.5:8000",
		"http://172.16.0.1:8080",
		"http://pi.lan:11434",
		"http://laptop.local:11434",
		"https://api.openai.com/v1",
		"http://0.0.0.0:11434",
		"http://8.8.8.8:8080",
	}
	for _, a := range remoteAddrs {
		p, err := ParseProvider(a, "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Local() {
			t.Errorf("expected %s to NOT be Local(), got true", a)
		}
	}
}

func TestProviderCleartextKeySecurity(t *testing.T) {
	// 1. Insecure: Plain HTTP to public Internet with key must be rejected
	insecureCases := []string{
		"openai@http://api.example.com",
		"openai@http://8.8.8.8:8000",
		"openai@http://api.together.xyz/v1",
		"openai@http://openai.public.cloud:443",
	}
	for _, u := range insecureCases {
		p, err := ParseProvider(u, "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Open("secret-api-key", RetryPolicy{})
		if !errors.Is(err, ErrInsecureKey) {
			t.Errorf("%s: expected ErrInsecureKey, got: %v", u, err)
		}
	}

	// 2. Secure: HTTPS with key is always allowed anywhere
	secureHTTPS := []string{
		"openai@https://api.openai.com/v1",
		"openai@https://api.together.xyz",
		"openai@https://8.8.8.8:8443",
	}
	for _, u := range secureHTTPS {
		p, err := ParseProvider(u, "")
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Open("secret-api-key", RetryPolicy{})
		if err != nil || c == nil {
			t.Errorf("%s: expected success over https, got error: %v", u, err)
		}
	}

	// 3. Secure: Plain HTTP with key is allowed on localhost, RFC1918, .lan, .local, single-label
	securePrivateHTTP := []string{
		"openai@http://localhost:8000",
		"openai@http://127.0.0.1:8000",
		"openai@http://192.168.1.10:8000",
		"openai@http://10.0.1.5:8000",
		"openai@http://172.16.5.5:8000",
		"openai@http://pi-server.lan:8000",
		"openai@http://cluster.local:8000",
		"openai@http://home.home.arpa:8000",
		"openai@http://singlename:8000",
	}
	for _, u := range securePrivateHTTP {
		p, err := ParseProvider(u, "")
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Open("internal-token", RetryPolicy{Attempts: 1})
		if err != nil || c == nil {
			t.Errorf("%s: expected allowed on private network, got error: %v", u, err)
		}
	}

	// 4. Empty key: Plain HTTP without key is allowed anywhere (e.g. public endpoints with no auth)
	publicNoKey := "openai@http://8.8.8.8:8000"
	p, _ := ParseProvider(publicNoKey, "")
	c, err := p.Open("", RetryPolicy{})
	if err != nil || c == nil {
		t.Errorf("empty key on plain http should be allowed, got: %v", err)
	}
}

func TestProviderOpenRemoteWrapsWithRetryLocalDoesNot(t *testing.T) {
	// Local provider: returns unwrapped client
	pLocal, _ := ParseProvider("ollama@http://127.0.0.1:11434", "")
	cLocal, err := pLocal.Open("", RetryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, isRetrying := cLocal.(*retrying); isRetrying {
		t.Error("local provider should NOT be wrapped in WithRetry")
	}

	// Remote provider: returns retrying client
	pRemote, _ := ParseProvider("ollama@http://192.168.1.200:11434", "")
	cRemote, err := pRemote.Open("", RetryPolicy{Attempts: 3, Base: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, isRetrying := cRemote.(*retrying); !isRetrying {
		t.Error("remote provider MUST be wrapped in WithRetry")
	}
}

func TestProviderURLEdgeCases(t *testing.T) {
	// IPv6 edge cases
	pIPv6, err := ParseProvider("http://[::1]:11434", "")
	if err != nil {
		t.Fatalf("unexpected error for IPv6 loopback: %v", err)
	}
	if !pIPv6.Local() {
		t.Errorf("IPv6 [::1] should be recognized as local")
	}

	// Trailing slashes stripped cleanly
	pSlash, err := ParseProvider("openai@https://api.openai.com/v1////", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pSlash.URL != "https://api.openai.com/v1" {
		t.Errorf("expected trailing slashes stripped, got %q", pSlash.URL)
	}

	// Uppercase host normalized
	pUpper, err := ParseProvider("ollama@HTTP://LOCALHOST:11434", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pUpper.Local() {
		t.Errorf("HTTP://LOCALHOST should be recognized as local")
	}
}
