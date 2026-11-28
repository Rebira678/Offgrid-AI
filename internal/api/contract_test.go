package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

var t0 = time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)

type clk struct{ t time.Time }

func (c *clk) now() time.Time { c.t = c.t.Add(time.Second); return c.t } // every write gets a later stamp

type rig struct {
	t      *testing.T
	st     *sqlstore.Store
	h      http.Handler
	routes []Route
}

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	c := &clk{t0}
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"), sqlstore.Options{NodeID: "site", Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{Store: st, AllowImport: true, Now: func() time.Time { return t0 }}
	if mod != nil {
		mod(&cfg)
	}
	h, routes := Handler(cfg)
	return &rig{t: t, st: st, h: h, routes: routes}
}

func newFrozenRig(t *testing.T, at time.Time) *rig {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"), sqlstore.Options{NodeID: "site", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, routes := Handler(Config{Store: st})
	return &rig{t: t, st: st, h: h, routes: routes}
}

type reply struct {
	code int
	h    http.Header
	body []byte
}

func (r *rig) do(method, path string, body any, hdr map[string]string) reply {
	r.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	res := w.Result()
	b, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, b}
}

func (rp reply) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(rp.body, v); err != nil {
		t.Fatalf("not JSON: %q", rp.body)
	}
}

func (rp reply) errCode(t *testing.T) string {
	t.Helper()
	var e Error
	rp.into(t, &e)
	return e.Code
}

// ---------------------------------------------------------------- a small validator for the part of OpenAPI/JSON Schema we use

type spec struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := json.Unmarshal(Spec, &s); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	return s
}

func (s spec) resolve(sc map[string]any) map[string]any {
	if ref, ok := sc["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		m, ok := s.Components.Schemas[name].(map[string]any)
		if !ok {
			panic("dangling $ref " + ref)
		}
		return m
	}
	return sc
}

func (s spec) validate(sc map[string]any, v any, path string) error {
	sc = s.resolve(sc)
	if v == nil {
		if sc["nullable"] == true {
			return nil
		}
		return fmt.Errorf("%s: null where %v is required", path, sc["type"])
	}
	if en, ok := sc["enum"].([]any); ok {
		found := false
		for _, e := range en {
			if e == v {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", path, v, en)
		}
	}
	switch sc["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: not an object", path)
		}
		props, _ := sc["properties"].(map[string]any)
		if req, ok := sc["required"].([]any); ok {
			for _, r := range req {
				if _, has := m[r.(string)]; !has {
					return fmt.Errorf("%s: missing %q", path, r)
				}
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p, known := props[k].(map[string]any)
			if !known {
				if sc["additionalProperties"] == true {
					continue
				}
				return fmt.Errorf("%s: field %q is not in the contract", path, k)
			}
			if err := s.validate(p, m[k], path+"."+k); err != nil {
				return err
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: not an array", path)
		}
		for i, e := range a {
			if err := s.validate(sc["items"].(map[string]any), e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: not a string", path)
		}
		if sc["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
				return fmt.Errorf("%s: %q is not an RFC 3339 time", path, str)
			}
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: not an integer", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: not a boolean", path)
		}
	}
	return nil
}

