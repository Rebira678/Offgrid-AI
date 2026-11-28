package phone

import (
	"encoding/json"
	"time"

	"offgrid/internal/task"
)

// PairingCode represents an issued one-time code to pair a phone.
type PairingCode struct {
	Code       string     `json:"code"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
}

// Session represents an enrolled, authenticated phone session.
type Session struct {
	Token      string    `json:"token"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Revoked    bool      `json:"revoked"`
}

// QueuedOp is one command performed offline on the phone, to be replayed upon reconnect.
type QueuedOp struct {
	OpID         string          `json:"op_id"`
	Action       string          `json:"action"` // create_task, update_status, add_note, delete_task, resolve_suggestion
	ClientTime   time.Time       `json:"client_time"`
	TaskID       string          `json:"task_id,omitempty"`
	SuggestionID string          `json:"suggestion_id,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// CreateTaskPayload holds details for offline task creation.
type CreateTaskPayload struct {
	Title    string `json:"title"`
	Priority string `json:"priority,omitempty"`
	Note     string `json:"note,omitempty"`
}

// UpdateStatusPayload holds details for offline status update.
type UpdateStatusPayload struct {
	Status string `json:"status"`
}

// AddNotePayload holds details for offline note addition.
type AddNotePayload struct {
	Text string `json:"text"`
}

// ResolveSuggestionPayload holds details for suggestion acceptance or dismissal.
type ResolveSuggestionPayload struct {
	Accept bool `json:"accept"`
}

// ReplayBatch is a collection of queued operations sent by a reconnecting phone.
type ReplayBatch struct {
	ClientID   string     `json:"client_id"`
	Operations []QueuedOp `json:"operations"`
}

// OpResult reports the outcome of replaying one queued operation.
type OpResult struct {
	OpID      string    `json:"op_id"`
	Action    string    `json:"action"`
	Status    string    `json:"status"` // applied, conflict_task_deleted, conflict_stale, error
	TaskID    string    `json:"task_id,omitempty"`
	Message   string    `json:"message,omitempty"`
	StampedAt time.Time `json:"stamped_at"`
}

// ReplayResponse summarizes the results of a replayed batch.
type ReplayResponse struct {
	Applied   int        `json:"applied"`
	Conflicts int        `json:"conflicts"`
	Errors    int        `json:"errors"`
	Results   []OpResult `json:"results"`
}

// PhoneAttachment summarizes an attached photo or document for the phone client.
type PhoneAttachment struct {
	Hash    string    `json:"hash"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	Present bool      `json:"present"`
	URL     string    `json:"url"`
	At      time.Time `json:"at"`
}

// PhoneTask is the client-facing representation of a task.
type PhoneTask struct {
	ID          string            `json:"id"`
	ShortID     string            `json:"short_id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	Priority    string            `json:"priority"`
	Notes       []task.Note       `json:"notes"`
	Attachments []PhoneAttachment `json:"attachments"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// PhoneSuggestion is the client-facing representation of an AI suggestion.
type PhoneSuggestion struct {
	ID               string    `json:"id"`
	TaskID           string    `json:"task_id"`
	TaskTitle        string    `json:"task_title"`
	CurrentStatus    string    `json:"current_status"`
	CurrentPriority  string    `json:"current_priority"`
	ProposedStatus   string    `json:"proposed_status"`
	ProposedPriority string    `json:"proposed_priority"`
	Rationale        string    `json:"rationale"`
	Stale            bool      `json:"stale"`
	CreatedAt        time.Time `json:"created_at"`
}
