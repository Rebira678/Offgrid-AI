package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"offgrid/internal/store/sqlstore"
)

func TestEnrollEdgeOversizedBodyAndMalformedJSON(t *testing.T) {
	env := newEnrollEnv(t)
	_, link := env.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)

	// 1. Oversized body (> 8192 bytes)
	hugePayload := bytes.Repeat([]byte("A"), maxEnrollBody+10)
	resp, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(hugePayload))
	if err != nil {
		t.Fatalf("POST /v1/enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 Request Entity Too Large", resp.StatusCode)
	}

	// 2. Trailing garbage after valid JSON
	req := request(t, link.Token, idb, "pi")
	b, _ := json.Marshal(req)
	withGarbage := append(b, []byte(" EXTRA_DATA")...)
	resp2, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(withGarbage))
	if err != nil {
		t.Fatalf("POST with garbage: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 Bad Request for trailing data", resp2.StatusCode)
	}

	// 3. Unknown JSON fields (strict decoding)
	unknownJSON := []byte(`{"token":"` + link.Token + `","device":"dev_1","publicKey":"key","label":"lbl","proof":"AA==","unknown_field":123}`)
	resp3, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(unknownJSON))
	if err != nil {
		t.Fatalf("POST unknown JSON: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 Bad Request for unknown fields", resp3.StatusCode)
	}
}

func TestEnrollRateLimitWindowSlideAndRecovery(t *testing.T) {
	env := newEnrollEnv(t)
	_, link := env.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)

	// Burn 10 attempts with invalid tokens
	badReq := request(t, "AAAAAAAAAAAAAAAAAAAAAAAA", idb, "pi")
	body, _ := json.Marshal(badReq)

	for i := 0; i < 10; i++ {
		r, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d status = %d, want 403", i+1, r.StatusCode)
		}
	}

	// 11th attempt: even with valid invite, must get 429 Too Many Requests
	validReq := request(t, link.Token, idb, "pi")
	vBody, _ := json.Marshal(validReq)
	r11, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(vBody))
	if err != nil {
		t.Fatal(err)
	}
	defer r11.Body.Close()
	if r11.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("11th attempt status = %d, want 429 Too Many Requests", r11.StatusCode)
	}

	// Advance clock by 61 seconds (past the 1-minute window)
	env.clk.t = env.clk.t.Add(61 * time.Second)

	// Valid attempt now succeeds cleanly
	rAfter, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(vBody))
	if err != nil {
		t.Fatal(err)
	}
	defer rAfter.Body.Close()
	if rAfter.StatusCode != http.StatusOK {
		t.Fatalf("after window slide, status = %d, want 200 OK", rAfter.StatusCode)
	}
}

func TestEnrollEndpointReturns404WhenEnrollerIsDisabled(t *testing.T) {
	clk := &fakeClock{t0}
	a, _ := newSignedStore(t, "laptop", clk)

	// Handler without Enroller
	h := Handler(a, ServerOptions{Key: key, Enroller: nil, Now: clk.now})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+PathEnroll, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 Not Found when Enroller is nil", resp.StatusCode)
	}
}

func TestCheckEnrollmentSecurityChecks(t *testing.T) {
	env := newEnrollEnv(t)
	_, link := env.invite(t, sqlstore.RoleMember)
	idb := newKeyT(t)
	idc := newKeyT(t)

	// Normal valid enrollment
	req := request(t, link.Token, idb, "clinic-pi")
	body, _ := json.Marshal(req)
	resp, err := http.Post(env.srv.URL+PathEnroll, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var er EnrollResponse
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &er); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 1. Valid CheckEnrollment passes
	if err := CheckEnrollment(er, link, idb, "clinic-pi"); err != nil {
		t.Fatalf("CheckEnrollment valid: %v", err)
	}

	// 2. Checking with device C instead of device B
	if err := CheckEnrollment(er, link, idc, "clinic-pi"); !errors.Is(err, ErrWrongAdmin) {
		t.Errorf("CheckEnrollment for wrong device: err = %v, want ErrWrongAdmin", err)
	}

	// 3. Checking with wrong label
	if err := CheckEnrollment(er, link, idb, "wrong-label"); !errors.Is(err, ErrWrongAdmin) {
		t.Errorf("CheckEnrollment with wrong label: err = %v, want ErrWrongAdmin", err)
	}

	// 4. Bit-flipped signature in EnrollResponse
	tampered := er
	tampered.Receipt = make([]byte, len(er.Receipt))
	copy(tampered.Receipt, er.Receipt)
	tampered.Receipt[0] ^= 0xff
	if err := CheckEnrollment(tampered, link, idb, "clinic-pi"); !errors.Is(err, ErrWrongAdmin) {
		t.Errorf("CheckEnrollment with bit-flipped signature: err = %v, want ErrWrongAdmin", err)
	}
}
