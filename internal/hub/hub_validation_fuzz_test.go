package hub

import (
	"testing"
)

// FuzzGroupNameValidation tests that group name validation behaves safely and deterministically
// on arbitrary string inputs.
func FuzzGroupNameValidation(f *testing.F) {
	seeds := []string{
		"crew",
		"team-1",
		"a",
		"0",
		"-bad",
		"Crew",
		"team_one",
		"team.one",
		"team/one",
		"a long group name that exceeds forty characters total limit",
		"",
		" ",
		"\x00\x01\x02",
		"dev_1234567890abcdef1234567890abcdef",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		matched := groupRe.MatchString(name)
		if matched {
			if len(name) < 1 || len(name) > 40 {
				t.Fatalf("group name %q matched but length %d outside [1, 40]", name, len(name))
			}
			if name[0] == '-' {
				t.Fatalf("group name %q matched but starts with hyphen", name)
			}
			for i := 0; i < len(name); i++ {
				c := name[i]
				isLower := c >= 'a' && c <= 'z'
				isDigit := c >= '0' && c <= '9'
				isHyphen := c == '-'
				if !isLower && !isDigit && !isHyphen {
					t.Fatalf("group name %q matched with invalid char %q", name, c)
				}
			}
		}
	})
}

// FuzzFounderValidation tests that founder device ID validation matches strictly.
func FuzzFounderValidation(f *testing.F) {
	seeds := []string{
		"dev_0123456789abcdef0123456789abcdef",
		"dev_00000000000000000000000000000000",
		"dev_ffffffffffffffffffffffffffffffff",
		"dev_short",
		"dev_1234567890abcdef1234567890abcdef1",
		"node_0123456789abcdef0123456789abcdef",
		"DEV_0123456789ABCDEF0123456789ABCDEF",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, founder string) {
		matched := founderRe.MatchString(founder)
		if matched {
			if len(founder) != 36 {
				t.Fatalf("founder %q matched but length != 36", founder)
			}
			if founder[:4] != "dev_" {
				t.Fatalf("founder %q matched but does not start with dev_", founder)
			}
			hexPart := founder[4:]
			for i := 0; i < len(hexPart); i++ {
				c := hexPart[i]
				isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
				if !isHex {
					t.Fatalf("founder %q matched with non-hex char %q", founder, c)
				}
			}
		}
	})
}
