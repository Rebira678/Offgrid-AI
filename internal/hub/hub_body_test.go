package hub

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"offgrid/internal/syncer"
)

func TestHubMaxBodyExactBoundary(t *testing.T) {
	limit := int64(1024)
	e := newEnv(t, func(o *Options) { o.MaxBody = limit })
	a, _ := e.twoSites()
	hc := e.client(a, "x", nil).HTTP
	url := "https://" + e.addr + Prefix + "crew" + syncer.PathPush

	// Exactly limit bytes with declared Content-Length
	exactBody := strings.Repeat("a", int(limit))
	req, _ := http.NewRequest("POST", url, strings.NewReader(exactBody))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Should not be rejected with 413 Request Entity Too Large (it will be 400 bad request due to raw 'a' body)
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("body of exact limit size was rejected with 413")
	}

	// 1 byte over limit with declared Content-Length
	overBody := strings.Repeat("a", int(limit)+1)
	reqOver, _ := http.NewRequest("POST", url, strings.NewReader(overBody))
	respOver, err := hc.Do(reqOver)
	if err != nil {
		t.Fatal(err)
	}
	respOver.Body.Close()
	if respOver.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body of limit+1 was %d, want 413", respOver.StatusCode)
	}

	// 1 byte over limit chunked (Content-Length = -1)
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(overBody))
		pw.Close()
	}()
	reqChunked, _ := http.NewRequest("POST", url, pr)
	reqChunked.ContentLength = -1
	respChunked, err := hc.Do(reqChunked)
	if err != nil {
		t.Fatal(err)
	}
	respChunked.Body.Close()
	if respChunked.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked body of limit+1 was %d, want 413", respChunked.StatusCode)
	}
}
