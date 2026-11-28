package phone

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// LoadOrCreateSecret reads the session signing key, or makes one (32 random bytes, mode 0600) when the
// file does not exist. A key that other users can read, or that is too short, is refused: anyone holding
// it can make a session for any enrolled phone.
func LoadOrCreateSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newSecret(path, false)
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s can be read by other users (mode %o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
		}
	}
	if len(b) < MinSecret {
		return nil, fmt.Errorf("%s holds %d bytes; a session key needs at least %d. Delete it to make a new one (every phone then signs in again)", path, len(b), MinSecret)
	}
	return b, nil
}

// RotateSecret replaces the key. Every session made with the old one stops working at once; phones sign
// in again with their device keys, with no new code.
func RotateSecret(path string) error {
	_, err := newSecret(path, true)
	return err
}

func newSecret(path string, replace bool) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if !replace {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return b, err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp, path)
}

// Serve listens on addr until ctx ends. With a certificate it serves HTTPS (what a phone needs); with
// none it serves plain HTTP and ONLY on this machine, for trying the app in a desktop browser.
func Serve(ctx context.Context, addr string, c Config, cert *tls.Certificate, onListen func(net.Addr)) error {
	hd, err := Handler(c)
	if err != nil {
		return err
	}
	if cert == nil {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("bad address %q: %v", addr, err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("refusing to serve plain HTTP on %q: a phone needs HTTPS, so give -cert and -key (offgrid ca issue). Without them this only listens on this machine", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           hd,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute, // a photo over a weak WiFi link
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if cert != nil {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
