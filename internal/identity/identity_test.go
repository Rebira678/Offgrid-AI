package identity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var idRe = regexp.MustCompile(`^dev_[0-9a-f]{32}$`)

func keyPath(t *testing.T) string { return filepath.Join(t.TempDir(), "sub", "device.key") }

// An independent vector: the P-256 generator point, hashed with Python's hashlib.
// The Day 33 phone page hashes the same 65 raw bytes with SHA-256.
func TestDeviceIDKnownVector(t *testing.T) {
	pub := "ecdsa-p256:BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"
	got, err := DeviceID(pub)
	if err != nil {
		t.Fatal(err)
	}
	if want := "dev_698bea63dc44a344663ff1429aea1084"; got != want {
		t.Fatalf("DeviceID = %s, want %s", got, want)
	}
}

func TestDeviceIDRejectsBadPublicKeys(t *testing.T) {
	for _, s := range []string{
		"", "ecdsa-p256", "ecdsa-p256:", "ecdsa-p256:!!!", "ecdsa-p256:AAAA",
		"ed25519:BGsX0fLhLEJH-Lzm5WOkQPJ3A32BLeszoPShOUXYmMKWT-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU",
		// right length, not on the curve
		"ecdsa-p256:" + base64.RawURLEncoding.EncodeToString(append([]byte{4}, bytes.Repeat([]byte{1}, 64)...)),
	} {
		if _, err := DeviceID(s); err == nil {
			t.Errorf("DeviceID(%q) accepted", s)
		}
	}
}

func TestKeySurvivesRestartAndIDIsStable(t *testing.T) {
	p := keyPath(t)
	a, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if !idRe.MatchString(a.ID()) {
		t.Fatalf("ID %q has the wrong shape", a.ID())
	}
	sig, err := a.Sign([]byte("before restart"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // "restart": nothing is kept in memory
		b, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if b.ID() != a.ID() || b.PublicKey() != a.PublicKey() {
			t.Fatalf("restart %d changed the identity: %s vs %s", i, b.ID(), a.ID())
		}
		if err := Verify(a.PublicKey(), []byte("before restart"), sig); err != nil {
			t.Fatalf("an old signature no longer verifies: %v", err)
		}
		s2, err := b.Sign([]byte("after restart"))
		if err != nil {
			t.Fatal(err)
		}
		if err := Verify(a.PublicKey(), []byte("after restart"), s2); err != nil {
			t.Fatalf("a signature from the reloaded key does not verify against the original public key: %v", err)
		}
	}
	if got, _ := DeviceID(a.PublicKey()); got != a.ID() {
		t.Fatalf("ID %s is not the hash of the public key (%s)", a.ID(), got)
	}
}

func TestTwoIdentitiesDiffer(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	if a.ID() == b.ID() || a.PublicKey() == b.PublicKey() {
		t.Fatal("two generated identities are equal")
	}
}

func TestCreateWritesPrivateFileInPrivateFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	p := keyPath(t)
	if _, err := Create(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %o, want 600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %o, want 700", di.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "offgrid-key v1 ecdsa-p256 ") || !strings.HasSuffix(string(b), "\n") || strings.Count(string(b), "\n") != 1 {
		t.Errorf("the file must start with the algorithm tag and be one line: %q", b[:min(len(b), 40)])
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("stray files left next to the key: %v", ents)
	}
}

func TestCreateNeverOverwrites(t *testing.T) {
	p := keyPath(t)
	a, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	if _, err := Create(p); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create: %v, want ErrExists", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("the existing key file was changed")
	}
	if b, _ := Load(p); b.ID() != a.ID() {
		t.Fatal("identity changed")
	}
}

func TestLoadMissingIsNotFoundAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nope", "device.key")
	if _, err := Load(p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load: %v, want ErrNotFound", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("Load left %v behind", ents)
	}
}

func TestLoadRefusesWrongPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o606, 0o666, 0o664, 0o601, 0o602, 0o610} {
		p := keyPath(t)
		if _, err := Create(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if !errors.Is(err, ErrPermissions) {
			t.Errorf("mode %o: %v, want ErrPermissions", mode, err)
			continue
		}
		if !strings.Contains(err.Error(), "chmod 600") || !strings.Contains(err.Error(), p) {
			t.Errorf("mode %o: the message must name the file and the fix: %v", mode, err)
		}
		if _, _, err := LoadOrCreate(p); !errors.Is(err, ErrPermissions) {
			t.Errorf("mode %o: LoadOrCreate: %v, want ErrPermissions (it must not make a new key)", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		p := keyPath(t)
		if _, err := Create(p); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		if _, err := Load(p); err != nil {
			t.Errorf("mode %o refused: %v", mode, err)
		}
	}
}

func TestLoadRefusesAKeyThroughASymlinkToAReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and POSIX permission bits")
	}
	real := keyPath(t)
	if _, err := Create(real); err != nil {
		t.Fatal(err)
	}
	os.Chmod(real, 0o644)
	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, err := Load(link); !errors.Is(err, ErrPermissions) {
		t.Fatalf("Load via symlink: %v, want ErrPermissions (the target's mode counts)", err)
	}
}

