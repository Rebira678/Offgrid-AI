package syncer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// Day 40: mutual TLS 1.3 between enrolled devices.
//
// Each device presents a self-signed certificate made from its device key
// (identity.Certificate). Nobody checks a chain or a host name. What each side
// checks is the KEY: it must be the one the membership log names for an
// enrolled, unrevoked device (a server also by an explicit pin, which is how a
// device that has no membership yet can reach its first peer). Signatures prove
// who wrote an op; TLS hides it in transit and decides who may download.

// HeaderRequest carries the request ID: 16 random bytes in hex.
const HeaderRequest = "X-Offgrid-Request"

// Codes added by Day 40.
const (
	CodeReplay = "replay" // the same request ID was already seen
	CodeBusy   = "busy"   // the replay cache is full: too many requests in one window
)

var (
	// ErrNotEnrolled: the device is not in the membership log.
	ErrNotEnrolled = errors.New("the device is not enrolled")
	// ErrDeviceRevoked: the device was enrolled and has been revoked.
	ErrDeviceRevoked = errors.New("the device has been revoked")
	// ErrPeerNotTrusted: the peer's key is not the one that was pinned.
	ErrPeerNotTrusted = errors.New("the peer is not trusted")
)

// Authorizer says whether a device with this key may talk to us. Nil means yes.
// The same type is used to pin the other side (Trust).
type Authorizer func(device, publicKey string) error

// Trust is what a client requires of the server it dials.
type Trust = Authorizer

// AuthorizerTTL is how long an answer is remembered. It is also how long a
// revoked device can keep an already open connection.
const AuthorizerTTL = 5 * time.Second

// MembershipSource is what MembershipAuthorizer reads: a store.
type MembershipSource interface {
	Members(ctx context.Context) (sqlstore.MembershipReport, error)
}

// MembershipAuthorizer allows exactly the devices that are enrolled and not
// revoked in src's membership log, with the key the log names. Answers are cached for AuthorizerTTL.
func MembershipAuthorizer(src MembershipSource, now func() time.Time) Authorizer {
	type entry struct {
		err error
		at  time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(device, publicKey string) error {
		t := now()
		key := device + "|" + publicKey
		mu.Lock()
		if e, ok := cache[key]; ok && t.Sub(e.at) < AuthorizerTTL && t.Sub(e.at) >= 0 {
			mu.Unlock()
			return e.err
		}
		mu.Unlock()
		err := ErrNotEnrolled
		rep, rerr := src.Members(context.Background())
		if rerr != nil {
			return rerr // not cached: a database error is not an answer
		}
		for _, m := range rep.Members {
			if m.Device == device && m.PublicKey == publicKey {
				if m.Revoked {
					err = ErrDeviceRevoked
				} else {
					err = nil
				}
			}
		}
		mu.Lock()
		if len(cache) >= 1024 {
			cache = map[string]entry{} // bounded: a stranger trying many keys cannot grow it
		}
		cache[key] = entry{err, t}
		mu.Unlock()
		return err
	}
}

// PinDevice trusts only the device with this ID.
func PinDevice(device string) Trust {
	return func(d, _ string) error {
		if d != device {
			return fmt.Errorf("%w: it presented %s, which is not the device %s", ErrPeerNotTrusted, d, device)
		}
		return nil
	}
}

// PinFingerprint trusts only the key with this SHA-256 fingerprint (see identity.Fingerprint).
func PinFingerprint(fp string) Trust {
	return func(_, key string) error {
		got, err := identity.Fingerprint(key)
		if err != nil || got != fp {
			return fmt.Errorf("%w: its key's fingerprint is not the one the invite named", ErrPeerNotTrusted)
		}
		return nil
	}
}

// PinMembers trusts the devices src's membership log names.
func PinMembers(src MembershipSource, now func() time.Time) Trust {
	return Trust(MembershipAuthorizer(src, now))
}

// AllOf trusts a peer only if every one of ts does.
func AllOf(ts ...Trust) Trust {
	return func(d, k string) error {
		for _, t := range ts {
			if err := t(d, k); err != nil {
				return err
			}
		}
		return nil
	}
}

// certSource mints device certificates, renewing before they run out.
type certSource struct {
	id  *identity.Identity
	now func() time.Time
	mu  sync.Mutex
	cur *tls.Certificate
	end time.Time
}

const certValidity = 24 * time.Hour

func (c *certSource) get() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now()
	if c.cur == nil || t.After(c.end.Add(-time.Hour)) || t.Before(c.end.Add(-certValidity-time.Hour)) {
		cert, err := c.id.Certificate(t, certValidity)
		if err != nil {
			return nil, err
		}
		c.cur, c.end = &cert, t.Add(certValidity)
	}
	return c.cur, nil
}

