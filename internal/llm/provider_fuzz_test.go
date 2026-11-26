package llm

import (
	"testing"
	"time"
)

func FuzzParseProvider(f *testing.F) {
	seeds := []struct {
		spec       string
		defaultURL string
	}{
		{"http://127.0.0.1:11434", ""},
		{"ollama@http://pi-full.lan:11434", ""},
		{"llamacpp@http://192.168.1.20:8080", ""},
		{"llama.cpp@http://10.0.0.5:8080", ""},
		{"openai@https://api.openai.com", "http://127.0.0.1:11434"},
		{"vllm@http://localhost:8000", ""},
		{"lmstudio@http://127.0.0.1:1234", ""},
		{"", "http://127.0.0.1:11434"},
		{"bare-host:11434", ""},
		{"[::1]:11434", ""},
		{"https://user:pass@example.com/v1", ""},
		{"invalid://url", ""},
		{"unknown@http://localhost", ""},
	}
	for _, s := range seeds {
		f.Add(s.spec, s.defaultURL)
	}

	f.Fuzz(func(t *testing.T, spec, defaultURL string) {
		p, err := ParseProvider(spec, defaultURL)
		if err == nil {
			// Invariants when parsing succeeds:
			if p.Kind == "" {
				t.Fatalf("successful parse must have non-empty kind: %+v", p)
			}
			if p.URL == "" {
				t.Fatalf("successful parse must have non-empty URL: %+v", p)
			}
			// Calling methods must never panic
			_ = p.Local()
			_ = p.onPrivateNetwork()
		}
	})
}

func BenchmarkParseProvider(b *testing.B) {
	cases := []string{
		"http://127.0.0.1:11434",
		"ollama@http://pi-full.lan:11434",
		"llamacpp@http://192.168.1.20:8080",
		"openai@https://api.openai.com/v1",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		spec := cases[i%len(cases)]
		p, err := ParseProvider(spec, "http://127.0.0.1:11434")
		if err != nil {
			b.Fatal(err)
		}
		_ = p.Local()
		_ = p.onPrivateNetwork()
	}
}

func BenchmarkRetryPolicyWait(b *testing.B) {
	p := RetryPolicy{
		Base: 100 * time.Millisecond,
		Max:  2 * time.Second,
		Rand: func() float64 { return 0.5 },
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Wait(i % 10)
	}
}
