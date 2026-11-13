package sqlstore

// Invites: how a new device joins.
//
// An admin makes a one-time invite. The invite is a token the admin shows once
// (typed, or as a QR code made from Invite.Link) and, in the link form, the
// admin's device ID, so the new device PINS the admin it is joining and cannot
// be tricked into joining someone else's membership. The new device presents
// the token with its public key and a proof that it holds the key; the admin's
// node checks the token, enrolls the device with a signed member.add op, and
// hands back a signed receipt.
//
// A stolen invite is a stolen enrollment until it is used or expires, so
// invites are short-lived (at most 24 hours) and single-use, and the secret is
// stored only as a salted hash. Invites live in the database of the node that
// made them and do NOT sync: redemption happens at that node.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"offgrid/internal/identity"
)

var (
	// ErrInviteInvalid: the token is unknown, malformed or wrong. It is also what a wrong guess at an
	// expired or used invite gets, so an answer never tells a guesser which invites exist.
	ErrInviteInvalid = errors.New("sqlstore: the invite is not valid")
	// ErrInviteExpired: right token, too late.
	ErrInviteExpired = errors.New("sqlstore: the invite has expired")
	// ErrInviteUsed: right token, already redeemed.
	ErrInviteUsed = errors.New("sqlstore: the invite was already used")
)

const (
	// MaxInviteTTL is the longest an invite may live.
	MaxInviteTTL = 24 * time.Hour

	inviteIDBytes     = 5  // public part, 40 bits: finds the row
	inviteSecretBytes = 10 // secret part, 80 bits: never stored
	tokenChars        = (inviteIDBytes + inviteSecretBytes) * 8 / 5
	inviteIDChars     = inviteIDBytes * 8 / 5
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite is what an admin gets back. Token is shown once and cannot be recovered.
type Invite struct {
	ID      string
	Token   string // grouped for reading aloud: abcd-efgh-...
	Admin   string // the issuing admin's device ID
	Role    Role
	Label   string
	Expires time.Time
}

// NormalizeToken makes a typed token comparable: upper case, no dashes or spaces.
func NormalizeToken(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "", "\n", "").Replace(s))
}

func parseToken(s string) (raw, id string, ok bool) {
	raw = NormalizeToken(s)
	if len(raw) != tokenChars {
		return "", "", false
	}
	if _, err := b32.DecodeString(raw); err != nil {
		return "", "", false
	}
	return raw, raw[:inviteIDChars], true
}

func group(raw string) string {
	var parts []string
	for i := 0; i < len(raw); i += 4 {
		end := i + 4
		if end > len(raw) {
			end = len(raw)
		}
		parts = append(parts, raw[i:end])
	}
	return strings.Join(parts, "-")
}

func tokenHash(salt []byte, raw string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(raw))
	return h.Sum(nil)
}

