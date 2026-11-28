package phone

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// A session is a signed statement "this node vouches for device D until time T". It holds no state on the
// node, so a restart does not log a phone out, and it can be checked without the database. What keeps it
// honest is that every request ALSO checks that D is still an enrolled, un-revoked member: revoking a lost
// phone ends its session at once, whatever the token still says.
//
// It is a bearer token (see ADR 018): anyone who copies the cookie out of the phone can use it until it
// expires or the device is revoked. HttpOnly, Secure and SameSite=Strict make copying it hard, not impossible.

var (
	errToken   = errors.New("not a valid session")
	errExpired = errors.New("the session has expired")
)

const (
	sessionContext   = "offgrid-phone-session-v1\x00"
	challengeContext = "offgrid-phone-challenge-v1\x00"
	maxTokenLen      = 512
	// MinSecret is the shortest HMAC key accepted.
	MinSecret = 32
	// ChallengeTTL is how long a login challenge may be answered.
	ChallengeTTL = time.Minute
	clockSlack   = 2 * time.Minute // a token "from the future" by more than this is forged or the node's clock moved
)

type claims struct {
	Device string `json:"d"`
	Issued int64  `json:"i"` // unix seconds
	Expiry int64  `json:"e"`
}

type signer struct{ key []byte }

func (s signer) mac(ctx, payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(ctx))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func (s signer) seal(ctx string, payload []byte) string {
	p := base64.RawURLEncoding.EncodeToString(payload)
	return "v1." + p + "." + base64.RawURLEncoding.EncodeToString(s.mac(ctx, p))
}

func (s signer) open(ctx, token string) ([]byte, error) {
	if len(token) > maxTokenLen {
		return nil, errToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errToken
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, s.mac(ctx, parts[1])) {
		return nil, errToken
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, errToken
	}
	return payload, nil
}

func (s signer) issue(device string, now time.Time, ttl time.Duration) string {
	b, _ := json.Marshal(claims{Device: device, Issued: now.Unix(), Expiry: now.Add(ttl).Unix()})
	return s.seal(sessionContext, b)
}

func (s signer) verify(token string, now time.Time) (claims, error) {
	var c claims
	payload, err := s.open(sessionContext, token)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(payload, &c) != nil || c.Device == "" {
		return claims{}, errToken
	}
	if now.Unix() >= c.Expiry {
		return claims{}, errExpired
	}
	if time.Unix(c.Issued, 0).After(now.Add(clockSlack)) {
		return claims{}, errToken
	}
	return c, nil
}

// challenge makes a nonce a phone must sign to log in again. It carries its own time, so the node keeps
// nothing, and it is single use (see nonces).
func (s signer) challenge(now time.Time) string {
	b := make([]byte, 8+16)
	binary.BigEndian.PutUint64(b, uint64(now.UnixNano()))
	_, _ = rand.Read(b[8:])
	return s.seal(challengeContext, b)
}

func (s signer) checkChallenge(nonce string, now time.Time) error {
	b, err := s.open(challengeContext, nonce)
	if err != nil || len(b) != 24 {
		return errToken
	}
	at := time.Unix(0, int64(binary.BigEndian.Uint64(b)))
	if age := now.Sub(at); age > ChallengeTTL || age < -clockSlack {
		return errExpired
	}
	return nil
}

// nonces remembers answered challenges for as long as they could still be answered, so a login that was
// seen on the wire cannot be played again.
type nonces struct {
	mu   sync.Mutex
	used map[string]time.Time
}

func (n *nonces) claim(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.used == nil {
		n.used = map[string]time.Time{}
	}
	for k, t := range n.used {
		if now.Sub(t) > ChallengeTTL+clockSlack {
			delete(n.used, k)
		}
	}
	if _, dup := n.used[nonce]; dup {
		return false
	}
	n.used[nonce] = now
	return true
}

// limiter slows guessing. Failures are counted per remote address and over the whole node; past the limit
// every attempt is refused (the right answer too) until the window has passed.
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	perKey int
	total  int
	fails  map[string][]time.Time
	all    []time.Time
}

func newLimiter() *limiter {
	return &limiter{window: time.Minute, perKey: 5, total: 20, fails: map[string][]time.Time{}}
}

func trim(ts []time.Time, now time.Time, w time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= w {
		i++
	}
	return ts[i:]
}

func (l *limiter) allowed(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = trim(l.fails[key], now, l.window)
	l.all = trim(l.all, now, l.window)
	return len(l.fails[key]) < l.perKey && len(l.all) < l.total
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 4096 { // a flood of addresses must not grow the table without bound
		l.fails = map[string][]time.Time{}
	}
	l.fails[key] = append(trim(l.fails[key], now, l.window), now)
	l.all = append(trim(l.all, now, l.window), now)
}