func verifyPeer(now func() time.Time, check Authorizer) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("the peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return fmt.Errorf("the peer's certificate is not valid: %w", err)
		}
		dev, key, err := identity.DeviceOfCertificate(leaf, now())
		if err != nil {
			return err
		}
		return check(dev, key)
	}
}

// ServerTLSConfig is the sync listener's TLS: 1.3 only, a client certificate is
// required, and a client that is not authorized by authz cannot finish the handshake.
func ServerTLSConfig(id *identity.Identity, authz Authorizer, now func() time.Time) (*tls.Config, error) {
	if now == nil {
		now = time.Now
	}
	src := &certSource{id: id, now: now}
	if _, err := src.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		NextProtos:            []string{"h2", "http/1.1"},
		GetCertificate:        func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return src.get() },
		VerifyPeerCertificate: verifyPeer(now, authz),
	}, nil
}

// EnrollServerTLSConfig is the enrollment listener's TLS: 1.3 only and no client
// certificate (a device being enrolled has none that anyone knows yet). The
// client pins the server instead, using the invite.
func EnrollServerTLSConfig(id *identity.Identity, now func() time.Time) (*tls.Config, error) {
	if now == nil {
		now = time.Now
	}
	src := &certSource{id: id, now: now}
	if _, err := src.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return src.get() },
	}, nil
}

// ClientTLSConfig is a client's TLS: 1.3 only, trust decides whether the server's
// key is acceptable (no CA, no host name: the key is the identity), and id, if
// not nil, is presented as the client certificate.
func ClientTLSConfig(id *identity.Identity, trust Trust, now func() time.Time) *tls.Config {
	if now == nil {
		now = time.Now
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
		// The standard verification is for CAs and host names. This design has neither; the key is checked below.
		InsecureSkipVerify:    true, //nolint:gosec // replaced by VerifyPeerCertificate, which pins the key
		VerifyPeerCertificate: verifyPeer(now, trust),
	}
	if id != nil {
		src := &certSource{id: id, now: now}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return src.get() }
	}
	return cfg
}

// NewTLSClient is an HTTP client that reuses connections (HTTP/2 when the server speaks it).
func NewTLSClient(cfg *tls.Config) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     cfg,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// Timeouts bound how long a client may hold a connection or a request open.
type Timeouts struct {
	ReadHeader, Read, Write, Idle time.Duration
}

// DefaultTimeouts suit a LAN or a phone on Wi-Fi with batches of a few thousand ops.
var DefaultTimeouts = Timeouts{ReadHeader: 10 * time.Second, Read: 30 * time.Second, Write: 60 * time.Second, Idle: 120 * time.Second}

// NewHTTPServer is an http.Server with every timeout set, so a client that
// connects and says nothing, or one byte at a time, is dropped.
func NewHTTPServer(h http.Handler, cfg *tls.Config, to Timeouts) *http.Server {
	return &http.Server{
		Handler:           h,
		TLSConfig:         cfg,
		ReadHeaderTimeout: to.ReadHeader,
		ReadTimeout:       to.Read,
		WriteTimeout:      to.Write,
		IdleTimeout:       to.Idle,
		MaxHeaderBytes:    16 << 10,
	}
}

// ---- request IDs and replay -----------------------------------------------------------------------------------------

var (
	errReplay     = errors.New("replayed request")
	errReplayFull = errors.New("replay cache full")
)

// replayCache remembers request IDs for ttl. It is bounded: when it is full it
// refuses NEW IDs rather than forgetting old ones, because forgetting is how a
// replay gets through. Entries older than ttl are dropped first.
type replayCache struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	seen map[string]time.Time
}

func newReplayCache(max int, ttl time.Duration) *replayCache {
	return &replayCache{max: max, ttl: ttl, seen: map[string]time.Time{}}
}

func (c *replayCache) see(id string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) >= c.max/2 { // prune lazily, when the cache is getting full
		for k, at := range c.seen {
			if now.Sub(at) > c.ttl {
				delete(c.seen, k)
			}
		}
	}
	if at, ok := c.seen[id]; ok && now.Sub(at) <= c.ttl {
		return errReplay
	}
	if _, ok := c.seen[id]; !ok && len(c.seen) >= c.max {
		return errReplayFull
	}
	c.seen[id] = now
	return nil
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("syncer: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func validRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// signSecure stamps a request for the mutual-TLS path: no HMAC, but a fresh
// request ID and the time, which the server checks.
func signSecure(r *http.Request, now time.Time) {
	r.Header.Set(HeaderRequest, newRequestID())
	r.Header.Set(HeaderTime, strconv.FormatInt(now.Unix(), 10))
}
