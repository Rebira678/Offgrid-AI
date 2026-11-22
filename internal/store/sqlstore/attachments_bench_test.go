package sqlstore

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"offgrid/internal/task"
)

func BenchmarkAttachmentsAdd(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "bench.db"), Options{
		NodeID:  "node-bench",
		BlobDir: filepath.Join(dir, "blobs"),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	tk, _, err := s.Create(ctx, task.NewTask{Title: "Benchmark Task", Priority: task.PriorityMedium})
	if err != nil {
		b.Fatal(err)
	}
	data := jpeg(32<<10, 1)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		// New task every 15 attachments so we don't hit the 20-attachment cap
		if i > 0 && i%15 == 0 {
			tk, _, err = s.Create(ctx, task.NewTask{Title: fmt.Sprintf("Benchmark Task %d", i), Priority: task.PriorityMedium})
			if err != nil {
				b.Fatal(err)
			}
		}
		data[len(data)-1] = byte(i)
		data[len(data)-2] = byte(i >> 8)
		_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", fmt.Sprintf("photo-%d.jpg", i))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAttachmentsList(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "bench.db"), Options{
		NodeID:  "node-bench",
		BlobDir: filepath.Join(dir, "blobs"),
	})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	tk, _, err := s.Create(ctx, task.NewTask{Title: "Benchmark List Task", Priority: task.PriorityMedium})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		data := jpeg(1024+i, byte(i))
		_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", fmt.Sprintf("att-%d.jpg", i))
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, err := s.Attachments(ctx, tk.ID)
		if err != nil {
			b.Fatal(err)
		}
		if len(list) != 15 {
			b.Fatalf("expected 15 attachments, got %d", len(list))
		}
	}
}
