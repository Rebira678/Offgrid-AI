package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// Store defines the store capabilities required by the phone client.
type Store interface {
	NodeID() string
	List(ctx context.Context, f task.Filter) ([]task.Task, error)
	Get(ctx context.Context, id string) (task.Task, error)
	Create(ctx context.Context, n task.NewTask) (task.Task, bool, error)
	Update(ctx context.Context, id string, c task.Changes) (task.Task, bool, error)
	AddNote(ctx context.Context, id, text string) (task.Task, error)
	DeleteTask(ctx context.Context, id string) error
	Suggestions(ctx context.Context, all bool) ([]task.Suggestion, error)
	Resolve(ctx context.Context, id string, accept bool) (task.Task, error)
	AddAttachment(ctx context.Context, taskID string, r io.Reader, declared int64, mediaType, name string) (sqlstore.Attachment, error)
	OpenAttachment(ctx context.Context, taskID, hash string) (*os.File, sqlstore.Attachment, error)
	Attachments(ctx context.Context, taskID string) ([]sqlstore.Attachment, error)
}

// API handles all phone client HTTP endpoints.
type API struct {
	store         Store
	sessions      *SessionStore
	now           func() time.Time
	secureCookies bool
	readOnly      bool
}

// NewAPI returns an API handler.
func NewAPI(st Store, sessions *SessionStore, now func() time.Time, secureCookies, readOnly bool) *API {
	if now == nil {
		now = time.Now
	}
	return &API{
		store:         st,
		sessions:      sessions,
		now:           now,
		secureCookies: secureCookies,
		readOnly:      readOnly,
	}
}

// PairRequest is the body sent when redeeming a pairing code.
type PairRequest struct {
	Code  string `json:"code"`
	Label string `json:"label,omitempty"`
}

// HandlePair exchanges a one-time pairing code for a session token and cookie.
func (a *API) HandlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req PairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json payload: "+err.Error())
		return
	}

	sess, err := a.sessions.RedeemPairingCode(r.Context(), req.Code, req.Label)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeInvalid):
			writeError(w, http.StatusUnauthorized, "invalid pairing code")
		case errors.Is(err, ErrCodeExpired):
			writeError(w, http.StatusUnauthorized, "pairing code has expired")
		case errors.Is(err, ErrCodeUsed):
			writeError(w, http.StatusUnauthorized, "pairing code has already been used")
		default:
			writeError(w, http.StatusInternalServerError, "could not redeem pairing code: "+err.Error())
		}
		return
	}

	// Issue HttpOnly secure cookie
	SetSessionCookie(w, sess.Token, a.secureCookies)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"token":       sess.Token,
		"label":       sess.Label,
		"enrolled_at": sess.CreatedAt,
	})
}

// HandleSession returns the status of the current session.
func (a *API) HandleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unpaired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"label":         sess.Label,
		"created_at":    sess.CreatedAt,
		"node_id":       a.store.NodeID(),
	})
}

// HandleLogout clears the session cookie.
func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// HandleListTasks returns all tasks enriched with attachment metadata.
func (a *API) HandleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := a.store.List(r.Context(), task.Filter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read tasks: "+err.Error())
		return
	}

	out := make([]PhoneTask, 0, len(tasks))
	for _, t := range tasks {
		pt := PhoneTask{
			ID:        t.ID,
			ShortID:   task.ShortID(t.ID),
			Title:     t.Title,
			Status:    string(t.Status),
			Priority:  string(t.Priority),
			Notes:     t.Notes,
			CreatedAt: t.CreatedAt,
			UpdatedAt: t.UpdatedAt,
		}

		// Retrieve attachments if available
		atts, err := a.store.Attachments(r.Context(), t.ID)
		if err == nil && len(atts) > 0 {
			pt.Attachments = make([]PhoneAttachment, 0, len(atts))
			for _, att := range atts {
				pt.Attachments = append(pt.Attachments, PhoneAttachment{
					Hash:    att.Hash,
					Name:    att.Name,
					Type:    att.Type,
					Size:    att.Size,
					Present: att.Present,
					URL:     fmt.Sprintf("/api/phone/tasks/%s/attachments/%s", t.ID, att.Hash),
					At:      att.At,
				})
			}
		}

		out = append(out, pt)
	}

	writeJSON(w, http.StatusOK, out)
}

