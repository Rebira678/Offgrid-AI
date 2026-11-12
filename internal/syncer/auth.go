package syncer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Headers carrying the signature and device identification.
const (
	HeaderNode     = "X-Offgrid-Node"
	HeaderTime     = "X-Offgrid-Time"
	HeaderSig      = "X-Offgrid-Sig"
	HeaderDeviceID = "X-Offgrid-Device-ID"
)

// MinKeyLen is the shortest shared key accepted. A short key can be guessed
// offline from one captured request.
const MinKeyLen = 16

// DefaultWindow is how far a request's timestamp may be from the server's
// clock. It limits how long a captured request stays replayable. It also means
// two replicas whose clocks differ by more than this cannot sync at all, and
// the error says by how much.
const DefaultWindow = 5 * time.Minute

// ErrKey means the shared key is missing or too short.
var ErrKey = errors.New("sync key is missing or shorter than 16 bytes")

// LoadKey reads the shared key from a file, or, when path is empty, from the
// environment variable OFFGRID_SYNC_KEY. Surrounding whitespace is trimmed.
// It refuses a file that anyone but its owner can read (mode 600 or 400).
// The key is never printed by this package.
func LoadKey(path string) ([]byte, error) {
	var raw string
	if path != "" {
		path = filepath.Clean(path)
		lst, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read the sync key file: %w", err)
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("sync key file %s is a symlink, which is not allowed for secrets", path)
		}
		if !lst.Mode().IsRegular() {
			return nil, fmt.Errorf("sync key file %s is not a regular file", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read the sync key file: %w", err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("cannot read the sync key file: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("sync key file %s is not a regular file", path)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("sync key file %s has mode %04o, but it holds a shared secret and must be readable by its owner only; fix it with: chmod 600 %q", path, fi.Mode().Perm(), path)
		}
		b, err := io.ReadAll(io.LimitReader(f, 4096))
		if err != nil {
			return nil, fmt.Errorf("cannot read the sync key file: %w", err)
		}
		raw = string(b)
	} else {
		raw = os.Getenv("OFFGRID_SYNC_KEY")
	}
	key := []byte(strings.TrimSpace(raw))
	if len(key) < MinKeyLen {
		return nil, fmt.Errorf("%w (give -key-file FILE or set OFFGRID_SYNC_KEY; try: openssl rand -hex 32)", ErrKey)
	}
	return key, nil
}

func bodyHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func mac(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{'\n'})
		}
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// SignRequest returns the signature for a request. The signed text names the
// method, path, sender and time, and the hash of the body, so none of them can
// be changed in flight.
func SignRequest(key []byte, method, path, node string, unix int64, body []byte) string {
	return mac(key, "offgrid-sync-v1 request", method, path, node, strconv.FormatInt(unix, 10), bodyHash(body))
}

// SignResponse signs a response. It covers the request's signature, so a
// response cannot be reused for a different request, and the status and body.
func SignResponse(key []byte, requestSig string, status int, body []byte) string {
	return mac(key, "offgrid-sync-v1 response", requestSig, strconv.Itoa(status), bodyHash(body))
}

func sigEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// authError says why a request was refused. Msg is safe to send to the caller.
type authError struct {
	Code string
	Msg  string
}

func (e *authError) Error() string { return e.Msg }

// parseAuthHeaders validates the presence and integer syntax of the sync auth
// headers without reading the request body or revealing the server's clock.
func parseAuthHeaders(r *http.Request) (node, sig string, unix int64, err error) {
	node = r.Header.Get(HeaderNode)
	ts := r.Header.Get(HeaderTime)
	sig = r.Header.Get(HeaderSig)
	if node == "" || ts == "" || sig == "" {
		return "", "", 0, &authError{CodeUnauthorized, "the request is not signed (missing " + HeaderSig + "); both replicas need the same sync key"}
	}
	unix, perr := strconv.ParseInt(ts, 10, 64)
	if perr != nil {
		return "", "", 0, &authError{CodeUnauthorized, "the request time is not a number"}
	}
	return node, sig, unix, nil
}

// verifyRequest checks the headers of r against key. body is the full body.
func verifyRequest(key []byte, r *http.Request, body []byte, now time.Time, window time.Duration) (node string, err error) {
	node, sig, unix, err := parseAuthHeaders(r)
	if err != nil {
		return "", err
	}
	want := SignRequest(key, r.Method, r.URL.Path, node, unix, body)
	if !sigEqual(sig, want) {
		return "", &authError{CodeUnauthorized, "the signature does not match: the sync keys differ, or the request was changed"}
	}
	// Only a correctly signed request learns about the clock. That keeps a
	// stranger from using this message to probe the server's time.
	diff := now.Sub(time.Unix(unix, 0))
	if diff < -window || diff > window {
		return "", &authError{CodeClockSkew, fmt.Sprintf("the clocks differ by %s, more than the allowed %s: set the time on the replica that is wrong (or raise -window)", diff.Round(time.Second).Abs(), window)}
	}
	return node, nil
}
