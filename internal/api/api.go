// Package api is Offgrid's HTTP contract for programs: scripts on this machine, the phone app, and a future
// native app. REST and JSON, described by openapi.json (embedded here and served at /v1/openapi.json).
//
// The promise (docs/api/versioning.md): nothing under /v1 changes in a way that breaks a client that follows
// the file; anything not yet proven by use lives under /v0 and may change. Every route has a contract test
// that validates its real responses against the file.
//
// This package does not authenticate. It is mounted by something that does: `offgrid api serve` (this
// machine only, with the dashboard's header rule) and the phone server (a paired phone's session).
package api

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"offgrid/internal/blobstore"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
	"offgrid/internal/web"
)

//go:embed openapi.json
var Spec []byte

// Version is the contract's major version.
const Version = "v1"

// Store is what the API needs. *sqlstore.Store satisfies it.
type Store interface {
	web.Store
	Get(ctx context.Context, id string) (task.Task, error)
	Create(ctx context.Context, n task.NewTask) (task.Task, bool, error)
	Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error)
	AddNote(ctx context.Context, id, text string) (task.Task, error)
	Attachments(ctx context.Context, taskID string) ([]sqlstore.Attachment, error)
	AddAttachment(ctx context.Context, taskID string, r io.Reader, declared int64, mediaType, name string) (sqlstore.Attachment, error)
}

// Config is everything the API reads.
type Config struct {
	Store Store
	// AllowImport mounts POST /v0/import/tasks.csv. Importing writes up to MaxImportRows tasks at once, so only
	// the machine's own owner gets it, not a paired phone.
	AllowImport bool
	MaxUpload   int64 // largest attachment (default 16 MiB; the blob store has its own cap)
	Now         func() time.Time
}

// Limits, all part of the contract.
const (
	DefaultLimit  = 100
	MaxLimit      = 500
	MaxJSONBody   = 64 << 10
	MaxImportBody = 8 << 20
	MaxImportRows = 10000
	maxText       = 1000
	defaultUpload = 16 << 20
)

// Route is one entry of the contract as the server implements it.
type Route struct {
	Method, Path string // Path uses {name} wildcards, exactly as in openapi.json
	Stability    string // "stable" under /v1, "v0" under /v0
}

type handler struct {
	c Config
}

type route struct {
	Route
	fn http.HandlerFunc
}

// Handler returns the API and the list of routes it serves.
func Handler(c Config) (http.Handler, []Route) {
	if c.MaxUpload <= 0 {
		c.MaxUpload = defaultUpload
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	h := &handler{c: c}
	rs := []route{
		{Route{"GET", "/v1/info", "stable"}, h.info},
		{Route{"GET", "/v1/openapi.json", "stable"}, h.spec},
		{Route{"GET", "/v1/tasks", "stable"}, h.listTasks},
		{Route{"POST", "/v1/tasks", "stable"}, h.createTask},
		{Route{"GET", "/v1/tasks/{id}", "stable"}, h.getTask},
		{Route{"PATCH", "/v1/tasks/{id}", "stable"}, h.patchTask},
		{Route{"POST", "/v1/tasks/{id}/notes", "stable"}, h.addNote},
		{Route{"GET", "/v1/tasks/{id}/attachments", "stable"}, h.listAttachments},
		{Route{"POST", "/v1/tasks/{id}/attachments", "stable"}, h.addAttachment},
		{Route{"GET", "/v1/tasks/{id}/attachments/{hash}", "stable"}, h.getAttachment},
		{Route{"GET", "/v1/suggestions", "stable"}, h.listSuggestions},
		{Route{"POST", "/v1/suggestions/{id}/accept", "stable"}, h.resolve(true)},
		{Route{"POST", "/v1/suggestions/{id}/dismiss", "stable"}, h.resolve(false)},
		{Route{"GET", "/v1/export/tasks.csv", "stable"}, h.exportCSV},
	}
	if c.AllowImport {
		rs = append(rs, route{Route{"POST", "/v0/import/tasks.csv", "v0"}, h.importCSV})
	}
	mux := http.NewServeMux()
	out := make([]Route, 0, len(rs))
	for _, r := range rs {
		mux.HandleFunc(r.Method+" "+r.Path, r.fn)
		out = append(out, r.Route)
	}
	notFound := func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range []string{"GET", "POST", "PATCH"} {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pat := mux.Handler(probe); pat != "" && pat != "/v1/" && pat != "/v0/" {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			Fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "this path does not take "+r.Method)
			return
		}
		Fail(w, http.StatusNotFound, "not_found", "there is no such route")
	}
	mux.HandleFunc("/v1/", notFound)
	mux.HandleFunc("/v0/", notFound)
	return mux, out
}

