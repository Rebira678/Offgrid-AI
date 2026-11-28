package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"math/rand"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"offgrid/internal/task"
)

func (r *rig) mk(title, site string, p task.Priority) task.Task {
	r.t.Helper()
	got, _, err := r.st.Create(context.Background(), task.NewTask{Title: title, Site: site, Priority: p})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func TestATaskIsShownAsTheContractPromises(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("fix pump", "farm", task.PriorityHigh)
	st := task.StatusDone
	r.st.Update(context.Background(), tk.ID, task.Changes{Status: &st})
	r.st.AddNote(context.Background(), tk.ID, "belt snapped")
	var got Task
	r.do("GET", "/v1/tasks/"+tk.ID, nil, nil).into(t, &got)
	if got.ID != tk.ID || got.Title != "fix pump" || got.Site != "farm" || got.Status != "done" || got.Priority != "high" {
		t.Fatalf("%+v", got)
	}
	if got.DoneAt == nil || got.DoneAt.Before(got.CreatedAt) || got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("times: %+v", got)
	}
	if len(got.Notes) != 1 || got.Notes[0].Text != "belt snapped" {
		t.Fatalf("%+v", got.Notes)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatal("times are UTC")
	}
	open := r.mk("paint", "", task.PriorityLow)
	var o Task
	rp := r.do("GET", "/v1/tasks/"+open.ID, nil, nil)
	rp.into(t, &o)
	if o.DoneAt != nil || !strings.Contains(string(rp.body), `"done_at":null`) || !strings.Contains(string(rp.body), `"notes":[]`) {
		t.Fatalf("an unfinished task has done_at null and notes []: %s", rp.body)
	}
}

func TestListingFiltersAndPaginatesWithoutLosingOrRepeatingATask(t *testing.T) {
	r := newRig(t, nil)
	var want []string
	for i := 0; i < 23; i++ {
		site, pr := "farm", task.PriorityLow
		if i%2 == 0 {
			site, pr = "house", task.PriorityHigh
		}
		want = append(want, r.mk(fmt.Sprintf("job %02d", i), site, pr).ID)
	}
	var got []string
	cursor := ""
	pages := 0
	for {
		path := "/v1/tasks?limit=5"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var l taskList
		r.do("GET", path, nil, nil).into(t, &l)
		pages++
		if len(l.Tasks) > 5 {
			t.Fatalf("page of %d", len(l.Tasks))
		}
		for _, x := range l.Tasks {
			got = append(got, x.ID)
		}
		if l.NextCursor == nil {
			break
		}
		if len(l.Tasks) != 5 {
			t.Fatalf("a short page with a cursor: %d", len(l.Tasks))
		}
		cursor = *l.NextCursor
		if pages > 10 {
			t.Fatal("never ends")
		}
		if pages == 2 { // a task added mid-listing is neither a repeat nor a hole in what was already listed
			want = append(want, r.mk("late arrival", "", task.PriorityLow).ID)
		}
	}
	if pages != 5 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages %d\n got %v\nwant %v", pages, got, want)
	}
	var h taskList
	r.do("GET", "/v1/tasks?priority=high&site=house", nil, nil).into(t, &h)
	if len(h.Tasks) != 12 || h.NextCursor != nil {
		t.Fatalf("%d", len(h.Tasks))
	}
	var exact taskList
	r.do("GET", "/v1/tasks?limit=500", nil, nil).into(t, &exact)
	if len(exact.Tasks) != 24 || exact.NextCursor != nil {
		t.Fatalf("%d %v", len(exact.Tasks), exact.NextCursor)
	}
	var one taskList
	r.do("GET", "/v1/tasks?limit=24", nil, nil).into(t, &one)
	if one.NextCursor != nil {
		t.Fatal("a page that holds everything has no next cursor")
	}
	for _, bad := range []string{"limit=501", "limit=-1", "limit=abc", "cursor=!!!", "cursor=" + "YWJj", "status=finished", "priority=urgent"} {
		if rp := r.do("GET", "/v1/tasks?"+bad, nil, nil); rp.code != 400 {
			t.Errorf("%s: %d", bad, rp.code)
		}
	}
	var empty taskList
	rp := r.do("GET", "/v1/tasks?site=nowhere", nil, nil)
	rp.into(t, &empty)
	if !strings.Contains(string(rp.body), `"tasks":[]`) || empty.NextCursor != nil {
		t.Fatalf("%s", rp.body)
	}
}