func templateRE(tpl string) *regexp.Regexp {
	parts := regexp.MustCompile(`\{[^}]+\}`).Split(tpl, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[^/]+") + "$")
}

// ---------------------------------------------------------------- the contract test

type contractCase struct {
	name, method, path string
	body               any
	hdr                map[string]string
	want               int
}

type tracker struct {
	t       *testing.T
	r       *rig
	s       spec
	covered map[string]bool
	codes   map[string]bool // "METHOD template status"
}

func (tr *tracker) call(c contractCase) reply {
	tr.t.Helper()
	hdr := map[string]string{}
	if c.method != "GET" {
		hdr[ActionHeader] = "1"
	}
	for k, v := range c.hdr {
		hdr[k] = v
	}
	rp := tr.r.do(c.method, c.path, c.body, hdr)
	if rp.code != c.want {
		tr.t.Fatalf("%s: %s %s = %d, want %d: %s", c.name, c.method, c.path, rp.code, c.want, rp.body)
	}
	pathOnly := strings.SplitN(c.path, "?", 2)[0]
	var tpl string
	for p, ops := range tr.s.Paths {
		if _, ok := ops[strings.ToLower(c.method)]; ok && templateRE(p).MatchString(pathOnly) {
			tpl = p
		}
	}
	if tpl == "" {
		tr.t.Fatalf("%s: no operation for %s %s in openapi.json", c.name, c.method, pathOnly)
	}
	var op struct {
		Stability string                     `json:"x-stability"`
		Responses map[string]json.RawMessage `json:"responses"`
	}
	json.Unmarshal(tr.s.Paths[tpl][strings.ToLower(c.method)], &op)
	rawResp, declared := op.Responses[fmt.Sprint(rp.code)]
	if !declared {
		tr.t.Fatalf("%s: %s %s answered %d, which the contract does not declare", c.name, c.method, tpl, rp.code)
	}
	var rs struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	}
	json.Unmarshal(rawResp, &rs)
	if len(rs.Content) > 0 {
		mt, _, _ := mime.ParseMediaType(rp.h.Get("Content-Type"))
		ct, ok := rs.Content[mt]
		if !ok {
			ct, ok = rs.Content["*/*"]
		}
		if !ok {
			tr.t.Fatalf("%s: %s %s answered %q, the contract declares %v", c.name, c.method, tpl, mt, rs.Content)
		}
		if mt == "application/json" {
			var v any
			if err := json.Unmarshal(rp.body, &v); err != nil {
				tr.t.Fatalf("%s: body is not JSON: %q", c.name, rp.body)
			}
			if err := tr.s.validate(ct.Schema, v, "$"); err != nil {
				tr.t.Fatalf("%s: %s %s %d: %v\n%s", c.name, c.method, tpl, rp.code, err, rp.body)
			}
		}
	}
	tr.covered[c.method+" "+tpl] = true
	tr.codes[fmt.Sprintf("%s %s %d", c.method, tpl, rp.code)] = true
	return rp
}

