package identity

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestIdentityNilSafety(t *testing.T) {
	var nilIdent *Identity

	if got := nilIdent.ID(); got != "" {
		t.Errorf("nil.ID() = %q, want empty string", got)
	}
	if got := nilIdent.Algorithm(); got != AlgECDSAP256 {
		t.Errorf("nil.Algorithm() = %q, want %q", got, AlgECDSAP256)
	}
	if got := nilIdent.PublicKey(); got != "" {
		t.Errorf("nil.PublicKey() = %q, want empty string", got)
	}

	sig, err := nilIdent.Sign([]byte("test"))
	if err == nil || sig != nil {
		t.Errorf("nil.Sign() should return error, got sig=%v, err=%v", sig, err)
	}

	// Zero-value struct safety
	zeroIdent := &Identity{}
	sig, err = zeroIdent.Sign([]byte("test"))
	if err == nil || sig != nil {
		t.Errorf("zeroIdent.Sign() should return error, got sig=%v, err=%v", sig, err)
	}

	// Nil slog LogValue
	lv := nilIdent.LogValue()
	if lv.Kind() != slog.KindAny && lv.Kind() != 0 {
		t.Logf("nil LogValue returned: %v", lv)
	}

	// Nil JSON Marshal
	js, err := json.Marshal(nilIdent)
	if err != nil {
		t.Fatalf("json.Marshal(nil) error: %v", err)
	}
	if string(js) != "null" {
		t.Errorf("json.Marshal(nil) = %s, want null", js)
	}
}

func TestIdentityCleanPathHandling(t *testing.T) {
	tmp := t.TempDir()
	messyPath := filepath.Join(tmp, "sub//dir/../dir//device.key")

	id, err := Create(messyPath)
	if err != nil {
		t.Fatalf("Create with messy path failed: %v", err)
	}

	cleanedPath := filepath.Clean(messyPath)
	loaded, err := Load(cleanedPath)
	if err != nil {
		t.Fatalf("Load with cleaned path failed: %v", err)
	}
	if loaded.ID() != id.ID() {
		t.Fatalf("ID mismatch: %s vs %s", loaded.ID(), id.ID())
	}

	// Loading again via another redundant slash variation
	loaded2, err := Load(filepath.Join(tmp, "sub", "dir", ".", "device.key"))
	if err != nil {
		t.Fatalf("Load with relative dot failed: %v", err)
	}
	if loaded2.ID() != id.ID() {
		t.Fatalf("ID mismatch: %s vs %s", loaded2.ID(), id.ID())
	}
}

func TestIdentityFastPathPublicValidation(t *testing.T) {
	// A valid P-256 base64url public key must have exactly 87 characters
	// Test varying lengths from 1 to 200 characters
	for l := 0; l <= 120; l++ {
		if l == 87 {
			continue // valid length
		}
		candidate := AlgECDSAP256 + ":" + base64.RawURLEncoding.EncodeToString(make([]byte, l))
		if _, err := DeviceID(candidate); err == nil {
			t.Errorf("DeviceID unexpectedly accepted candidate of len %d", l)
		}
	}

	// Test point format: must start with 0x04 (uncompressed)
	// Create 65 bytes starting with 0x02, 0x03, 0x00
	for _, prefix := range []byte{0x00, 0x02, 0x03, 0x05, 0xff} {
		raw := append([]byte{prefix}, make([]byte, 64)...)
		cand := AlgECDSAP256 + ":" + base64.RawURLEncoding.EncodeToString(raw)
		if _, err := DeviceID(cand); err == nil {
			t.Errorf("DeviceID accepted public key with invalid point prefix 0x%02x", prefix)
		}
	}
}