func TestCreateIsIdempotentAndPatchSaysWhetherAnythingChanged(t *testing.T) {
	r := newRig(t, nil)
	hdr := map[string]string{ActionHeader: "1"}
	var a, b createReply
	r.do("POST", "/v1/tasks", map[string]string{"title": "fix pump"}, hdr).into(t, &a)
	rp := r.do("POST", "/v1/tasks", map[string]string{"title": "fix pump"}, hdr)
	rp.into(t, &b)
	if !a.Created || b.Created || rp.code != 200 || a.Task.ID != b.Task.ID || a.Task.Priority != "medium" {
		t.Fatalf("%+v %+v", a, b)
	}
	var p patchReply
	r.do("PATCH", "/v1/tasks/"+a.Task.ID, map[string]string{"priority": "high"}, hdr).into(t, &p)
	if !p.Changed || p.Task.Priority != "high" {
		t.Fatalf("%+v", p)
	}
	r.do("PATCH", "/v1/tasks/"+a.Task.ID, map[string]string{"priority": "high"}, hdr).into(t, &p)
	if p.Changed {
		t.Fatal("changed twice")
	}
	if rp := r.do("PATCH", "/v1/tasks/"+a.Task.ID, map[string]any{}, hdr); rp.code != 422 {
		t.Fatalf("empty patch %d", rp.code)
	}
	if rp := r.do("PATCH", "/v1/tasks/"+a.Task.ID, map[string]any{"title": "new"}, hdr); rp.code != 400 {
		t.Fatalf("an unknown field is refused, not ignored: %d", rp.code)
	}
	if rp := r.do("POST", "/v1/tasks", map[string]string{"title": "x", "priority": "urgent"}, hdr); rp.code != 422 {
		t.Fatalf("%d", rp.code)
	}
	long := strings.Repeat("é", maxText+1)
	for _, body := range []map[string]string{{"title": long}, {"title": "x", "site": long}} {
		if rp := r.do("POST", "/v1/tasks", body, hdr); rp.code != 422 {
			t.Fatalf("long text %d", rp.code)
		}
	}
	if rp := r.do("POST", "/v1/tasks", map[string]string{"title": strings.Repeat("é", maxText)}, hdr); rp.code != 201 {
		t.Fatalf("exactly the limit: %d %s", rp.code, rp.body)
	}
	if rp := r.do("POST", "/v1/tasks/"+a.Task.ID+"/notes", map[string]string{"text": strings.Repeat("a", maxText+1)}, hdr); rp.code != 422 {
		t.Fatalf("long note %d", rp.code)
	}
	if rp := r.do("POST", "/v1/tasks", `{"title":"x"} {"title":"y"}`, hdr); rp.code != 400 {
		t.Fatalf("two values %d", rp.code)
	}
}

func TestAnAttachmentGoesInAndComesBackByteForByte(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("fix pump", "", task.PriorityLow)
	data := append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{9}, 2000)...)
	var a Attachment
	rp := r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", data, map[string]string{ActionHeader: "1", "Content-Type": "image/jpeg", "X-Offgrid-Name": url.PathEscape("pump 1.jpg")})
	rp.into(t, &a)
	if rp.code != 201 || a.Name != "pump 1.jpg" || a.Size != int64(len(data)) || a.Type != "image/jpeg" || !a.Present {
		t.Fatalf("%d %+v", rp.code, a)
	}
	var l attachmentList
	r.do("GET", "/v1/tasks/"+tk.ID+"/attachments", nil, nil).into(t, &l)
	if len(l.Attachments) != 1 || l.Attachments[0].Hash != a.Hash {
		t.Fatalf("%+v", l)
	}
	got := r.do("GET", "/v1/tasks/"+tk.ID+"/attachments/"+a.Hash, nil, nil)
	if got.code != 200 || !bytes.Equal(got.body, data) || got.h.Get("Content-Type") != "image/jpeg" || !strings.Contains(got.h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("%d %v", got.code, got.h)
	}
	long := strings.Repeat("é", 150) + ".jpg"
	rp = r.do("POST", "/v1/tasks/"+tk.ID+"/attachments", append(data, 1), map[string]string{ActionHeader: "1", "Content-Type": "image/jpeg", "X-Offgrid-Name": url.PathEscape(long)})
	rp.into(t, &a)
	if len([]rune(a.Name)) != 100 {
		t.Fatalf("name has %d characters", len([]rune(a.Name)))
	}
}