func TestEveryRouteAnswersAsTheContractSays(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxUpload = 5000 })
	tr := &tracker{t: t, r: r, s: loadSpec(t), covered: map[string]bool{}, codes: map[string]bool{}}
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{7}, 3000)...)

	tr.call(contractCase{"info", "GET", "/v1/info", nil, nil, 200})
	tr.call(contractCase{"spec", "GET", "/v1/openapi.json", nil, nil, 200})
	cr := tr.call(contractCase{"create", "POST", "/v1/tasks", map[string]any{"title": "fix pump", "site": "farm", "priority": "high"}, nil, 201})
	var created createReply
	cr.into(t, &created)
	id := created.Task.ID
	tr.call(contractCase{"create again", "POST", "/v1/tasks", map[string]any{"title": "fix pump", "site": "farm"}, nil, 200})
	tr.call(contractCase{"create no title", "POST", "/v1/tasks", map[string]any{"title": " "}, nil, 422})
	tr.call(contractCase{"create bad json", "POST", "/v1/tasks", `{`, nil, 400})
	tr.call(contractCase{"create too big", "POST", "/v1/tasks", strings.Repeat(" ", MaxJSONBody+1), nil, 413})
	tr.call(contractCase{"list", "GET", "/v1/tasks", nil, nil, 200})
	tr.call(contractCase{"list filtered", "GET", "/v1/tasks?status=open&priority=high&site=farm&limit=1", nil, nil, 200})
	tr.call(contractCase{"list bad limit", "GET", "/v1/tasks?limit=0", nil, nil, 400})
	tr.call(contractCase{"get", "GET", "/v1/tasks/" + id, nil, nil, 200})
	tr.call(contractCase{"attach too large", "POST", "/v1/tasks/" + id + "/attachments", append(jpeg, jpeg...), map[string]string{"Content-Type": "image/jpeg"}, 413})
	tr.call(contractCase{"get missing", "GET", "/v1/tasks/nope", nil, nil, 404})
	tr.call(contractCase{"patch", "PATCH", "/v1/tasks/" + id, map[string]any{"status": "done"}, nil, 200})
	tr.call(contractCase{"patch bad", "PATCH", "/v1/tasks/" + id, map[string]any{"status": "finished"}, nil, 422})
	tr.call(contractCase{"patch bad json", "PATCH", "/v1/tasks/" + id, `[]`, nil, 400})
	tr.call(contractCase{"patch missing", "PATCH", "/v1/tasks/nope", map[string]any{"status": "done"}, nil, 404})
	tr.call(contractCase{"patch too big", "PATCH", "/v1/tasks/" + id, strings.Repeat(" ", MaxJSONBody+1), nil, 413})
	tr.call(contractCase{"note", "POST", "/v1/tasks/" + id + "/notes", map[string]any{"text": "hello"}, nil, 201})
	tr.call(contractCase{"note empty", "POST", "/v1/tasks/" + id + "/notes", map[string]any{"text": " "}, nil, 422})
	tr.call(contractCase{"note missing", "POST", "/v1/tasks/nope/notes", map[string]any{"text": "x"}, nil, 404})
	tr.call(contractCase{"note bad json", "POST", "/v1/tasks/" + id + "/notes", `x`, nil, 400})
	tr.call(contractCase{"note too big", "POST", "/v1/tasks/" + id + "/notes", strings.Repeat(" ", MaxJSONBody+1), nil, 413})

	ar := tr.call(contractCase{"attach", "POST", "/v1/tasks/" + id + "/attachments", jpeg, map[string]string{"Content-Type": "image/jpeg", "X-Offgrid-Name": "pump%201.jpg"}, 201})
	var att Attachment
	ar.into(t, &att)
	tr.call(contractCase{"attach no type", "POST", "/v1/tasks/" + id + "/attachments", jpeg, map[string]string{"Content-Type": ""}, 415})
	tr.call(contractCase{"attach not an image", "POST", "/v1/tasks/" + id + "/attachments", []byte("hello hello hello hello hello"), map[string]string{"Content-Type": "image/jpeg"}, 415})
	tr.call(contractCase{"attach empty", "POST", "/v1/tasks/" + id + "/attachments", []byte{}, map[string]string{"Content-Type": "image/jpeg"}, 400})
	tr.call(contractCase{"attach missing", "POST", "/v1/tasks/nope/attachments", jpeg, map[string]string{"Content-Type": "image/jpeg"}, 404})
	tr.call(contractCase{"attachments", "GET", "/v1/tasks/" + id + "/attachments", nil, nil, 200})
	tr.call(contractCase{"attachments missing", "GET", "/v1/tasks/nope/attachments", nil, nil, 404})
	tr.call(contractCase{"attachment bytes", "GET", "/v1/tasks/" + id + "/attachments/" + att.Hash, nil, nil, 200})
	tr.call(contractCase{"attachment bytes missing", "GET", "/v1/tasks/" + id + "/attachments/" + strings.Repeat("0", 64), nil, nil, 404})

	hi := task.PriorityLow
	pend, _, err := r.st.Suggest(context.Background(), task.Draft{Task: id, Priority: &hi, By: "model", Key: "a", Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	t2, _, _ := r.st.Create(context.Background(), task.NewTask{Title: "paint", Priority: task.PriorityMedium})
	pend2, _, _ := r.st.Suggest(context.Background(), task.Draft{Task: t2.ID, Priority: &hi, By: "model", Key: "b"})
	tr.call(contractCase{"suggestions", "GET", "/v1/suggestions", nil, nil, 200})
	tr.call(contractCase{"suggestions all", "GET", "/v1/suggestions?all=true", nil, nil, 200})
	tr.call(contractCase{"suggestions bad", "GET", "/v1/suggestions?all=maybe", nil, nil, 400})
	tr.call(contractCase{"accept", "POST", "/v1/suggestions/" + pend.ID + "/accept", nil, nil, 200})
	tr.call(contractCase{"accept again", "POST", "/v1/suggestions/" + pend.ID + "/accept", nil, nil, 409})
	tr.call(contractCase{"dismiss", "POST", "/v1/suggestions/" + pend2.ID + "/dismiss", nil, nil, 200})
	tr.call(contractCase{"dismiss again", "POST", "/v1/suggestions/" + pend2.ID + "/dismiss", nil, nil, 409})
	tr.call(contractCase{"dismiss missing", "POST", "/v1/suggestions/zzz/dismiss", nil, nil, 404})
	tr.call(contractCase{"accept missing", "POST", "/v1/suggestions/zzz/accept", nil, nil, 404})

	tr.call(contractCase{"export", "GET", "/v1/export/tasks.csv", nil, nil, 200})
	tr.call(contractCase{"import dry", "POST", "/v0/import/tasks.csv?dry_run=true", "title\nnew one\n", map[string]string{"Content-Type": "text/csv"}, 200})
	tr.call(contractCase{"import", "POST", "/v0/import/tasks.csv", "title\nnew two\n", nil, 200})
	tr.call(contractCase{"import no header", "POST", "/v0/import/tasks.csv", "name\nx\n", nil, 422})
	tr.call(contractCase{"import bad flag", "POST", "/v0/import/tasks.csv?dry_run=x", "title\nx\n", nil, 400})
	tr.call(contractCase{"import too big", "POST", "/v0/import/tasks.csv", "title\n" + strings.Repeat("a", MaxImportBody), nil, 413})
	tr.call(contractCase{"import too many rows", "POST", "/v0/import/tasks.csv", "title\n" + strings.Repeat("a\n", MaxImportRows+1), nil, 422})

	// every operation in the file was called, and every status it declares for a route we can provoke was seen
	for p, ops := range tr.s.Paths {
		for m := range ops {
			if !tr.covered[strings.ToUpper(m)+" "+p] {
				t.Errorf("openapi.json lists %s %s but no test called it", strings.ToUpper(m), p)
			}
		}
	}
	// a status in the file that no request produced is a promise nobody keeps (500 needs a broken store: see below)
	for p, ops := range tr.s.Paths {
		for m, raw := range ops {
			var o struct {
				Responses map[string]json.RawMessage `json:"responses"`
			}
			json.Unmarshal(raw, &o)
			for code := range o.Responses {
				if code != "500" && !tr.codes[fmt.Sprintf("%s %s %s", strings.ToUpper(m), p, code)] {
					t.Errorf("%s %s declares %s but no test saw it", strings.ToUpper(m), p, code)
				}
			}
		}
	}
	// the contract and the server list the same routes, and the version prefix matches the stability
	got := map[string]bool{}
	for _, rt := range r.routes {
		got[rt.Method+" "+rt.Path] = true
		if (strings.HasPrefix(rt.Path, "/v1/") && rt.Stability != "stable") || (strings.HasPrefix(rt.Path, "/v0/") && rt.Stability != "v0") {
			t.Errorf("%s %s is marked %s", rt.Method, rt.Path, rt.Stability)
		}
		if !strings.HasPrefix(rt.Path, "/v1/") && !strings.HasPrefix(rt.Path, "/v0/") {
			t.Errorf("%s %s is under no version", rt.Method, rt.Path)
		}
		if !tr.covered[rt.Method+" "+rt.Path] {
			t.Errorf("the server serves %s %s but openapi.json does not describe it", rt.Method, rt.Path)
		}
	}
	for k := range tr.covered {
		if !got[k] {
			t.Errorf("openapi.json lists %s but the server does not serve it", k)
		}
	}
	// the stability marker in the file agrees with the path
	for p, ops := range tr.s.Paths {
		for m, raw := range ops {
			var o struct {
				S string `json:"x-stability"`
			}
			json.Unmarshal(raw, &o)
			want := "stable"
			if strings.HasPrefix(p, "/v0/") {
				want = "v0"
			}
			if o.S != want {
				t.Errorf("%s %s is marked %q in the file, want %q", m, p, o.S, want)
			}
		}
	}
}

func TestUnknownRoutesAndWrongMethodsUseTheSameError(t *testing.T) {
	r := newRig(t, nil)
	s := loadSpec(t)
	errSchema := s.Components.Schemas["Error"].(map[string]any)
	for _, c := range []struct {
		method, path string
		code         int
		allow        string
	}{
		{"GET", "/v1/nope", 404, ""},
		{"GET", "/v1/", 404, ""},
		{"GET", "/v0/nope", 404, ""},
		{"DELETE", "/v1/tasks", 405, "GET, POST"},
		{"PUT", "/v1/tasks/x", 405, "GET, PATCH"},
		{"POST", "/v1/info", 405, "GET"},
		{"GET", "/v1/suggestions/x/accept", 405, "POST"},
		{"GET", "/v0/import/tasks.csv", 405, "POST"},
	} {
		rp := r.do(c.method, c.path, nil, map[string]string{ActionHeader: "1"})
		if rp.code != c.code {
			t.Errorf("%s %s: %d %s", c.method, c.path, rp.code, rp.body)
			continue
		}
		var v any
		json.Unmarshal(rp.body, &v)
		if err := s.validate(errSchema, v, "$"); err != nil {
			t.Errorf("%s %s: %v", c.method, c.path, err)
		}
		if rp.h.Get("Allow") != c.allow {
			t.Errorf("%s %s: Allow %q want %q", c.method, c.path, rp.h.Get("Allow"), c.allow)
		}
	}
}

func TestImportIsNotMountedUnlessAllowed(t *testing.T) {
	r := newRig(t, func(c *Config) { c.AllowImport = false })
	if rp := r.do("POST", "/v0/import/tasks.csv", "title\nx\n", nil); rp.code != 404 {
		t.Fatalf("%d", rp.code)
	}
	for _, rt := range r.routes {
		if strings.HasPrefix(rt.Path, "/v0/") {
			t.Errorf("%s %s is mounted", rt.Method, rt.Path)
		}
	}
}

func TestTheSpecIsValidAndSelfContained(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(Spec, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi %v", doc["openapi"])
	}
	// every $ref resolves
	refs := regexp.MustCompile(`"\$ref":\s*"([^"]+)"`).FindAllStringSubmatch(string(Spec), -1)
	if len(refs) < 20 {
		t.Errorf("only %d refs", len(refs))
	}
	s := loadSpec(t)
	for _, m := range refs {
		name := strings.TrimPrefix(m[1], "#/components/schemas/")
		if _, ok := s.Components.Schemas[name]; !ok || name == m[1] {
			t.Errorf("dangling ref %s", m[1])
		}
	}
	// no schema is left unused
	for name := range s.Components.Schemas {
		if !strings.Contains(string(Spec), `"#/components/schemas/`+name+`"`) && name != "Event" {
			t.Errorf("schema %s is never used", name)
		}
	}
	// the error codes the server uses are all in the enum
	enum := s.Components.Schemas["Error"].(map[string]any)["properties"].(map[string]any)["code"].(map[string]any)["enum"].([]any)
	have := map[string]bool{}
	for _, e := range enum {
		have[e.(string)] = true
	}
	for _, c := range []string{"bad_request", "invalid", "not_found", "stale", "already_resolved", "too_large", "unsupported_media_type", "method_not_allowed", "internal"} {
		if !have[c] {
			t.Errorf("code %s is not in the contract", c)
		}
	}
}

func TestValidatorCatchesWhatItShould(t *testing.T) {
	s := loadSpec(t)
	task := s.Components.Schemas["Task"].(map[string]any)
	good := map[string]any{"id": "x", "title": "t", "site": "", "status": "open", "priority": "low", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z", "done_at": nil, "notes": []any{}}
	if err := s.validate(task, good, "$"); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(map[string]any){
		"missing field":  func(m map[string]any) { delete(m, "site") },
		"extra field":    func(m map[string]any) { m["extra"] = 1 },
		"bad enum":       func(m map[string]any) { m["status"] = "finished" },
		"wrong type":     func(m map[string]any) { m["title"] = 5.0 },
		"bad time":       func(m map[string]any) { m["created_at"] = "yesterday" },
		"null not ok":    func(m map[string]any) { m["title"] = nil },
		"bad array item": func(m map[string]any) { m["notes"] = []any{"x"} },
	} {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		mut(c)
		if s.validate(task, c, "$") == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

type brokenStore struct{ Store }

var errSecret = fmt.Errorf("disk on fire at /secret/path/node.db")

func (brokenStore) List(context.Context, task.Filter) ([]task.Task, error) { return nil, errSecret }
func (brokenStore) Get(context.Context, string) (task.Task, error)         { return task.Task{}, errSecret }
func (brokenStore) Info(context.Context) (sqlstore.Info, error)            { return sqlstore.Info{}, errSecret }
func (brokenStore) Suggestions(context.Context, bool) ([]task.Suggestion, error) {
	return nil, errSecret
}

func TestStoreFailuresAreInternalErrorsThatDoNotLeakDetails(t *testing.T) {
	r := newRig(t, nil)
	h, _ := Handler(Config{Store: brokenStore{r.st}, AllowImport: true})
	r.h = h
	s := loadSpec(t)
	errSchema := s.Components.Schemas["Error"].(map[string]any)
	for _, c := range []struct{ m, p string }{{"GET", "/v1/info"}, {"GET", "/v1/tasks"}, {"GET", "/v1/tasks/x"}, {"GET", "/v1/suggestions"}, {"GET", "/v1/export/tasks.csv"}, {"GET", "/v1/tasks/x/attachments"}} {
		rp := r.do(c.m, c.p, nil, nil)
		if rp.code != 500 || rp.errCode(t) != "internal" {
			t.Errorf("%s: %d %s", c.p, rp.code, rp.body)
		}
		if strings.Contains(string(rp.body), "secret") || strings.Contains(string(rp.body), "fire") {
			t.Errorf("%s leaked the failure: %s", c.p, rp.body)
		}
		var v any
		json.Unmarshal(rp.body, &v)
		if err := s.validate(errSchema, v, "$"); err != nil {
			t.Error(err)
		}
	}
}

func TestLocalGuard(t *testing.T) {
	r := newRig(t, nil)
	g := LocalGuard(r.h)
	do := func(method, host string, hdr map[string]string) int {
		req := httptest.NewRequest(method, "http://x/v1/info", nil)
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		return w.Code
	}
	act := map[string]string{ActionHeader: "1"}
	for name, c := range map[string]struct {
		method, host string
		hdr          map[string]string
		want         int
	}{
		"local read":             {"GET", "127.0.0.1:8090", nil, 200},
		"localhost read":         {"GET", "localhost:8090", nil, 200},
		"v6 loopback":            {"GET", "[::1]:8090", nil, 200},
		"rebound name":           {"GET", "evil.example", nil, 403},
		"lan address":            {"GET", "192.168.1.5:8090", nil, 403},
		"write without header":   {"POST", "127.0.0.1", nil, 403},
		"write with header":      {"POST", "127.0.0.1", act, 405}, // /v1/info takes GET only: the guard let it through
		"cross origin":           {"GET", "127.0.0.1:8090", map[string]string{"Origin": "https://evil.example"}, 403},
		"same origin":            {"GET", "127.0.0.1:8090", map[string]string{"Origin": "http://127.0.0.1:8090"}, 200},
		"cross-site fetch":       {"GET", "127.0.0.1", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		"same-site fetch":        {"GET", "127.0.0.1", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		"same-origin fetch":      {"GET", "127.0.0.1", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		"patch needs the header": {"PATCH", "127.0.0.1", nil, 403},
	} {
		if got := do(c.method, c.host, c.hdr); got != c.want {
			t.Errorf("%s: %d want %d", name, got, c.want)
		}
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1/v1/info", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if w.Header().Get(k) != want {
			t.Errorf("%s = %q", k, w.Header().Get(k))
		}
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("no policy")
	}
}