func goodLine(t *testing.T) (line string, der []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ = x509.MarshalPKCS8PrivateKey(k)
	return "offgrid-key v1 ecdsa-p256 " + base64.RawURLEncoding.EncodeToString(der) + "\n", der
}

func TestLoadRefusesCorruptFilesWithoutEchoingThem(t *testing.T) {
	good, _ := goodLine(t)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384DER, _ := x509.MarshalPKCS8PrivateKey(p384)
	const secret = "SECRETSENTINEL0123456789"
	cases := map[string]string{
		"empty":               "",
		"garbage":             secret + "\x00\xff",
		"truncated":           good[:len(good)/2],
		"no newline":          strings.TrimSuffix(good, "\n"),
		"wrong magic":         "xxxxxx-key v1 ecdsa-p256 " + strings.Fields(good)[3] + "\n",
		"not base64":          "offgrid-key v1 ecdsa-p256 " + secret + "!!!\n",
		"base64 but not DER":  "offgrid-key v1 ecdsa-p256 " + base64.RawURLEncoding.EncodeToString([]byte(secret)) + "\n",
		"an RSA key":          "offgrid-key v1 ecdsa-p256 " + base64.RawURLEncoding.EncodeToString(rsaDER) + "\n",
		"a P-384 key":         "offgrid-key v1 ecdsa-p256 " + base64.RawURLEncoding.EncodeToString(p384DER) + "\n",
		"two lines":           good + good,
		"trailing junk":       good + secret + "\n",
		"extra field":         strings.Replace(good, "ecdsa-p256 ", "ecdsa-p256 "+secret+" ", 1),
		"huge":                strings.Repeat("A", 1<<20),
		"standard base64 pad": "offgrid-key v1 ecdsa-p256 " + base64.StdEncoding.EncodeToString([]byte(secret)) + "\n",
	}
	for name, content := range cases {
		p := keyPath(t)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
			continue
		}
		if strings.Contains(err.Error(), secret) || (len(content) > 60 && strings.Contains(err.Error(), content[len(content)/2:len(content)/2+30])) {
			t.Errorf("%s: the error repeats the file's content: %v", name, err)
		}
		if _, _, err := LoadOrCreate(p); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: LoadOrCreate: %v, want ErrCorrupt (it must never replace a bad key silently)", name, err)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Errorf("%s: the bad file was modified", name)
		}
	}
}

func TestLoadUnknownAlgorithmOrVersionIsUnsupportedNotCorrupt(t *testing.T) {
	good, _ := goodLine(t)
	f := strings.Fields(good)
	for name, content := range map[string]string{
		"newer version": "offgrid-key v2 ecdsa-p256 " + f[3] + "\n",
		"new algorithm": "offgrid-key v1 ed25519 " + f[3] + "\n",
	} {
		p := keyPath(t)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(content), 0o600)
		_, err := Load(p)
		if !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrUnsupported only", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "newer") {
			t.Errorf("%s: the message should say the file may come from a newer Offgrid: %v", name, err)
		}
	}
}

func TestLoadOrCreateCreatesOnceThenLoads(t *testing.T) {
	p := keyPath(t)
	a, created, err := LoadOrCreate(p)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	b, created, err := LoadOrCreate(p)
	if err != nil || created || b.ID() != a.ID() {
		t.Fatalf("second: created=%v err=%v same=%v", created, err, b != nil && b.ID() == a.ID())
	}
}

