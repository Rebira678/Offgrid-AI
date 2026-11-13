package sqlstore

import (
	"strings"
	"testing"
)

func TestParseInviteLinkEdgeCases(t *testing.T) {
	validAdmin := "dev_3ea058e9ffc372acd01022df5ac77dd0"
	validToken := "ABCDEFGHIJKLMNOPQRSTUVWX" // 24 chars base32

	// 1. Valid links
	for _, link := range []string{
		"og-invite/1/" + validAdmin + "/" + validToken + "/",
		"og-invite/1/" + validAdmin + "/" + validToken,
		"  og-invite/1/" + validAdmin + "/" + validToken + "  \n",
	} {
		l, err := ParseInviteLink(link)
		if err != nil {
			t.Errorf("valid link %q failed: %v", link, err)
		}
		if l.Admin != validAdmin || l.Token != validToken {
			t.Errorf("parsed %+v, want admin=%s tok=%s", l, validAdmin, validToken)
		}
	}

	// 2. Malformed links
	invalidLinks := []string{
		"",
		"http://localhost:7878/invite",
		"og-invite/2/" + validAdmin + "/" + validToken + "/", // unsupported version 2
		"og-invite/1/short-admin/" + validToken + "/",
		"og-invite/1/" + validAdmin + "/short-token/",
		"og-invite/1/" + validAdmin + "/" + strings.Repeat("A", 100) + "/",
		"og-invite/1/" + validAdmin + "/INVALID_CHARS_123456789012/",
	}

	for _, bad := range invalidLinks {
		if _, err := ParseInviteLink(bad); err == nil {
			t.Errorf("accepted bad invite link %q", bad)
		}
	}
}