func TestSuggestionsAreListedAndAcceptingARefusesAStaleOne(t *testing.T) {
	r := newRig(t, nil)
	tk := r.mk("fix pump", "", task.PriorityLow)
	hi := task.PriorityHigh
	sg, _, err := r.st.Suggest(context.Background(), task.Draft{Task: tk.ID, Priority: &hi, By: "model", Key: "k", Note: "leaking"})
	if err != nil {
		t.Fatal(err)
	}
	var l suggestionList
	r.do("GET", "/v1/suggestions", nil, nil).into(t, &l)
	if len(l.Suggestions) != 1 || l.Suggestions[0].ID != sg.ID || l.Suggestions[0].Priority == nil || *l.Suggestions[0].Priority != "high" || l.Suggestions[0].Status != nil || l.Suggestions[0].State != "pending" || l.Suggestions[0].Note != "leaking" {
		t.Fatalf("%+v", l)
	}
	lo := task.PriorityLow
	med := task.PriorityMedium
	r.st.Update(context.Background(), tk.ID, task.Changes{Priority: &med})
	_ = lo
	rp := r.do("POST", "/v1/suggestions/"+sg.ID+"/accept", nil, map[string]string{ActionHeader: "1"})
	if rp.code != 409 || rp.errCode(t) != "stale" {
		t.Fatalf("%d %s", rp.code, rp.body)
	}
	if g, _ := r.st.Get(context.Background(), tk.ID); g.Priority != task.PriorityMedium {
		t.Fatalf("the edit was undone: %v", g.Priority)
	}
	rp = r.do("POST", "/v1/suggestions/"+sg.ID+"/dismiss", nil, map[string]string{ActionHeader: "1"})
	if rp.code != 200 {
		t.Fatalf("%d", rp.code)
	}
	var none, all suggestionList
	r.do("GET", "/v1/suggestions", nil, nil).into(t, &none)
	r.do("GET", "/v1/suggestions?all=true", nil, nil).into(t, &all)
	if len(none.Suggestions) != 0 || len(all.Suggestions) != 1 || all.Suggestions[0].State != "dismissed" {
		t.Fatalf("%+v %+v", none, all)
	}
}

func TestInfoNamesTheNodeAndTheVersion(t *testing.T) {
	r := newRig(t, nil)
	r.mk("a", "", task.PriorityLow)
	var in infoReply
	r.do("GET", "/v1/info", nil, nil).into(t, &in)
	if in.APIVersion != "v1" || in.Node != "site" || in.Tasks != 1 || !in.Time.Equal(t0) {
		t.Fatalf("%+v", in)
	}
}

func TestTheSpecIsServedAsIs(t *testing.T) {
	r := newRig(t, nil)
	rp := r.do("GET", "/v1/openapi.json", nil, nil)
	if !bytes.Equal(rp.body, Spec) || !strings.HasPrefix(rp.h.Get("Content-Type"), "application/json") {
		t.Fatal("served spec differs from the embedded file")
	}
}

// ---------------------------------------------------------------- CSV

