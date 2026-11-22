package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func webp(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("RIFF\x24\x00\x00\x00WEBPVP8 "))
	return b
}

func gif(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	return b
}

func mp4(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"))
	return b
}

func pdf(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("%PDF-1.7\n%âãÏÓ\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"))
	return b
}

// TestAttachmentsUnicodeAndEmojiFilenames verifies handling of UTF-8 filenames,
// boundary length of 120 runes, and rejection of 121 runes.
func TestAttachmentsUnicodeAndEmojiFilenames(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Solar Inverter Fault")

	// Multi-byte Unicode and Emoji
	unicodeName := "📸_インバーター_Reparación_2026_🚨.jpg"
	data := jpeg(1024, 42)
	att, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(data), int64(len(data)), "image/jpeg", unicodeName)
	if err != nil {
		t.Fatalf("AddAttachment with Unicode failed: %v", err)
	}
	if att.Name != unicodeName {
		t.Fatalf("name mismatch: got %q, want %q", att.Name, unicodeName)
	}

	// Boundary test: Exactly 120 runes (should succeed)
	name120 := strings.Repeat("日", 120)
	att120, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(1024, 43)), 1024, "image/jpeg", name120)
	if err != nil {
		t.Fatalf("120-rune name rejected: %v", err)
	}
	if att120.Name != name120 {
		t.Fatalf("120-rune name mismatch: len=%d", len([]rune(att120.Name)))
	}

	// Boundary test: Exactly 121 runes (should fail)
	name121 := strings.Repeat("日", 121)
	_, err = s.AddAttachment(ctx, tk.ID, bytes.NewReader(jpeg(1024, 44)), 1024, "image/jpeg", name121)
	if !errors.Is(err, ErrBadAttachment) {
		t.Fatalf("expected ErrBadAttachment for 121 runes, got %v", err)
	}

	// Whitespace trimming
	paddedName := "   circuit-breaker.png   \t\n "
	attPadded, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(png(512)), 512, "image/png", paddedName)
	if err != nil {
		t.Fatalf("padded name failed: %v", err)
	}
	if attPadded.Name != "circuit-breaker.png" {
		t.Fatalf("expected trimmed name, got %q", attPadded.Name)
	}
}

// TestAttachmentsMediaTypeSniffMatrix verifies detection and allowlist adherence across formats.
func TestAttachmentsMediaTypeSniffMatrix(t *testing.T) {
	ctx := context.Background()
	s, _ := freshStore(t)
	tk := mk(t, s, "Inspection Checklist")

	validFormats := []struct {
		desc string
		mime string
		data []byte
	}{
		{"WebP Image", "image/webp", webp(1000)},
		{"GIF Image", "image/gif", gif(500)},
		{"MP4 Video", "video/mp4", mp4(2048)},
		{"PDF Document", "application/pdf", pdf(1024)},
		{"Plain UTF-8 Text", "text/plain", []byte("Field technician report: verified junction box.\nOK.\n")},
	}

	for _, tc := range validFormats {
		t.Run(tc.desc, func(t *testing.T) {
			att, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(tc.data), int64(len(tc.data)), tc.mime, tc.desc)
			if err != nil {
				t.Fatalf("AddAttachment(%s) failed: %v", tc.mime, err)
			}
			if att.Type != tc.mime || !att.Present {
				t.Fatalf("attachment %+v not as expected", att)
			}
		})
	}

	// Test forbidden and malicious content types
	maliciousFormats := []struct {
		desc string
		mime string
		data []byte
	}{
		{"SVG XML", "image/svg+xml", []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>")},
		{"HTML Document", "text/html", []byte("<!DOCTYPE html><html><body><h1>Phishing</h1></body></html>")},
		{"JavaScript Payload", "application/javascript", []byte("fetch('/api/keys')")},
		{"Shell Script", "application/x-sh", []byte("#!/bin/sh\nrm -rf /")},
		{"Binary in Text", "text/plain", []byte("Some text \x00 with forbidden null bytes")},
		{"Mismatch JPEG as PNG", "image/png", jpeg(500, 1)},
		{"Mismatch PNG as PDF", "application/pdf", png(500)},
	}

	for _, tc := range maliciousFormats {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := s.AddAttachment(ctx, tk.ID, bytes.NewReader(tc.data), int64(len(tc.data)), tc.mime, "malicious")
			if !errors.Is(err, ErrBadAttachment) {
				t.Fatalf("expected ErrBadAttachment for %s, got %v", tc.desc, err)
			}
		})
	}
}
