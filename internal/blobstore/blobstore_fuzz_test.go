package blobstore

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func FuzzValidHash(f *testing.F) {
	seeds := []string{
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"",
		"short",
		"0123456789abcdef0123456789ABCDEF0123456789abcdef0123456789abcdef",
		"../../etc/passwd",
		"\x00",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		valid := ValidHash(s)
		if valid {
			if len(s) != 64 {
				t.Fatalf("ValidHash returned true for length %d", len(s))
			}
			for i := 0; i < len(s); i++ {
				c := s[i]
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
					t.Fatalf("ValidHash returned true for non-lowercase-hex %c", c)
				}
			}
		}
	})
}

func FuzzPutExpected(f *testing.F) {
	dir := f.TempDir()
	store, err := New(filepath.Join(dir, "fuzz-blobs"), 1024)
	if err != nil {
		f.Fatal(err)
	}

	f.Add([]byte("hello world"), "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", int64(11))
	f.Add([]byte(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", int64(0))
	f.Add([]byte("random data"), "badhash", int64(11))

	f.Fuzz(func(t *testing.T, data []byte, wantHash string, size int64) {
		_, _ = store.PutExpected(context.Background(), bytes.NewReader(data), wantHash, size)
	})
}
