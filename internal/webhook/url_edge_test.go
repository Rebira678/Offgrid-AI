package webhook

import (
	"testing"
)

func TestWebhookURLValidationEdges(t *testing.T) {
	cases := []struct {
		url   string
		valid bool
	}{
		{"http://[::1]:8080/hook", true},
		{"https://[2001:db8::1]/webhook", true},
		{"http://127.0.0.1:9090/events", true},
		{"https://api.example.com/v1/webhook?source=site", true},
		{"http://localhost:3000/webhook", true},
		{"HTTP://example.com/hook", true},
		{"HTTPS://example.com/hook", true},
		// Invalid cases
		{"ftp://example.com/hook", false},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
		{"https://user:password@example.com/webhook", false},
		{"https://example.com/webhook#fragment", false},
		{"/relative/path/webhook", false},
		{"http://", false},
		{"", false},
	}

	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			err := ValidateURL(c.url)
			if (err == nil) != c.valid {
				t.Fatalf("ValidateURL(%q) valid=%v, but got err: %v", c.url, c.valid, err)
			}
		})
	}
}

func TestURLPortBoundaries(t *testing.T) {
	// Port 65535 is highest valid TCP port
	if err := ValidateURL("https://example.com:65535/hook"); err != nil {
		t.Fatalf("expected port 65535 to be valid, got: %v", err)
	}

	// Port 70000 is invalid
	if err := ValidateURL("https://example.com:70000/hook"); err == nil {
		t.Fatalf("expected port 70000 to be invalid")
	}
}
