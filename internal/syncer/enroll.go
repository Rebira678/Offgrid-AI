package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// Enrollment is how a new device joins (Day 38). It presents a one-time invite
// and proves it holds its own key; the admin node adds it with a signed
// member.add op and answers with a signed receipt.
//
// This endpoint is the only one without the shared-key HMAC: a device being
// enrolled has no key yet. What protects it instead:
//   - the invite is 80 bits of secret, one-time, short-lived, and stored hashed;
//   - every kind of bad invite gets the same answer, so a guesser learns nothing;
//   - failures are rate limited, so guessing is not just hopeless but slow;
//   - the body is small.
//
// It does not give the new device the shared sync key. That still travels out
// of band until mTLS (Day 40) replaces it.

// Enroller is the part of a store that can redeem an invite.
type Enroller interface {
	RedeemInvite(ctx context.Context, token, publicKey, label string, proof []byte) (sqlstore.Enrollment, error)
}

var _ Enroller = (*sqlstore.Store)(nil)

const (
	// MaxEnrollFailures is how many failed attempts one server tolerates inside
	// EnrollFailureWindow before it refuses everyone, even with a valid invite.
	MaxEnrollFailures   = 10
	EnrollFailureWindow = time.Minute
	maxEnrollBody       = 8 << 10
)

// EnrollRequest is what the new device sends.
type EnrollRequest struct {
	Token     string `json:"token"`
	PublicKey string `json:"public_key"`
	Label     string `json:"label"`
	Proof     []byte `json:"proof"`
}

// EnrollResponse is the admin node's answer.
type EnrollResponse struct {
	InviteID    string `json:"invite_id"`
	Device      string `json:"device"`
	Role        string `json:"role"`
	Label       string `json:"label"`
	AdminDevice string `json:"admin_device"`
	AdminKey    string `json:"admin_key"`
	AdminNode   string `json:"admin_node"`
	Receipt     []byte `json:"receipt"`
}

// failureLimiter counts failures in a sliding window. It is per server, not per
// address: behind a proxy or NAT the address means little, and a global cap
// bounds the guessing rate whoever is guessing. The price is that a guesser can
// make a legitimate enrollment wait one minute; that is accepted and documented.
type failureLimiter struct {
	mu   sync.Mutex
	fail []time.Time
}

func (l *failureLimiter) prune(now time.Time) {
	i := 0
	for i < len(l.fail) && now.Sub(l.fail[i]) >= EnrollFailureWindow {
		i++
	}
	l.fail = l.fail[i:]
}

func (l *failureLimiter) blocked(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	return len(l.fail) >= MaxEnrollFailures
}

func (l *failureLimiter) record(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	l.fail = append(l.fail, now)
}

func (s *server) enroll(w http.ResponseWriter, r *http.Request) {
	write := func(rep reply) {
		b, _ := json.Marshal(rep.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		w.Write(b)
	}
	now := s.opt.Now()
	if s.lim.blocked(now) {
		write(fail(http.StatusTooManyRequests, CodeTooManyAttempts, "too many failed enrollment attempts; wait a minute"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEnrollBody+1))
	if err != nil || len(body) > maxEnrollBody {
		s.lim.record(now)
		write(fail(http.StatusRequestEntityTooLarge, CodeBadRequest, "the request is too large or unreadable"))
		return
	}
	var req EnrollRequest
	if bad := decode(body, &req); bad != nil {
		s.lim.record(now)
		write(*bad)
		return
	}
	en, err := s.opt.Enroller.RedeemInvite(r.Context(), req.Token, req.PublicKey, req.Label, req.Proof)
	if err != nil {
		switch {
		case errors.Is(err, sqlstore.ErrAlreadyMember):
			write(fail(http.StatusConflict, CodeAlreadyMember, "this device is already a member"))
		case errors.Is(err, sqlstore.ErrInviteInvalid), errors.Is(err, sqlstore.ErrInviteUsed),
			errors.Is(err, sqlstore.ErrInviteExpired), errors.Is(err, sqlstore.ErrBadSignature):
			// One answer for every case: which of them it was is exactly what a guesser wants to know.
			s.lim.record(now)
			s.logf("enroll refused: %v", err)
			write(fail(http.StatusForbidden, CodeInviteInvalid, "the invite is not valid"))
		case errors.Is(err, sqlstore.ErrNotAuthorized):
			s.lim.record(now)
			write(fail(http.StatusForbidden, CodeNotAuthorized, "this node cannot enroll devices"))
		default:
			s.logf("enroll failed: %v", err)
			write(fail(http.StatusInternalServerError, CodeInternal, "enrollment failed"))
		}
		return
	}
	s.logf("enrolled %s as %s (%q)", en.Device, en.Role, en.Label)
	write(reply{http.StatusOK, EnrollResponse{
		InviteID: en.InviteID, Device: en.Device, Role: string(en.Role), Label: en.Label,
		AdminDevice: en.AdminDevice, AdminKey: en.AdminKey, AdminNode: en.AdminNode, Receipt: en.Receipt,
	}})
}

// Enroll presents an invite to the admin node at peer. It sends no HMAC: the
// device has no shared key yet. Check the answer with CheckEnrollment before
// believing it.
func Enroll(ctx context.Context, hc *http.Client, peer string, req EnrollRequest) (EnrollResponse, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return EnrollResponse{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(peer, "/")+PathEnroll, strings.NewReader(string(b)))
	if err != nil {
		return EnrollResponse{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hr)
	if err != nil {
		return EnrollResponse{}, fmt.Errorf("cannot reach the peer: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrollBody*4))
	if err != nil {
		return EnrollResponse{}, fmt.Errorf("reading the peer's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e ErrorResponse
		_ = json.Unmarshal(rb, &e)
		return EnrollResponse{}, &RemoteError{resp.StatusCode, e.Code, e.Error}
	}
	var out EnrollResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return EnrollResponse{}, fmt.Errorf("the peer's response is not valid: %w", err)
	}
	return out, nil
}

// ErrWrongAdmin means the answer did not come from the admin the invite named,
// or does not describe this device: do not trust it.
var ErrWrongAdmin = errors.New("the enrollment answer is not from the admin named in the invite, or is not about this device")

// CheckEnrollment is what the new device does before believing the answer. The
// invite pinned the admin's device ID; the answer must carry a key that hashes
// to it and a receipt that key signed for exactly this device, role and label.
func CheckEnrollment(r EnrollResponse, link sqlstore.InviteLink, own sqlstore.Signer, label string) error {
	if r.AdminDevice != link.Admin || r.Device != own.ID() || r.Label != label {
		return ErrWrongAdmin
	}
	if r.Role != string(sqlstore.RoleMember) && r.Role != string(sqlstore.RoleAdmin) {
		return ErrWrongAdmin
	}
	if id, err := identity.DeviceID(r.AdminKey); err != nil || id != link.Admin {
		return ErrWrongAdmin
	}
	invID, err := sqlstore.InviteID(link.Token)
	if err != nil || invID != r.InviteID {
		return ErrWrongAdmin
	}
	if err := identity.Verify(r.AdminKey, sqlstore.ReceiptMessage(r.InviteID, r.Device, sqlstore.Role(r.Role), r.Label), r.Receipt); err != nil {
		return ErrWrongAdmin
	}
	return nil
}