// CreateTaskRequest is the body for creating a task online.
type CreateTaskRequest struct {
	Title    string `json:"title"`
	Priority string `json:"priority,omitempty"`
	Note     string `json:"note,omitempty"`
}

// HandleCreateTask creates a new task.
func (a *API) HandleCreateTask(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	var req CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title cannot be empty")
		return
	}

	pri := task.Priority(req.Priority)
	if pri == "" {
		pri = task.PriorityMedium
	}

	created, _, err := a.store.Create(r.Context(), task.NewTask{
		Title:    req.Title,
		Priority: pri,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create task: "+err.Error())
		return
	}
	if req.Note != "" {
		created, _ = a.store.AddNote(r.Context(), created.ID, req.Note)
	}

	writeJSON(w, http.StatusCreated, PhoneTask{
		ID:        created.ID,
		ShortID:   task.ShortID(created.ID),
		Title:     created.Title,
		Status:    string(created.Status),
		Priority:  string(created.Priority),
		Notes:     created.Notes,
		CreatedAt: created.CreatedAt,
		UpdatedAt: created.UpdatedAt,
	})
}

// HandleUpdateStatus updates a task status.
func (a *API) HandleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	id := r.PathValue("id")
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	st := task.Status(req.Status)
	updated, _, err := a.store.Update(r.Context(), id, task.Changes{Status: &st})
	if errors.Is(err, task.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, PhoneTask{
		ID:        updated.ID,
		ShortID:   task.ShortID(updated.ID),
		Title:     updated.Title,
		Status:    string(updated.Status),
		Priority:  string(updated.Priority),
		Notes:     updated.Notes,
		CreatedAt: updated.CreatedAt,
		UpdatedAt: updated.UpdatedAt,
	})
}

// HandleAddNote appends a note to a task.
func (a *API) HandleAddNote(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	id := r.PathValue("id")
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeError(w, http.StatusBadRequest, "note cannot be empty")
		return
	}

	updated, err := a.store.AddNote(r.Context(), id, req.Note)
	if errors.Is(err, task.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot add note: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, PhoneTask{
		ID:        updated.ID,
		ShortID:   task.ShortID(updated.ID),
		Title:     updated.Title,
		Status:    string(updated.Status),
		Priority:  string(updated.Priority),
		Notes:     updated.Notes,
		CreatedAt: updated.CreatedAt,
		UpdatedAt: updated.UpdatedAt,
	})
}

// HandleDeleteTask removes a task.
func (a *API) HandleDeleteTask(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	id := r.PathValue("id")
	err := a.store.DeleteTask(r.Context(), id)
	if errors.Is(err, task.ErrNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete task: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

// HandleGetAttachment streams an attachment file with security headers.
func (a *API) HandleGetAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hash := r.PathValue("hash")

	f, att, err := a.store.OpenAttachment(r.Context(), id, hash)
	switch {
	case errors.Is(err, task.ErrNotFound) || (err != nil && att.Hash == ""):
		writeError(w, http.StatusNotFound, "attachment not found")
		return
	case errors.Is(err, blobstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "file not present on this node yet")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "cannot read attachment: "+err.Error())
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Content-Type", att.Type)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	h.Set("Cache-Control", "private, max-age=86400")

	disp := "attachment"
	if isInlineMedia(att.Type) {
		disp = "inline"
	}
	name := att.Name
	if name == "" {
		name = att.Hash[:12]
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))

	http.ServeContent(w, r, name, att.At, f)
}

func isInlineMedia(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/") || mediaType == "application/pdf"
}

