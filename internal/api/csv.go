package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"offgrid/internal/task"
)

// CSVColumns is the export's header, in order. An import needs only "title"; it ignores columns it does not know.
var CSVColumns = []string{"id", "title", "site", "status", "priority", "created_at", "updated_at", "done_at", "notes"}

// Spreadsheets run a cell that starts with = + - @ (or a tab or carriage return) as a formula, and a task title
// comes from anywhere: a phone, another site, an AI. So export puts a ' in front of such a cell, and import takes
// exactly one off again. dangerous also looks past leading quotes so that a title that really starts with ' is
// not mistaken for an escape: "'=x" goes out as "”=x" and comes back as "'=x".
func dangerous(s string) bool {
	i := 0
	for i < len(s) && s[i] == '\'' {
		i++
	}
	return i < len(s) && strings.IndexByte("=+-@\t\r", s[i]) >= 0
}

func escapeCell(s string) string {
	if dangerous(s) {
		return "'" + s
	}
	return s
}

func unescapeCell(s string) string {
	if strings.HasPrefix(s, "'") && dangerous(s[1:]) {
		return s[1:]
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// WriteCSV writes tasks as CSV. Notes go in one cell as a JSON array, because a note may hold any character and
// the cell must decode to exactly the notes that were there.
func WriteCSV(w io.Writer, ts []task.Task) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVColumns); err != nil {
		return err
	}
	for _, t := range ts {
		notes := make([]string, 0, len(t.Notes))
		for _, n := range t.Notes {
			notes = append(notes, n.Text)
		}
		nj, _ := json.Marshal(notes)
		row := []string{t.ID, escapeCell(t.Title), escapeCell(t.Site), string(t.Status), string(t.Priority),
			fmtTime(t.CreatedAt), fmtTime(t.UpdatedAt), fmtTime(t.DoneAt), escapeCell(string(nj))}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func (h *handler) exportCSV(w http.ResponseWriter, r *http.Request) {
	ts, err := h.c.Store.List(r.Context(), task.Filter{})
	if err != nil {
		fromStore(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, ts); err != nil {
		fromStore(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="offgrid-tasks.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// ImportProblem is one row that was skipped or failed.
type ImportProblem struct {
	Row     int    `json:"row"` // 1 is the header; the first task is row 2
	Message string `json:"message"`
}

// ImportReport says what an import did (or, with dry_run, would do).
type ImportReport struct {
	DryRun  bool            `json:"dry_run"`
	Rows    int             `json:"rows"`
	Created int             `json:"created"`
	Skipped []ImportProblem `json:"skipped"`
	Failed  []ImportProblem `json:"failed"`
}

type importRow struct {
	n        int
	title    string
	site     string
	status   task.Status
	priority task.Priority
	notes    []string
}

// ParseCSV reads an import without touching a store, so a dry run and the real run agree on what is valid.
func ParseCSV(r io.Reader) (rows []importRow, failed []ImportProblem, err error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = false
	head, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("the file is empty; it needs a header row with at least a title column")
		}
		return nil, nil, fmt.Errorf("cannot read the header: %v", err)
	}
	if len(head) > 0 {
		head[0] = strings.TrimPrefix(head[0], "\xef\xbb\xbf") // Excel writes a byte order mark
	}
	col := map[string]int{}
	for i, name := range head {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, dup := col[name]; !dup {
			col[name] = i
		}
	}
	if _, ok := col["title"]; !ok {
		return nil, nil, errors.New(`the header has no "title" column`)
	}
	cell := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	for n := 2; ; n++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if n-1 > MaxImportRows {
			return nil, nil, fmt.Errorf("more than %d rows; split the file", MaxImportRows)
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, nil, err // the whole body is too big: a size problem, not a bad row
		}
		if err != nil {
			failed = append(failed, ImportProblem{Row: n, Message: "cannot read this row: " + err.Error()})
			continue
		}
		blank := true
		for _, f := range rec {
			if strings.TrimSpace(f) != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		row := importRow{n: n, title: strings.TrimSpace(unescapeCell(cell(rec, "title"))), site: strings.TrimSpace(unescapeCell(cell(rec, "site"))),
			status: task.StatusOpen, priority: task.PriorityMedium}
		bad := func(msg string) { failed = append(failed, ImportProblem{Row: n, Message: msg}) }
		if row.title == "" {
			bad("the title is empty")
			continue
		}
		if utf8.RuneCountInString(row.title) > maxText || utf8.RuneCountInString(row.site) > maxText {
			bad(fmt.Sprintf("text is limited to %d characters", maxText))
			continue
		}
		if s := strings.TrimSpace(cell(rec, "status")); s != "" {
			st, err := task.ParseStatus(s)
			if err != nil {
				bad(err.Error())
				continue
			}
			row.status = st
		}
		if p := strings.TrimSpace(cell(rec, "priority")); p != "" {
			pr, err := task.ParsePriority(p)
			if err != nil {
				bad(err.Error())
				continue
			}
			row.priority = pr
		}
		if raw := unescapeCell(cell(rec, "notes")); strings.TrimSpace(raw) != "" {
			var ns []string
			if strings.HasPrefix(strings.TrimSpace(raw), "[") {
				if err := json.Unmarshal([]byte(raw), &ns); err != nil {
					bad("notes is not a JSON list of text: " + err.Error())
					continue
				}
			} else {
				ns = []string{raw} // a hand-made file: the whole cell is one note
			}
			okNotes := true
			for _, t := range ns {
				if strings.TrimSpace(t) == "" || utf8.RuneCountInString(t) > maxText {
					okNotes = false
				}
			}
			if !okNotes {
				bad(fmt.Sprintf("a note is 1 to %d characters", maxText))
				continue
			}
			row.notes = ns
		}
		rows = append(rows, row)
	}
	return rows, failed, nil
}

// ImportCSV adds the tasks in a CSV file. It never changes or removes a task that exists: a row whose
// unfinished task (same title and site) is already there is skipped. Finished rows go first, each marked done
// before the next row is made, so a finished "fix pump" cannot be swallowed by an open "fix pump" from the same
// file, and an open one is not swallowed by a finished one.
func ImportCSV(ctx context.Context, st Store, r io.Reader, dryRun bool) (ImportReport, error) {
	rows, failed, err := ParseCSV(r)
	rep := ImportReport{DryRun: dryRun, Skipped: []ImportProblem{}, Failed: []ImportProblem{}}
	if err != nil {
		return rep, err
	}
	rep.Failed = append(rep.Failed, failed...)
	rep.Rows = len(rows) + len(failed)
	var ordered []importRow
	for _, row := range rows {
		if row.status == task.StatusDone {
			ordered = append(ordered, row)
		}
	}
	for _, row := range rows {
		if row.status != task.StatusDone {
			ordered = append(ordered, row)
		}
	}
	for _, row := range ordered {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if dryRun {
			rep.Created++
			continue
		}
		t, created, err := st.Create(ctx, task.NewTask{Title: row.title, Site: row.site, Priority: row.priority})
		if err != nil {
			rep.Failed = append(rep.Failed, ImportProblem{Row: row.n, Message: err.Error()})
			continue
		}
		if !created {
			rep.Skipped = append(rep.Skipped, ImportProblem{Row: row.n, Message: "an unfinished task with this title and site already exists"})
			continue
		}
		if row.status != task.StatusOpen {
			s := row.status
			if _, _, err := st.Update(ctx, t.ID, task.Changes{Status: &s}); err != nil {
				rep.Failed = append(rep.Failed, ImportProblem{Row: row.n, Message: "made the task but could not set its status: " + err.Error()})
				continue
			}
		}
		for _, n := range row.notes {
			if _, err := st.AddNote(ctx, t.ID, n); err != nil {
				rep.Failed = append(rep.Failed, ImportProblem{Row: row.n, Message: "made the task but could not add a note: " + err.Error()})
				break
			}
		}
		rep.Created++
	}
	return rep, nil
}

func (h *handler) importCSV(w http.ResponseWriter, r *http.Request) {
	dry := false
	switch v := r.URL.Query().Get("dry_run"); v {
	case "", "false":
	case "true":
		dry = true
	default:
		Fail(w, http.StatusBadRequest, "bad_request", "dry_run is true or false")
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxImportBody)
	rep, err := ImportCSV(r.Context(), h.c.Store, body, dry)
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			Fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the file is over %d bytes", MaxImportBody))
		case r.Context().Err() != nil:
		default:
			Fail(w, http.StatusUnprocessableEntity, "invalid", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