// ---- replies

// Error is the one error type every route returns.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Fail writes the contract's error.
func Fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, Error{Code: code, Message: msg})
}

func fromStore(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	switch {
	case r.Context().Err() != nil:
		// the client has gone; nothing to say
	case errors.Is(err, task.ErrNotFound), errors.Is(err, task.ErrSuggestionNotFound):
		Fail(w, http.StatusNotFound, "not_found", "no such task or suggestion")
	case errors.Is(err, task.ErrStale):
		Fail(w, http.StatusConflict, "stale", err.Error())
	case errors.Is(err, task.ErrSuggestionResolved):
		Fail(w, http.StatusConflict, "already_resolved", err.Error())
	case errors.Is(err, task.ErrInvalid):
		Fail(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, blobstore.ErrTooLarge), errors.As(err, &tooBig):
		Fail(w, http.StatusRequestEntityTooLarge, "too_large", "that is over the size limit")
	case errors.Is(err, blobstore.ErrEmpty):
		Fail(w, http.StatusBadRequest, "bad_request", "an empty body is not an attachment")
	case errors.Is(err, sqlstore.ErrBadAttachment):
		Fail(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
	default:
		Fail(w, http.StatusInternalServerError, "internal", "the node could not complete the request")
	}
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			Fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the body is over %d bytes", limit))
		} else {
			Fail(w, http.StatusBadRequest, "bad_request", "cannot read the JSON body: "+err.Error())
		}
		return false
	}
	if dec.More() {
		Fail(w, http.StatusBadRequest, "bad_request", "more than one JSON value in the body")
		return false
	}
	return true
}

// ---- JSON shapes (the contract's schemas)