func TestLoadOrCreateRacingStartsAgreeOnOneKey(t *testing.T) {
	p := keyPath(t)
	const n = 16
	var wg sync.WaitGroup
	ids := make([]string, n)
	made := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, c, err := LoadOrCreate(p)
			errs[i], made[i] = err, c
			if err == nil {
				ids[i] = id.ID()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	creators := 0
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("start %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("starts disagree: %s vs %s", ids[i], ids[0])
		}
		if made[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d starts say they created the key, want exactly 1", creators)
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
		t.Fatalf("stray files: %v", ents)
	}
}

func TestSignVerify(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	msg := []byte("the op")
	sig, err := a.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (raw r||s, what WebCrypto makes)", len(sig))
	}
	if err := Verify(a.PublicKey(), msg, sig); err != nil {
		t.Fatal(err)
	}
	if Verify(b.PublicKey(), msg, sig) == nil {
		t.Error("another device's key verified it")
	}
	if Verify(a.PublicKey(), []byte("the op!"), sig) == nil {
		t.Error("a changed message verified")
	}
	for i := range sig {
		bad := append([]byte(nil), sig...)
		bad[i] ^= 1
		if Verify(a.PublicKey(), msg, bad) == nil {
			t.Fatalf("flipping byte %d still verified", i)
		}
	}
	if Verify(a.PublicKey(), msg, append(append([]byte(nil), sig...), 0)) == nil {
		t.Error("a valid signature with a trailing byte verified")
	}
	if Verify(a.PublicKey(), msg, sig[:63]) == nil {
		t.Error("a truncated signature verified")
	}
	for _, n := range []int{0, 1, 63, 65, 72} {
		if Verify(a.PublicKey(), msg, make([]byte, n)) == nil {
			t.Errorf("a %d-byte signature verified", n)
		}
	}
	if Verify(a.PublicKey(), msg, make([]byte, 64)) == nil {
		t.Error("an all-zero signature verified")
	}
	if Verify("ed25519:AAAA", msg, sig) == nil {
		t.Error("an unknown algorithm verified")
	}
}

// No way of printing, logging or encoding an Identity may show the private key.
func TestNoKeyMaterialInAnyOutput(t *testing.T) {
	p := keyPath(t)
	id, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(p)
	der, _ := x509.MarshalPKCS8PrivateKey(id.key)
	d := id.key.D.Bytes()
	needles := []string{
		strings.TrimSpace(strings.Fields(string(file))[3]),
		base64.StdEncoding.EncodeToString(der), base64.RawStdEncoding.EncodeToString(der),
		hex.EncodeToString(d), id.key.D.String(), id.key.D.Text(16),
		base64.RawURLEncoding.EncodeToString(d), base64.StdEncoding.EncodeToString(d),
	}
	var out strings.Builder
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%T"} {
		fmt.Fprintf(&out, verb+"\n", id)
		fmt.Fprintf(&out, verb+"\n", *id)
		fmt.Fprintf(&out, verb+"\n", []*Identity{id})
	}
	fmt.Fprintln(&out, id, fmt.Sprint(id), id.String(), id.GoString())
	var tb, jb bytes.Buffer
	slog.New(slog.NewTextHandler(&tb, nil)).Info("starting", "identity", id, "id", id.ID())
	slog.New(slog.NewJSONHandler(&jb, nil)).Info("starting", slog.Any("identity", id), slog.Group("g", slog.Any("i", id)))
	js, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), id.ID()) || !strings.Contains(string(js), id.PublicKey()) {
		t.Errorf("JSON should carry the ID and public key: %s", js)
	}
	out.Write(js)
	jl, _ := json.Marshal(struct{ Who *Identity }{id})
	out.Write(jl)
	out.Write(tb.Bytes())
	out.Write(jb.Bytes())
	for _, n := range needles {
		if len(n) < 8 {
			t.Fatalf("needle %q too short to mean anything", n)
		}
		if strings.Contains(out.String(), n) {
			t.Errorf("key material %q... appears in the output:\n%s", n[:8], out.String())
		}
	}
	if !strings.Contains(out.String(), id.ID()) {
		t.Error("the output should still show the device ID")
	}
	if !strings.Contains(out.String(), "redacted") {
		t.Error("the output should say the key is redacted")
	}
	// Errors from every failure path.
	var errs strings.Builder
	for _, mode := range []os.FileMode{0o644} {
		os.Chmod(p, mode)
		_, e := Load(p)
		errs.WriteString(e.Error())
	}
	os.Chmod(p, 0o600)
	_, e := Create(p)
	errs.WriteString(e.Error())
	for _, n := range needles {
		if strings.Contains(errs.String(), n) {
			t.Errorf("key material in an error message: %s", errs.String())
		}
	}
}

func TestParseKeyFileAcceptsTheFormatItWrites(t *testing.T) {
	a, _ := Generate()
	b, err := parseKeyFile(a.encode())
	if err != nil || b.ID() != a.ID() {
		t.Fatalf("round trip: %v", err)
	}
}

