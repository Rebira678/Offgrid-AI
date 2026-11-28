package phone

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The stable API is on the phone server behind the phone's own session, with the same CSRF rules.
func TestV1IsBehindThePhoneSessionAndTheSameGuard(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("fix pump")
	_, ck := r.pair("Ana's phone")
	hdr := map[string]string{ActionHeader: "1", "Content-Type": "application/json"}

	for _, p := range []string{"/v1/tasks", "/v1/info", "/v1/openapi.json", "/v0/anything"} {
		if rp := r.do("GET", p, nil, "", nil); rp.code != 401 || rp.errCode(t) != "unpaired" {
			t.Errorf("%s without a session: %d %s", p, rp.code, rp.body)
		}
	}
	if rp := r.do("GET", "/v1/tasks", nil, "garbage", nil); rp.code != 401 {
		t.Errorf("a forged cookie: %d", rp.code)
	}

	rp := r.do("GET", "/v1/tasks", nil, ck, nil)
	if rp.code != 200 || !strings.Contains(rp.body, "fix pump") {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if rp := r.do("GET", "/v1/openapi.json", nil, ck, nil); rp.code != 200 || !strings.Contains(rp.body, `"openapi"`) {
		t.Fatalf("%d", rp.code)
	}
	if rp := r.do("POST", "/v1/tasks", map[string]any{"title": "from the api"}, ck, hdr); rp.code != 201 {
		t.Fatalf("create: %d %s", rp.code, rp.body)
	}
	// PATCH works on /v1 with the header, and is refused without it, from another origin, or from another site
	patch := map[string]any{"status": "done"}
	if rp := r.do("PATCH", "/v1/tasks/"+tk.ID, patch, ck, nil); rp.code != 403 || rp.errCode(t) != "missing_header" {
		t.Fatalf("PATCH without the header: %d %s", rp.code, rp.body)
	}
	if rp := r.do("PATCH", "/v1/tasks/"+tk.ID, patch, ck, map[string]string{ActionHeader: "1", "Origin": "https://evil.example"}); rp.code != 403 {
		t.Fatalf("cross-origin PATCH: %d", rp.code)
	}
	if rp := r.do("PATCH", "/v1/tasks/"+tk.ID, patch, ck, map[string]string{ActionHeader: "1", "Sec-Fetch-Site": "cross-site"}); rp.code != 403 {
		t.Fatalf("cross-site PATCH: %d", rp.code)
	}
	if rp := r.do("PATCH", "/v1/tasks/"+tk.ID, patch, "", hdr); rp.code != 401 {
		t.Fatalf("PATCH with no session: %d", rp.code)
	}
	if rp := r.do("PATCH", "/v1/tasks/"+tk.ID, patch, ck, hdr); rp.code != 200 {
		t.Fatalf("PATCH: %d %s", rp.code, rp.body)
	}
	got, _ := r.st.Get(context.Background(), tk.ID)
	if got.Status != "done" {
		t.Fatalf("%v", got.Status)
	}
	// other methods are still refused, and PATCH is still refused outside the versioned API
	for _, m := range []string{"PUT", "DELETE", "OPTIONS"} {
		if rp := r.do(m, "/v1/tasks", nil, ck, hdr); rp.code != 405 || rp.h.Get("Allow") != "GET, HEAD, POST, PATCH" {
			t.Errorf("%s: %d %q", m, rp.code, rp.h.Get("Allow"))
		}
	}
	if rp := r.do("PATCH", "/api/state", patch, ck, hdr); rp.code != 405 || rp.h.Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("PATCH on the phone's own API: %d %q", rp.code, rp.h.Get("Allow"))
	}
	// a phone does not get bulk import
	if rp := r.do("POST", "/v0/import/tasks.csv", []byte("title\nx\n"), ck, hdr); rp.code != 404 {
		t.Fatalf("import on the phone server: %d %s", rp.code, rp.body)
	}
	// and an unknown versioned path is the API's JSON 404, not a page
	if rp := r.do("GET", "/v1/nothing", nil, ck, nil); rp.code != 404 || rp.errCode(t) != "not_found" {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
}

func TestV1CutsOffARevokedPhone(t *testing.T) {
	r := newRig(t, nil)
	p, ck := r.pair("lost phone")
	if rp := r.do("GET", "/v1/tasks", nil, ck, nil); rp.code != 200 {
		t.Fatal(rp.code)
	}
	if _, err := r.st.Revoke(context.Background(), mustDev(t, p)); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(memberTTL + time.Second)
	if rp := r.do("GET", "/v1/tasks", nil, ck, nil); rp.code != 401 {
		t.Fatalf("a revoked phone reached /v1: %d", rp.code)
	}
}

func TestV1AttachmentLimitFollowsThePhoneConfigAndV0KeepsItsOwn404(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxPhoto = 2048 })
	tk := r.mk("t")
	_, ck := r.pair("p")
	big := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, make([]byte, 5000)...)
	rp := r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", big, ck, map[string]string{ActionHeader: "1", "Content-Type": "image/jpeg", "X-Offgrid-Name": "a.jpg"})
	if rp.code != 413 {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	ok := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, make([]byte, 1000)...)
	if rp := r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", ok, ck, map[string]string{ActionHeader: "1", "Content-Type": "image/jpeg", "X-Offgrid-Name": "a.jpg"}); rp.code != 201 {
		t.Fatalf("a photo under the phone limit: %d %s", rp.code, rp.body)
	}
	if rp := r.do("PATCH", "/v0/nothing", map[string]any{}, ck, map[string]string{ActionHeader: "1"}); rp.code == 405 {
		t.Fatalf("PATCH on /v0 must reach the API: %d", rp.code)
	}
}