// HandleUploadAttachment receives and attaches a photo or file to a task.
func (a *API) HandleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	id := r.PathValue("id")
	contentType := r.Header.Get("Content-Type")

	var (
		fileReader io.Reader
		fileName   = r.Header.Get("X-File-Name")
		mediaType  string
		size       int64
	)

	if strings.HasPrefix(contentType, "multipart/form-data") {
		err := r.ParseMultipartForm(MaxPhotoBodyBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot parse multipart form: "+err.Error())
			return
		}
		file, header, err := r.FormFile("photo")
		if err != nil {
			// Fallback to "file" field
			file, header, err = r.FormFile("file")
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing photo or file field in form")
			return
		}
		defer file.Close()
		fileReader = file
		fileName = header.Filename
		mediaType = header.Header.Get("Content-Type")
		size = header.Size
	} else {
		fileReader = r.Body
		mediaType = contentType
		size = r.ContentLength
	}

	if mediaType == "" || mediaType == "application/octet-stream" {
		if ext := mime.TypeByExtension(strings.ToLower(strings.TrimPrefix(fileName, "."))); ext != "" {
			mediaType = ext
		} else if strings.HasSuffix(strings.ToLower(fileName), ".png") {
			mediaType = "image/png"
		} else if strings.HasSuffix(strings.ToLower(fileName), ".jpg") || strings.HasSuffix(strings.ToLower(fileName), ".jpeg") {
			mediaType = "image/jpeg"
		} else if strings.HasSuffix(strings.ToLower(fileName), ".webp") {
			mediaType = "image/webp"
		} else if strings.HasSuffix(strings.ToLower(fileName), ".gif") {
			mediaType = "image/gif"
		} else if strings.HasSuffix(strings.ToLower(fileName), ".pdf") {
			mediaType = "application/pdf"
		} else {
			mediaType = "image/jpeg"
		}
	}
	if fileName == "" {
		fileName = fmt.Sprintf("photo_%d.jpg", a.now().Unix())
	}

	att, err := a.store.AddAttachment(r.Context(), id, fileReader, size, mediaType, fileName)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot attach file: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, PhoneAttachment{
		Hash:    att.Hash,
		Name:    att.Name,
		Type:    att.Type,
		Size:    att.Size,
		Present: att.Present,
		URL:     fmt.Sprintf("/api/phone/tasks/%s/attachments/%s", id, att.Hash),
		At:      att.At,
	})
}

// HandleListSuggestions lists active AI suggestions for tasks.
func (a *API) HandleListSuggestions(w http.ResponseWriter, r *http.Request) {
	sugs, err := a.store.Suggestions(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read suggestions: "+err.Error())
		return
	}

	out := make([]PhoneSuggestion, 0, len(sugs))
	for _, s := range sugs {
		t, _ := a.store.Get(r.Context(), s.Task)
		propStatus := ""
		if s.Status != nil {
			propStatus = string(*s.Status)
		}
		propPriority := ""
		if s.Priority != nil {
			propPriority = string(*s.Priority)
		}

		out = append(out, PhoneSuggestion{
			ID:               s.ID,
			TaskID:           s.Task,
			TaskTitle:        t.Title,
			CurrentStatus:    string(s.BaseStatus),
			CurrentPriority:  string(s.BasePriority),
			ProposedStatus:   propStatus,
			ProposedPriority: propPriority,
			Rationale:        s.Note,
			Stale:            s.Stale,
			CreatedAt:        s.At,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// HandleResolveSuggestion accepts or dismisses an AI suggestion.
func (a *API) HandleResolveSuggestion(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	id := r.PathValue("id")
	action := r.PathValue("action")

	var accept bool
	switch action {
	case "accept":
		accept = true
	case "dismiss":
		accept = false
	default:
		writeError(w, http.StatusBadRequest, "unknown action: use accept or dismiss")
		return
	}

	updated, err := a.store.Resolve(r.Context(), id, accept)
	switch {
	case err == nil:
		msg := "dismissed suggestion"
		if accept {
			msg = fmt.Sprintf("accepted suggestion: %s is now %s, %s", task.ShortID(updated.ID), updated.Status, updated.Priority)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
	case errors.Is(err, task.ErrStale):
		writeError(w, http.StatusConflict, "refused: task was edited after AI proposed this change. Dismiss it instead.")
	case errors.Is(err, task.ErrSuggestionResolved):
		writeError(w, http.StatusConflict, "suggestion has already been resolved")
	case errors.Is(err, task.ErrSuggestionNotFound), errors.Is(err, task.ErrNotFound):
		writeError(w, http.StatusNotFound, "suggestion or task not found")
	default:
		writeError(w, http.StatusInternalServerError, "could not resolve suggestion: "+err.Error())
	}
}

// HandleReplayQueue replays a batch of queued operations created offline.
func (a *API) HandleReplayQueue(w http.ResponseWriter, r *http.Request) {
	if a.readOnly {
		writeError(w, http.StatusForbidden, "store is read-only")
		return
	}

	var batch ReplayBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json replay batch: "+err.Error())
		return
	}

	sess, _ := SessionFromContext(r.Context())
	res := ReplayQueuedOps(r.Context(), a.store, sess, batch, a.now)

	writeJSON(w, http.StatusOK, res)
}
