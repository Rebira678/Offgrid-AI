package api

import (
	"strings"
	"testing"
)

func TestExoticRoutesAndMethodRejections(t *testing.T) {
	r := newRig(t, nil)

	// Disallowed methods on /v1/tasks
	for _, m := range []string{"PUT", "DELETE"} {
		rp := r.do(m, "/v1/tasks", nil, nil)
		if rp.code != 405 {
			t.Fatalf("expected 405 for %s /v1/tasks, got %d", m, rp.code)
		}
		allow := rp.h.Get("Allow")
		if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
			t.Fatalf("expected Allow header to contain GET and POST, got %q", allow)
		}
	}

	// Path traversal attempt in route: ServeMux redirects (307) or rejects (404/400)
	rp := r.do("GET", "/v1/tasks/../../etc/passwd", nil, nil)
	if rp.code != 404 && rp.code != 400 && rp.code != 307 {
		t.Fatalf("expected 404, 400, or 307 for path traversal, got %d", rp.code)
	}

	// Nonexistent subroute under /v1
	rp = r.do("GET", "/v1/nonexistent/route", nil, nil)
	if rp.code != 404 {
		t.Fatalf("expected 404 for nonexistent route, got %d", rp.code)
	}
}