// Task is a task as clients see it.
type Task struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Site      string     `json:"site"`
	Status    string     `json:"status"`
	Priority  string     `json:"priority"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DoneAt    *time.Time `json:"done_at"`
	Notes     []Note     `json:"notes"`
}

// Note is one note on a task.
type Note struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

func toTask(t task.Task) Task {
	out := Task{ID: t.ID, Title: t.Title, Site: t.Site, Status: string(t.Status), Priority: string(t.Priority),
		CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(), Notes: make([]Note, 0, len(t.Notes))}
	if !t.DoneAt.IsZero() {
		d := t.DoneAt.UTC()
		out.DoneAt = &d
	}
	for _, n := range t.Notes {
		out.Notes = append(out.Notes, Note{Text: n.Text, At: n.At.UTC()})
	}
	return out
}

// ---- info and spec

type infoReply struct {
	APIVersion string    `json:"api_version"`
	Node       string    `json:"node"`
	Tasks      int       `json:"tasks"`
	Time       time.Time `json:"time"`
}

func (h *handler) info(w http.ResponseWriter, r *http.Request) {
	in, err := h.c.Store.Info(r.Context())
	if err != nil {
		fromStore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, infoReply{APIVersion: Version, Node: h.c.Store.NodeID(), Tasks: in.Tasks, Time: h.c.Now().UTC()})
}

func (h *handler) spec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(Spec)
}

// ---- tasks

type taskList struct {
	Tasks      []Task  `json:"tasks"`
	NextCursor *string `json:"next_cursor"`
}

func encodeCursor(t task.Task) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.CreatedAt.UnixNano(), 10) + "." + t.ID))
}

func decodeCursor(s string) (int64, string, bool) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return 0, "", false
	}
	ns, id, ok := strings.Cut(string(b), ".")
	n, err := strconv.ParseInt(ns, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", false
	}
	return n, id, true
}

func (h *handler) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var f task.Filter
	if v := q.Get("status"); v != "" {
		s, err := task.ParseStatus(v)
		if err != nil {
			Fail(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		f.Status = s
	}
	if v := q.Get("priority"); v != "" {
		p, err := task.ParsePriority(v)
		if err != nil {
			Fail(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		f.Priority = p
	}
	f.Site = q.Get("site")
	limit := DefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxLimit {
			Fail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("limit is a whole number from 1 to %d", MaxLimit))
			return
		}
		limit = n
	}
	var afterNS int64
	var afterID string
	hasCursor := false
	if v := q.Get("cursor"); v != "" {
		var ok bool
		if afterNS, afterID, ok = decodeCursor(v); !ok {
			Fail(w, http.StatusBadRequest, "bad_request", "that cursor is not one this node gave")
			return
		}
		hasCursor = true
	}
	all, err := h.c.Store.List(r.Context(), f)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i].CreatedAt.UnixNano(), all[j].CreatedAt.UnixNano()
		if a != b {
			return a < b
		}
		return all[i].ID < all[j].ID
	})
	out := taskList{Tasks: []Task{}}
	for i, t := range all {
		if hasCursor {
			n := t.CreatedAt.UnixNano()
			if n < afterNS || n == afterNS && t.ID <= afterID {
				continue
			}
		}
		if len(out.Tasks) == limit {
			c := encodeCursor(all[i-1])
			out.NextCursor = &c
			break
		}
		out.Tasks = append(out.Tasks, toTask(t))
	}
	writeJSON(w, http.StatusOK, out)
}

type createRequest struct {
	Title    string `json:"title"`
	Site     string `json:"site"`
	Priority string `json:"priority"`
}

type createReply struct {
	Task    Task `json:"task"`
	Created bool `json:"created"`
}

func tooLong(fields ...string) bool {
	for _, f := range fields {
		if utf8.RuneCountInString(f) > maxText {
			return true
		}
	}
	return false
}

func (h *handler) createTask(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decode(w, r, MaxJSONBody, &req) {
		return
	}
	pr := task.PriorityMedium
	if req.Priority != "" {
		p, err := task.ParsePriority(req.Priority)
		if err != nil {
			Fail(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		pr = p
	}
	if tooLong(req.Title, req.Site) {
		Fail(w, http.StatusUnprocessableEntity, "invalid", fmt.Sprintf("text is limited to %d characters", maxText))
		return
	}
	t, created, err := h.c.Store.Create(r.Context(), task.NewTask{Title: req.Title, Site: req.Site, Priority: pr})
	if err != nil {
		fromStore(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, createReply{Task: toTask(t), Created: created})
}

func (h *handler) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := h.c.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		fromStore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTask(t))
}

type patchRequest struct {
	Status   *string `json:"status"`
	Priority *string `json:"priority"`
}

type patchReply struct {
	Task    Task `json:"task"`
	Changed bool `json:"changed"`
}

func (h *handler) patchTask(w http.ResponseWriter, r *http.Request) {
	var req patchRequest
	if !decode(w, r, MaxJSONBody, &req) {
		return
	}
	var ch task.Changes
	if req.Status != nil {
		s, err := task.ParseStatus(*req.Status)
		if err != nil {
			Fail(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		ch.Status = &s
	}
	if req.Priority != nil {
		p, err := task.ParsePriority(*req.Priority)
		if err != nil {
			Fail(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		ch.Priority = &p
	}
	if ch.Status == nil && ch.Priority == nil {
		Fail(w, http.StatusUnprocessableEntity, "invalid", "give a status, a priority, or both")
		return
	}
	t, changed, err := h.c.Store.Update(r.Context(), r.PathValue("id"), ch)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, patchReply{Task: toTask(t), Changed: changed})
}

type noteRequest struct {
	Text string `json:"text"`
}

func (h *handler) addNote(w http.ResponseWriter, r *http.Request) {
	var req noteRequest
	if !decode(w, r, MaxJSONBody, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || tooLong(text) {
		Fail(w, http.StatusUnprocessableEntity, "invalid", fmt.Sprintf("a note is 1 to %d characters", maxText))
		return
	}
	t, err := h.c.Store.AddNote(r.Context(), r.PathValue("id"), text)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toTask(t))
}

// ---- attachments

// Attachment is a file on a task. Present is false while the bytes have not reached this node.
type Attachment struct {
	Hash    string    `json:"hash"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	At      time.Time `json:"at"`
	Present bool      `json:"present"`
}

