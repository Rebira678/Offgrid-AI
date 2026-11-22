package blobstore

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func BenchmarkBlobstorePut(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1 << 10},
		{"64KB", 64 << 10},
		{"1MB", 1 << 20},
	}

	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			store, err := New(filepath.Join(b.TempDir(), "bench-blobs"), 10<<20)
			if err != nil {
				b.Fatal(err)
			}
			data := bytes.Repeat([]byte("A"), s.size)
			ctx := context.Background()

			b.ResetTimer()
			b.SetBytes(int64(s.size))
			for i := 0; i < b.N; i++ {
				// Vary 1 byte so each iteration creates a unique blob
				data[0] = byte(i)
				data[1] = byte(i >> 8)
				_, err := store.Put(ctx, bytes.NewReader(data), int64(s.size))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBlobstoreOpenAndVerify(b *testing.B) {
	store, err := New(filepath.Join(b.TempDir(), "bench-blobs"), 10<<20)
	if err != nil {
		b.Fatal(err)
	}
	data := bytes.Repeat([]byte("X"), 64<<10)
	ref, err := store.Put(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if err := store.Verify(ref.Hash); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBlobstoreConcurrentPut(b *testing.B) {
	store, err := New(filepath.Join(b.TempDir(), "bench-blobs"), 10<<20)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	const dataSize = 16 << 10

	b.ResetTimer()
	b.SetBytes(dataSize)
	b.RunParallel(func(pb *testing.PB) {
		data := bytes.Repeat([]byte("C"), dataSize)
		i := 0
		for pb.Next() {
			i++
			data[0] = byte(i)
			data[1] = byte(i >> 8)
			_, err := store.Put(ctx, bytes.NewReader(data), dataSize)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