// CreateInvite makes an invite. This device must be an admin.
func (s *Store) CreateInvite(ctx context.Context, role Role, label string, ttl time.Duration) (Invite, error) {
	var inv Invite
	if ttl <= 0 || ttl > MaxInviteTTL {
		return inv, fmt.Errorf("sqlstore: an invite must live between a moment and %s: a stolen invite is a stolen enrollment until it expires", MaxInviteTTL)
	}
	if role != RoleAdmin && role != RoleMember {
		return inv, fmt.Errorf("sqlstore: role %q is not admin or member", role)
	}
	if err := checkLabel(label); err != nil {
		return inv, fmt.Errorf("sqlstore: %w", err)
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := s.requireAdmin(ctx, tx); err != nil {
			return err
		}
		secret := make([]byte, inviteIDBytes+inviteSecretBytes)
		salt := make([]byte, 16)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		raw := b32.EncodeToString(secret)
		now := s.now().UTC()
		inv = Invite{ID: raw[:inviteIDChars], Token: group(raw), Admin: s.signer.ID(), Role: role, Label: label, Expires: now.Add(ttl)}
		_, err := tx.ExecContext(ctx, `INSERT INTO invites (id, salt, hash, role, label, created_by, created_at, expires) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			inv.ID, salt, tokenHash(salt, raw), string(role), label, inv.Admin, now.UnixNano(), inv.Expires.UnixNano())
		return err
	})
	return inv, err
}

// enrollMessage is what a new device signs to prove it holds the key it presents.
func enrollMessage(inviteID, publicKey, label string) []byte {
	return []byte("offgrid-enroll-v1\x00" + inviteID + "\x00" + publicKey + "\x00" + label)
}

// receiptMessage is what the admin signs to tell the new device it was enrolled, as what.
func receiptMessage(inviteID, device string, role Role, label string) []byte {
	return []byte("offgrid-enroll-receipt-v1\x00" + inviteID + "\x00" + device + "\x00" + string(role) + "\x00" + label)
}

// EnrollProof signs the enrollment request with the device's own key.
func EnrollProof(token string, sg Signer, label string) ([]byte, error) {
	_, id, ok := parseToken(token)
	if !ok {
		return nil, ErrInviteInvalid
	}
	return sg.Sign(enrollMessage(id, sg.PublicKey(), label))
}

// ReceiptMessage lets the new device check the admin's receipt.
func ReceiptMessage(inviteID, device string, role Role, label string) []byte {
	return receiptMessage(inviteID, device, role, label)
}

// InviteID is the public part of a token.
func InviteID(token string) (string, error) {
	_, id, ok := parseToken(token)
	if !ok {
		return "", ErrInviteInvalid
	}
	return id, nil
}

// Enrollment is the result of a redemption.
type Enrollment struct {
	InviteID    string
	Device      string
	Role        Role
	Label       string
	AdminDevice string
	AdminKey    string
	AdminNode   string // the replica the member.add op is on: sync with it to receive the membership
	Receipt     []byte // the admin's signature over receiptMessage
}

// RedeemInvite enrolls a device, in ONE transaction: the token is checked, the
// invite is marked used, the member.add op is recorded and signed. If any step
// fails nothing happens, and the invite stays usable. The token is compared in
// constant time, and a wrong token is "invalid" whatever state the invite is in.
func (s *Store) RedeemInvite(ctx context.Context, token, publicKey, label string, proof []byte) (Enrollment, error) {
	var en Enrollment
	raw, id, ok := parseToken(token)
	if !ok {
		return en, ErrInviteInvalid
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		var salt, want []byte
		var role string
		var expires int64
		var redeemed sql.NullInt64
		var inviteLabel string
		err := tx.QueryRowContext(ctx, `SELECT salt, hash, role, label, expires, redeemed_at FROM invites WHERE id = ?`, id).Scan(&salt, &want, &role, &inviteLabel, &expires, &redeemed)
		if errors.Is(err, sql.ErrNoRows) {
			// Spend the same work as a real check, so unknown IDs do not answer faster.
			subtle.ConstantTimeCompare(tokenHash(make([]byte, 16), raw), make([]byte, 32))
			return ErrInviteInvalid
		} else if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(tokenHash(salt, raw), want) != 1 {
			return ErrInviteInvalid
		}
		if redeemed.Valid {
			return ErrInviteUsed
		}
		if !s.now().Before(time.Unix(0, expires)) {
			return ErrInviteExpired
		}
		if err := identity.Verify(publicKey, enrollMessage(id, publicKey, label), proof); err != nil {
			return fmt.Errorf("%w: the proof that the device holds the key it presented does not verify (%v)", ErrBadSignature, err)
		}
		rep, err := s.requireAdmin(ctx, tx)
		if err != nil {
			return err
		}
		if label == "" {
			label = inviteLabel
		}
		if err := s.addChecked(ctx, tx, rep, publicKey, Role(role), label); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE invites SET redeemed_at = ?, redeemed_by = ? WHERE id = ? AND redeemed_at IS NULL`, s.now().UnixNano(), mustDevice(publicKey), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrInviteUsed
		}
		dev := mustDevice(publicKey)
		receipt, err := s.signer.Sign(receiptMessage(id, dev, Role(role), label))
		if err != nil {
			return err
		}
		en = Enrollment{InviteID: id, Device: dev, Role: Role(role), Label: label, AdminDevice: s.signer.ID(), AdminKey: s.signer.PublicKey(), AdminNode: s.node, Receipt: receipt}
		return nil
	})
	if err != nil {
		return Enrollment{}, err
	}
	return en, nil
}

func mustDevice(pub string) string {
	d, _ := identity.DeviceID(pub)
	return d
}

// InviteLink is the parsed form of an invite string.
type InviteLink struct {
	Admin       string // device ID to pin
	Token       string
	Fingerprint string // the node's TLS certificate fingerprint (hex SHA-256); empty until Day 40
}

// Link writes the invite as one string, for a QR code or a message:
//
//	og-invite/1/<admin device ID>/<token>/<certificate fingerprint, or empty>
func (i Invite) Link(fingerprint string) string {
	return "og-invite/1/" + i.Admin + "/" + i.Token + "/" + fingerprint
}

// ParseInviteLink reads what Link wrote.
func ParseInviteLink(s string) (InviteLink, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 5 || parts[0] != "og-invite" || parts[1] != "1" {
		return InviteLink{}, errors.New("not an Offgrid invite (expected og-invite/1/DEVICE/TOKEN/FINGERPRINT)")
	}
	l := InviteLink{Admin: parts[2], Token: parts[3], Fingerprint: parts[4]}
	if !strings.HasPrefix(l.Admin, "dev_") || len(l.Admin) != 36 {
		return InviteLink{}, errors.New("the invite names no valid admin device")
	}
	if _, _, ok := parseToken(l.Token); !ok {
		return InviteLink{}, errors.New("the invite's token is malformed")
	}
	if l.Fingerprint != "" {
		if len(l.Fingerprint) != 64 || strings.Trim(l.Fingerprint, "0123456789abcdef") != "" {
			return InviteLink{}, errors.New("the invite's fingerprint is not a SHA-256 in hex")
		}
	}
	return l, nil
}