func TestExportHasTheDocumentedColumnsAndEscapesFormulas(t *testing.T) {
	r := newRig(t, nil)
	r.mk("=HYPERLINK(\"http://evil\")", "+1 site", task.PriorityLow)
	r.mk("-5 pumps", "@home", task.PriorityHigh)
	r.mk("'=quoted", "", task.PriorityLow)
	r.mk("plain, with comma", "", task.PriorityLow)
	rp := r.do("GET", "/v1/export/tasks.csv", nil, nil)
	if !strings.HasPrefix(rp.h.Get("Content-Type"), "text/csv") || !strings.Contains(rp.h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("%v", rp.h)
	}
	recs, err := csv.NewReader(bytes.NewReader(rp.body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(recs[0], ",") != strings.Join(CSVColumns, ",") {
		t.Fatalf("%v", recs[0])
	}
	titles := map[string]bool{}
	for _, rec := range recs[1:] {
		titles[rec[1]] = true
		for _, cell := range rec {
			if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
				t.Errorf("a cell starts with a formula character: %q", cell)
			}
		}
	}
	for _, want := range []string{`'=HYPERLINK("http://evil")`, "'-5 pumps", "''=quoted", "plain, with comma"} {
		if !titles[want] {
			t.Errorf("missing %q in %v", want, titles)
		}
	}
}

func TestEscapingIsReversibleForAnyText(t *testing.T) {
	rng := rand.New(rand.NewSource(52))
	alphabet := []rune("'=+-@\t\r ab,\"\n[]é")
	for i := 0; i < 20000; i++ {
		n := rng.Intn(7)
		var b []rune
		for j := 0; j < n; j++ {
			b = append(b, alphabet[rng.Intn(len(alphabet))])
		}
		s := string(b)
		e := escapeCell(s)
		if e != "" && strings.ContainsRune("=+-@\t\r", rune(e[0])) {
			t.Fatalf("%q escapes to %q, which still starts a formula", s, e)
		}
		if got := unescapeCell(e); got != s {
			t.Fatalf("%q -> %q -> %q", s, e, got)
		}
	}
}

func snapshot(t *testing.T, r *rig) []string {
	t.Helper()
	ts, err := r.st.List(context.Background(), task.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ts {
		var ns []string
		for _, n := range x.Notes {
			ns = append(ns, n.Text)
		}
		out = append(out, fmt.Sprintf("%q|%q|%s|%s|%q", x.Title, x.Site, x.Status, x.Priority, ns))
	}
	sort.Strings(out)
	return out
}

func TestACSVRoundTripKeepsEveryTask(t *testing.T) {
	a := newRig(t, nil)
	rng := rand.New(rand.NewSource(7))
	words := []string{"fix pump", "=SUM(A1)", "-3 bags", "'=odd", "water, plants", "line\nbreak", `say "hi"`, "ünï", "@mention", "+44 call", "x", "[1]"}
	sts := []task.Status{task.StatusOpen, task.StatusInProgress, task.StatusDone}
	prs := []task.Priority{task.PriorityLow, task.PriorityMedium, task.PriorityHigh}
	made := 0
	for i := 0; i < 60; i++ {
		title, site := words[rng.Intn(len(words))], ""
		if rng.Intn(2) == 0 {
			site = words[rng.Intn(len(words))]
		}
		tk, created, err := a.st.Create(context.Background(), task.NewTask{Title: title, Site: site, Priority: prs[rng.Intn(3)]})
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			// finish the earlier one so a repeat of the same title is a new task, as it can be in real data
			d := task.StatusDone
			a.st.Update(context.Background(), tk.ID, task.Changes{Status: &d})
			continue
		}
		made++
		for n := rng.Intn(3); n > 0; n-- {
			a.st.AddNote(context.Background(), tk.ID, words[rng.Intn(len(words))]+fmt.Sprint(n))
		}
		if s := sts[rng.Intn(3)]; s != task.StatusOpen {
			a.st.Update(context.Background(), tk.ID, task.Changes{Status: &s})
		}
	}
	before := snapshot(t, a)
	if len(before) < 40 {
		t.Fatalf("only %d tasks to round-trip", len(before))
	}
	exp := a.do("GET", "/v1/export/tasks.csv", nil, nil)

	b := newRig(t, nil)
	rep, err := ImportCSV(context.Background(), b.st, bytes.NewReader(exp.body), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 0 || len(rep.Skipped) != 0 || rep.Created != len(before) || rep.Rows != len(before) {
		t.Fatalf("%+v (want %d tasks)", rep, len(before))
	}
	after := snapshot(t, b)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("the round trip changed the data\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	// and importing the same file again adds nothing that is unfinished twice
	rep2, _ := ImportCSV(context.Background(), b.st, bytes.NewReader(exp.body), false)
	if rep2.Created < 0 || len(snapshot(t, b)) < len(before) {
		t.Fatal("second import lost data")
	}
	for _, s := range rep2.Skipped {
		if !strings.Contains(s.Message, "already exists") {
			t.Fatalf("%+v", s)
		}
	}
}

func TestAnOpenAndAFinishedTaskWithTheSameTitleBothSurviveAnImport(t *testing.T) {
	r := newRig(t, nil)
	csvText := "title,status\nfix pump,open\nfix pump,done\nfix pump,done\n"
	rep, err := ImportCSV(context.Background(), r.st, strings.NewReader(csvText), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 3 || len(rep.Skipped) != 0 {
		t.Fatalf("%+v", rep)
	}
	ts, _ := r.st.List(context.Background(), task.Filter{})
	var done, open int
	for _, x := range ts {
		if x.Status == task.StatusDone {
			done++
		} else {
			open++
		}
	}
	if done != 2 || open != 1 {
		t.Fatalf("done %d open %d", done, open)
	}
}

func TestImportReportsBadRowsAndKeepsTheGoodOnes(t *testing.T) {
	r := newRig(t, nil)
	r.mk("existing", "", task.PriorityLow)
	in := "\xef\xbb\xbfTitle , Priority,STATUS,notes,extra\r\n" +
		"good one,high,in_progress,\"[\"\"a\"\",\"\"b\"\"]\",ignored\r\n" +
		",low,open,,\r\n" + // no title
		"bad priority,urgent,,,\r\n" +
		"bad status,low,finished,,\r\n" +
		"bad notes,low,open,[1,2],\r\n" +
		"empty note,low,open,\"[\"\"\"\"]\",\r\n" +
		"existing,low,open,,\r\n" +
		"\r\n" + // blank line is not a row
		",,,,\r\n" + // blank record
		"hand made note,,,just text,\r\n" +
		"short row\r\n"
	rep, err := ImportCSV(context.Background(), r.st, strings.NewReader(in), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 3 || len(rep.Failed) != 5 || len(rep.Skipped) != 1 || rep.Skipped[0].Row != 8 {
		t.Fatalf("%+v", rep)
	}
	rows := map[int]bool{}
	for _, f := range rep.Failed {
		rows[f.Row] = true
	}
	for _, n := range []int{3, 4, 5, 6, 7} {
		if !rows[n] {
			t.Errorf("row %d was not reported: %+v", n, rep.Failed)
		}
	}
	snap := strings.Join(snapshot(t, r), "\n")
	for _, want := range []string{`"good one"|""|in_progress|high|["a" "b"]`, `"hand made note"|""|open|medium|["just text"]`, `"short row"|""|open|medium|[]`} {
		if !strings.Contains(snap, want) {
			t.Errorf("missing %s in\n%s", want, snap)
		}
	}
}

func TestADryRunChangesNothingAndSaysWhatWouldHappen(t *testing.T) {
	r := newRig(t, nil)
	rep, err := ImportCSV(context.Background(), r.st, strings.NewReader("title\na\nb\n=bad priority?\n"), true)
	if err != nil || !rep.DryRun || rep.Created != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	if n := len(snapshot(t, r)); n != 0 {
		t.Fatalf("a dry run wrote %d tasks", n)
	}
}

func TestImportRefusesAFileThatIsNotATaskList(t *testing.T) {
	r := newRig(t, nil)
	for name, in := range map[string]string{"empty": "", "no title column": "name,site\nx,y\n", "only a header, no tasks": "title\n"} {
		rep, err := ImportCSV(context.Background(), r.st, strings.NewReader(in), false)
		if name == "only a header, no tasks" {
			if err != nil || rep.Created != 0 || rep.Rows != 0 {
				t.Errorf("%s: %+v %v", name, rep, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ImportCSV(context.Background(), r.st, strings.NewReader("title\n"+strings.Repeat("a\n", MaxImportRows+1)), false); err == nil {
		t.Error("too many rows")
	}
	rep, err := ImportCSV(context.Background(), r.st, strings.NewReader("title\n"+strings.Repeat("a\n", 1)+strings.Repeat("b\n", 0)), false)
	if err != nil || rep.Created != 1 {
		t.Errorf("%+v %v", rep, err)
	}
	var big strings.Builder
	big.WriteString("title\n")
	for i := 0; i < MaxImportRows; i++ {
		fmt.Fprintf(&big, "t%d\n", i)
	}
	if _, err := ImportCSV(context.Background(), newRig(t, nil).st, strings.NewReader(big.String()), true); err != nil {
		t.Errorf("exactly the row limit: %v", err)
	}
}

func TestImportStopsWhenItsContextEnds(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportCSV(ctx, r.st, strings.NewReader("title\na\nb\n"), false); err == nil {
		t.Fatal("kept going")
	}
}

func TestTasksMadeInTheSameInstantPageInAStableOrderAndTimesAreUTC(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("Addis", 3*3600) // a node whose local zone is not UTC must still speak UTC
	defer func() { time.Local = old }()
	same := time.Date(2026, 11, 28, 12, 0, 0, 0, time.Local)
	// a store whose clock never moves: every task has the same creation time
	r2 := newFrozenRig(t, same)
	var want []string
	for i := 0; i < 10; i++ {
		want = append(want, r2.mk(fmt.Sprintf("same instant %d", i), "", task.PriorityLow).ID)
	}
	sort.Strings(want)
	var got []string
	cursor := ""
	for n := 0; n < 20; n++ {
		path := "/v1/tasks?limit=3"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rp := r2.do("GET", path, nil, nil)
		var l taskList
		rp.into(t, &l)
		for _, x := range l.Tasks {
			got = append(got, x.ID)
			if !strings.HasSuffix(x.CreatedAt.String(), "UTC") && x.CreatedAt.Location() != time.UTC {
				t.Fatalf("created_at is not UTC: %v", x.CreatedAt)
			}
		}
		if !strings.Contains(string(rp.body), `Z"`) || strings.Contains(string(rp.body), "+03:00") {
			t.Fatalf("times must end in Z: %s", rp.body)
		}
		if l.NextCursor == nil {
			break
		}
		cursor = *l.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
