package sqlstore

import (
	"testing"
)

func FuzzCheckAttachmentName(f *testing.F) {
	seeds := []string{
		"photo.jpg",
		"Inspection Report 2026.pdf",
		"📸_fault.png",
		"../../etc/passwd",
		"a/b/c.jpg",
		"a\\b\\c.jpg",
		"\x00null.jpg",
		"\nbreak.jpg",
		"\tcontrol.jpg",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		err := checkAttachmentName(name)
		if err == nil {
			// If it passes, assert it contains no forbidden runes and rune count <= 120
			runes := []rune(name)
			if len(runes) > maxAttachmentName {
				t.Fatalf("checkAttachmentName accepted name with %d runes (> %d)", len(runes), maxAttachmentName)
			}
			for _, r := range name {
				if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
					t.Fatalf("checkAttachmentName accepted forbidden rune %q", r)
				}
			}
		}
	})
}
