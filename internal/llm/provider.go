package llm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"offgrid/internal/ollama"
)

// Provider kinds. Anything that speaks "/v1/chat/completions" is KindOpenAI; llama.cpp's own server is
// the same protocol with no key, so it has its own name only to make a configuration readable.
const (
	KindOllama   = "ollama"
	KindLlamaCpp = "llamacpp"
	KindOpenAI   = "openai"
)

// ErrInsecureKey means an API key would have been sent in clear text to a host that is not on this
// machine or a private network. The fix is an https URL, or no key.
var ErrInsecureKey = errors.New("llm: refusing to send an API key over plain http to a host outside this machine and the local network")

// Provider is a parsed provider spec.
type Provider struct {
	Kind string
	URL  string // no trailing slash
}

// ParseProvider reads one setting that names both the kind of server and where it is:
//
//	http://127.0.0.1:11434            Ollama (a bare address is Ollama, as it always was)
//	ollama@http://pi-full.lan:11434   Ollama on another node
//	llamacpp@http://192.168.1.20:8080 llama.cpp's server (llama-server), here on the LAN
//	openai@https://api.example.com    any OpenAI-compatible endpoint (vLLM, LM Studio, a hosted API)
//
// A scheme is added (http) when the address has none. The empty spec means defaultURL as Ollama.
func ParseProvider(spec, defaultURL string) (Provider, error) {
	spec = strings.TrimSpace(spec)
	kind := KindOllama
	if i := strings.Index(spec, "@"); i > 0 && !strings.Contains(spec[:i], "/") {
		switch strings.ToLower(spec[:i]) {
		case "ollama":
			kind = KindOllama
		case "llamacpp", "llama.cpp", "llama-server":
			kind = KindLlamaCpp
		case "openai", "openai-compat", "vllm", "lmstudio":
			kind = KindOpenAI
		default:
			return Provider{}, fmt.Errorf("llm: unknown provider %q (use ollama, llamacpp or openai, as kind@url)", spec[:i])
		}
		spec = spec[i+1:]
	}
	if spec == "" {
		spec = defaultURL
	}
	if spec == "" {
		return Provider{}, errors.New("llm: no server address given")
	}
	if !strings.Contains(spec, "://") {
		spec = "http://" + spec
	}
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Provider{}, fmt.Errorf("llm: %q is not an http or https address", spec)
	}
	return Provider{Kind: kind, URL: strings.TrimRight(u.String(), "/")}, nil
}

// Local reports whether the server is on this machine.
func (p Provider) Local() bool {
	u, err := url.Parse(p.URL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// onPrivateNetwork reports whether a plain-http key would stay on this machine or the local network:
// loopback, RFC 1918 and link-local addresses, and ".lan", ".local" and single-label names.
func (p Provider) onPrivateNetwork() bool {
	u, err := url.Parse(p.URL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if p.Local() {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return !strings.Contains(h, ".") || strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".home.arpa")
}

// Open builds the client. A remote provider (anything not on this machine) is wrapped in WithRetry
// under policy; a local one is not, because a local server that is down will not come back in two
// seconds and the router's answer (defer to the queue) is the right one at once.
func (p Provider) Open(key string, policy RetryPolicy) (Client, error) {
	if key != "" && strings.HasPrefix(p.URL, "http://") && !p.onPrivateNetwork() {
		return nil, ErrInsecureKey
	}
	var c Client
	switch p.Kind {
	case KindOllama:
		c = NewOllama(ollama.NewClient(p.URL))
	case KindLlamaCpp, KindOpenAI:
		c = NewOpenAICompat(p.URL, key)
	default:
		return nil, fmt.Errorf("llm: unknown provider kind %q", p.Kind)
	}
	if p.Local() {
		return c, nil
	}
	return WithRetry(c, policy), nil
}
