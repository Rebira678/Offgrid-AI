package api

import (
	"strings"
	"testing"
)

func TestJSONPayloadEdgeCases(t *testing.T) {
	r := newRig(t, nil)
	hdr := map[string]string{ActionHeader: "1"}

	// 1. Trailing garbage after valid JSON
	rp := r.do("POST", "/v1/tasks", `{"title":"test trailing"} trailing_garbage`, hdr)
	if rp.code != 400 {
		t.Fatalf("expected 400 on trailing garbage, got %d: %s", rp.code, rp.body)
	}

	// 2. Non-object JSON
	nonObjects := []string{`["title", "array"]`, `12345`, `"just string"`, `true`}
	for _, no := range nonObjects {
		rp := r.do("POST", "/v1/tasks", no, hdr)
		if rp.code != 400 {
			t.Fatalf("expected 400 for non-object %q, got %d: %s", no, rp.code, rp.body)
		}
	}
	// "null" decodes to zero-value struct, which fails validation with 422
	if rp := r.do("POST", "/v1/tasks", "null", hdr); rp.code != 422 {
		t.Fatalf("expected 422 for null task, got %d: %s", rp.code, rp.body)
	}

	// 3. Unknown fields rejected
	rp = r.do("POST", "/v1/tasks", `{"title":"valid", "injected_field": "disallowed"}`, hdr)
	if rp.code != 400 {
		t.Fatalf("expected 400 for disallowed field, got %d: %s", rp.code, rp.body)
	}

	// 4. Oversized body > 64KB
	oversized := `{"title":"` + strings.Repeat("a", 70000) + `"}`
	rp = r.do("POST", "/v1/tasks", oversized, hdr)
	if rp.code != 413 {
		t.Fatalf("expected 413 for oversized body, got %d: %s", rp.code, rp.body)
	}

	// Create a real task to test PATCH edges
	var cr createReply
	r.do("POST", "/v1/tasks", map[string]string{"title": "task for patch"}, hdr).into(t, &cr)
	taskID := cr.Task.ID

	// 5. Empty PATCH body
	rp = r.do("PATCH", "/v1/tasks/"+taskID, `{}`, hdr)
	if rp.code != 422 {
		t.Fatalf("expected 422 for empty patch, got %d: %s", rp.code, rp.body)
	}

	// 6. PATCH with read-only fields
	rp = r.do("PATCH", "/v1/tasks/"+taskID, `{"id":"new_id"}`, hdr)
	if rp.code != 400 {
		t.Fatalf("expected 400 for read-only field in patch, got %d: %s", rp.code, rp.body)
	}

	// 7. PATCH with invalid status value
	rp = r.do("PATCH", "/v1/tasks/"+taskID, `{"status":"completed"}`, hdr)
	if rp.code != 422 {
		t.Fatalf("expected 422 for invalid status in patch, got %d: %s", rp.code, rp.body)
	}

	// 8. Add note with empty string or whitespace only
	rp = r.do("POST", "/v1/tasks/"+taskID+"/notes", map[string]string{"text": ""}, hdr)
	if rp.code != 422 {
		t.Fatalf("expected 422 for empty note text, got %d: %s", rp.code, rp.body)
	}
	rp = r.do("POST", "/v1/tasks/"+taskID+"/notes", map[string]string{"text": "    \t\n   "}, hdr)
	if rp.code != 422 {
		t.Fatalf("expected 422 for whitespace note text, got %d: %s", rp.code, rp.body)
	}
}
