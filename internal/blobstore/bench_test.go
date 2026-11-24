package blobstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// BenchmarkPut is the cost of taking a 4 MiB photo in: stream, hash, fsync, rename. The bytes differ on
// every iteration (a repeat would take the "already here" shortcut and measure nothing). SetBytes makes
// the report say MB/s, which is what a person waiting for an attachment feels.
func BenchmarkPut(b *testing.B) {
	s, err := New(filepath.Join(b.TempDir(), "blobs"), 64<<20)
	if err != nil {
		b.Fatal(err)
	}
	buf := bytes.Repeat([]byte{0xAB}, 4<<20)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		binary.BigEndian.PutUint64(buf, uint64(i)+1)
		if _, err := s.Put(context.Background(), bytes.NewReader(buf), int64(len(buf))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResume is the cost of finishing a cut transfer: begin, copy the second half, commit. The
// first half is the "partial file" a cut left behind; resuming must cost about the second half, not the
// whole file.
func BenchmarkResume(b *testing.B) {
	s, err := New(filepath.Join(b.TempDir(), "blobs"), 64<<20)
	if err != nil {
		b.Fatal(err)
	}
	buf := bytes.Repeat([]byte{0xCD}, 4<<20)
	half := len(buf) / 2
	b.SetBytes(int64(len(buf) - half))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		binary.BigEndian.PutUint64(buf, uint64(i)+1)
		tr, err := s.Begin(hashOf(buf), int64(len(buf)))
		if err != nil {
			b.Fatal(err)
		}
		if err := tr.Copy(context.Background(), bytes.NewReader(buf[:half])); err != nil {
			b.Fatal(err)
		}
		tr.Close() // a cut: the partial stays
		tr, err = s.Begin(hashOf(buf), int64(len(buf)))
		if err != nil {
			b.Fatal(err)
		}
		if tr.Offset() != int64(half) {
			b.Fatalf("resumed at %d, want %d", tr.Offset(), half)
		}
		b.StartTimer()
		if err := tr.Copy(context.Background(), bytes.NewReader(buf[half:])); err != nil {
			b.Fatal(err)
		}
		if _, err := tr.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutConcurrent measures concurrent ingest throughput under multi-threaded writers.
func BenchmarkPutConcurrent(b *testing.B) {
	s, err := New(filepath.Join(b.TempDir(), "blobs-conc"), 256<<20)
	if err != nil {
		b.Fatal(err)
	}
	const blobSize = 1 << 20 // 1 MiB per blob
	b.SetBytes(blobSize)
	b.ReportAllocs()
	var seq uint64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := bytes.Repeat([]byte{0xEF}, blobSize)
		for pb.Next() {
			id := atomic.AddUint64(&seq, 1)
			binary.BigEndian.PutUint64(buf, id)
			if _, err := s.Put(context.Background(), bytes.NewReader(buf), int64(len(buf))); err != nil {
				b.Fatal(err)
			}
		}
	})
}