type attachmentList struct {
	Attachments []Attachment `json:"attachments"`
}

func toAttachment(a sqlstore.Attachment) Attachment {
	return Attachment{Hash: a.Hash, Name: a.Name, Type: a.Type, Size: a.Size, At: a.At.UTC(), Present: a.Present}
}

func (h *handler) listAttachments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.c.Store.Get(r.Context(), id); err != nil {
		fromStore(w, r, err)
		return
	}
	as, err := h.c.Store.Attachments(r.Context(), id)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	out := attachmentList{Attachments: make([]Attachment, 0, len(as))}
	for _, a := range as {
		out.Attachments = append(out.Attachments, toAttachment(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) addAttachment(w http.ResponseWriter, r *http.Request) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt == "" {
		Fail(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send the file with its Content-Type")
		return
	}
	name, err := url.PathUnescape(r.Header.Get("X-Offgrid-Name"))
	if err != nil || strings.TrimSpace(name) == "" {
		name = "file"
	}
	if utf8.RuneCountInString(name) > 100 {
		name = string([]rune(name)[:100])
	}
	if r.ContentLength > h.c.MaxUpload {
		Fail(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("an attachment may be at most %d bytes", h.c.MaxUpload))
		return
	}
	body := http.MaxBytesReader(w, r.Body, h.c.MaxUpload)
	a, err := h.c.Store.AddAttachment(r.Context(), r.PathValue("id"), body, r.ContentLength, mt, name)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAttachment(a))
}

func (h *handler) getAttachment(w http.ResponseWriter, r *http.Request) {
	web.ServeAttachment(w, r, h.c.Store, Fail)
}

// ---- suggestions

// Suggestion is something the AI proposed and a person has not yet decided.
type Suggestion struct {
	ID       string    `json:"id"`
	Task     string    `json:"task"`
	Status   *string   `json:"status"`
	Priority *string   `json:"priority"`
	Note     string    `json:"note"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	State    string    `json:"state"`
	Stale    bool      `json:"stale"`
}

type suggestionList struct {
	Suggestions []Suggestion `json:"suggestions"`
}

func (h *handler) listSuggestions(w http.ResponseWriter, r *http.Request) {
	all := false
	switch v := r.URL.Query().Get("all"); v {
	case "", "false":
	case "true":
		all = true
	default:
		Fail(w, http.StatusBadRequest, "bad_request", "all is true or false")
		return
	}
	ss, err := h.c.Store.Suggestions(r.Context(), all)
	if err != nil {
		fromStore(w, r, err)
		return
	}
	out := suggestionList{Suggestions: make([]Suggestion, 0, len(ss))}
	for _, s := range ss {
		x := Suggestion{ID: s.ID, Task: s.Task, Note: s.Note, By: s.By, At: s.At.UTC(), State: string(s.State), Stale: s.Stale}
		if s.Status != nil {
			v := string(*s.Status)
			x.Status = &v
		}
		if s.Priority != nil {
			v := string(*s.Priority)
			x.Priority = &v
		}
		out.Suggestions = append(out.Suggestions, x)
	}
	writeJSON(w, http.StatusOK, out)
}

type resolveReply struct {
	Task Task `json:"task"`
}

func (h *handler) resolve(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := h.c.Store.Resolve(r.Context(), r.PathValue("id"), accept)
		if err != nil {
			fromStore(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, resolveReply{Task: toTask(t)})
	}
}