func TestIdentitySignatureVerificationEdgeCases(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("critical security payload")
	sig, err := id.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Signature must be exactly 64 bytes
	for _, badLen := range []int{0, 1, 32, 63, 65, 128} {
		badSig := make([]byte, badLen)
		if err := Verify(id.PublicKey(), msg, badSig); err == nil {
			t.Errorf("Verify accepted signature of length %d", badLen)
		}
	}

	// 2. All-zero r or s
	zeroR := append(make([]byte, 32), sig[32:]...)
	if err := Verify(id.PublicKey(), msg, zeroR); err == nil {
		t.Error("Verify accepted signature with r=0")
	}

	zeroS := append(append([]byte(nil), sig[:32]...), make([]byte, 32)...)
	if err := Verify(id.PublicKey(), msg, zeroS); err == nil {
		t.Error("Verify accepted signature with s=0")
	}

	// 3. Out of curve range: r >= N or s >= N
	N := elliptic.P256().Params().N
	nBytes := N.Bytes()
	sigN_R := append(append([]byte(nil), nBytes...), sig[32:]...)
	if err := Verify(id.PublicKey(), msg, sigN_R); err == nil {
		t.Error("Verify accepted signature with r=N")
	}

	sigN_S := append(append([]byte(nil), sig[:32]...), nBytes...)
	if err := Verify(id.PublicKey(), msg, sigN_S); err == nil {
		t.Error("Verify accepted signature with s=N")
	}

	// 4. Large N+1
	nPlusOne := new(big.Int).Add(N, big.NewInt(1)).Bytes()
	if len(nPlusOne) == 32 {
		sigLarge := append(append([]byte(nil), sig[:32]...), nPlusOne...)
		if err := Verify(id.PublicKey(), msg, sigLarge); err == nil {
			t.Error("Verify accepted signature with s=N+1")
		}
	}

	// 5. Normal signature must verify cleanly
	if err := Verify(id.PublicKey(), msg, sig); err != nil {
		t.Fatalf("Valid signature failed: %v", err)
	}
}

func TestIdentityConcurrentSignAndVerifyStress(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 25
	const iterations = 40
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*iterations)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				payload := []byte(fmt.Sprintf("worker-%d-iter-%d", gid, i))
				sig, err := id.Sign(payload)
				if err != nil {
					errCh <- fmt.Errorf("Sign failed: %w", err)
					return
				}
				if len(sig) != 64 {
					errCh <- fmt.Errorf("signature length = %d", len(sig))
					return
				}
				if err := Verify(id.PublicKey(), payload, sig); err != nil {
					errCh <- fmt.Errorf("Verify failed: %w", err)
					return
				}
				// Verify tampered payload fails
				if err := Verify(id.PublicKey(), append(payload, '!'), sig); err == nil {
					errCh <- errors.New("tampered payload unexpectedly verified")
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}

func TestIdentityCorruptFileEdgeScenarios(t *testing.T) {
	dir := t.TempDir()

	// 1. Directory as file path
	dirPath := filepath.Join(dir, "is_a_directory")
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dirPath)
	if err == nil {
		t.Error("Load on directory should return error")
	}

	// 2. Trailing carriage return (CRLF)
	p := filepath.Join(dir, "crlf.key")
	id, _ := Generate()
	rawKey := id.encode()
	// Replace \n with \r\n
	crlfData := append(rawKey[:len(rawKey)-1], '\r', '\n')
	if err := os.WriteFile(p, crlfData, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(p)
	if err == nil || !errors.Is(err, ErrCorrupt) {
		t.Errorf("Load on CRLF file should fail with ErrCorrupt, got %v", err)
	}

	// 3. Leading whitespace before magic
	pWhitespace := filepath.Join(dir, "ws.key")
	wsData := append([]byte("  "), rawKey...)
	if err := os.WriteFile(pWhitespace, wsData, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(pWhitespace)
	if err == nil || !errors.Is(err, ErrCorrupt) {
		t.Errorf("Load on leading whitespace should fail with ErrCorrupt, got %v", err)
	}
}

func TestIdentityPermissionsEdgeReadonly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission test")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "readonly.key")
	id, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}

	// Set mode 0400 (read-only for owner)
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("Load(0400) should succeed for owner read-only, got: %v", err)
	}
	if loaded.ID() != id.ID() {
		t.Errorf("ID mismatch: %s vs %s", loaded.ID(), id.ID())
	}
}
