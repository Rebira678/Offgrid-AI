package phone

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func sg() signer { return signer{key: bytes.Repeat([]byte{1}, 32)} }

func TestASessionRoundTripsAndCarriesItsDevice(t *testing.T) {
	tok := sg().issue("dev_a", t0, time.Hour)
	c, err := sg().verify(tok, t0.Add(time.Minute))
	if err != nil || c.Device != "dev_a" || c.Expiry != t0.Add(time.Hour).Unix() {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestASessionThatWasTamperedWithOrSignedByAnotherKeyIsRefused(t *testing.T) {
	tok := sg().issue("dev_a", t0, time.Hour)
	for i := 0; i < len(tok); i++ {
		b := []byte(tok)
		if b[i] == '.' {
			continue
		}
		b[i] ^= 1
		if _, err := sg().verify(string(b), t0); err == nil {
			t.Fatalf("a change at byte %d was accepted", i)
		}
	}
	if _, err := (signer{key: bytes.Repeat([]byte{2}, 32)}).verify(tok, t0); err == nil {
		t.Fatal("another key verified it")
	}
	for _, bad := range []string{"", "v1", "v1..", "v2." + strings.Split(tok, ".")[1] + "." + strings.Split(tok, ".")[2], tok + ".x", strings.Repeat("a", maxTokenLen+1)} {
		if _, err := sg().verify(bad, t0); err == nil {
			t.Errorf("%.20q accepted", bad)
		}
	}
}

func TestASessionIsNotAChallengeAndAChallengeIsNotASession(t *testing.T) {
	s := sg()
	if _, err := s.verify(s.challenge(t0), t0); err == nil {
		t.Fatal("a challenge worked as a session")
	}
	if err := s.checkChallenge(s.issue("dev_a", t0, time.Hour), t0); err == nil {
		t.Fatal("a session worked as a challenge")
	}
}

func TestSessionTimeBoundaries(t *testing.T) {
	tok := sg().issue("dev_a", t0, time.Hour)
	if _, err := sg().verify(tok, t0.Add(time.Hour-time.Second)); err != nil {
		t.Errorf("one second before expiry: %v", err)
	}
	if _, err := sg().verify(tok, t0.Add(time.Hour)); err != errExpired {
		t.Errorf("at expiry: %v", err)
	}
	if _, err := sg().verify(tok, t0.Add(-clockSlack+time.Second)); err != nil {
		t.Errorf("a clock a little behind: %v", err)
	}
	if _, err := sg().verify(tok, t0.Add(-clockSlack-time.Minute)); err == nil {
		t.Error("a token issued far in the future was accepted")
	}
}

func TestChallengeTimeBoundaries(t *testing.T) {
	s := sg()
	n := s.challenge(t0)
	if err := s.checkChallenge(n, t0.Add(ChallengeTTL)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := s.checkChallenge(n, t0.Add(ChallengeTTL+time.Second)); err == nil {
		t.Error("late")
	}
	if err := s.checkChallenge(n, t0.Add(-clockSlack-time.Second)); err == nil {
		t.Error("from the future")
	}
	a, b := s.challenge(t0), s.challenge(t0)
	if a == b {
		t.Error("two challenges at the same instant are equal")
	}
}

func TestNoncesAreSingleUseAndForgottenWhenTheyCouldNoLongerBeAnswered(t *testing.T) {
	var n nonces
	if !n.claim("a", t0) || n.claim("a", t0.Add(time.Second)) {
		t.Fatal("single use")
	}
	n.claim("b", t0.Add(ChallengeTTL+clockSlack+time.Second))
	n.mu.Lock()
	_, kept := n.used["a"]
	n.mu.Unlock()
	if kept {
		t.Fatal("an old nonce was kept forever")
	}
}

func TestTheLimiterTableDoesNotGrowWithoutBound(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 5000; i++ {
		l.fail(strings.Repeat("x", 3)+string(rune(i+256)), t0)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 4097 {
		t.Fatalf("%d keys", len(l.fails))
	}
}

func TestTheLimiterWindowIsExactAndPerKey(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 5; i++ {
		l.fail("a", t0)
	}
	if l.allowed("a", t0.Add(59*time.Second)) {
		t.Error("allowed inside the window")
	}
	if !l.allowed("b", t0) {
		t.Error("another address was locked out")
	}
	if !l.allowed("a", t0.Add(60*time.Second)) {
		t.Error("still locked at the end of the window")
	}
	l2 := newLimiter()
	for i := 0; i < 4; i++ {
		l2.fail("a", t0)
	}
	if !l2.allowed("a", t0) {
		t.Error("locked one failure early")
	}
}
