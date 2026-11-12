package main

import (
	"errors"
	"fmt"
	"os"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// deviceSigner loads or provisions this device's key so every op the command
// writes is signed. The key is $OFFGRID_IDENTITY_KEY, else the config folder's
// device.key, the same place `offgrid identity init` makes it.
//
// Since Day 37 (Security Checkpoint 1), signing is the default:
//   - If no key exists, LoadOrCreate automatically creates a mode-0600 ECDSA P-256
//     device key on first write.
//   - If OFFGRID_ALLOW_UNSIGNED=1 is set, unsigned writes are permitted (for testing
//     legacy Day 30 migrations).
//   - A key file that is too open, corrupt, or from an incompatible version produces
//     an error and the command refuses to run.
func deviceSigner() (sqlstore.Signer, error) {
	if os.Getenv("OFFGRID_ALLOW_UNSIGNED") == "1" {
		return nil, nil
	}
	path := os.Getenv("OFFGRID_IDENTITY_KEY")
	if path == "" {
		p, err := identity.DefaultPath()
		if err != nil {
			return nil, nil // no config folder, so no key to find
		}
		id, _, err := identity.LoadOrCreate(p)
		if err == nil {
			return id, nil
		}
		return nil, fmt.Errorf("device key %s: %w\nRefusing to run rather than write unsigned ops. Fix the file, or move it away to run without signing", p, err)
	}
	id, err := identity.Load(path)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, identity.ErrNotFound):
		return nil, nil
	}
	return nil, fmt.Errorf("device key %s: %w\nRefusing to run rather than write unsigned ops. Fix the file, or move it away to run without signing", path, err)
}

// deviceSignerReadOnly only loads an existing key; it never creates one.
// Commands that only inspect existing databases use this to ensure
// read-only operations never write a key file to disk.
func deviceSignerReadOnly() (sqlstore.Signer, error) {
	if os.Getenv("OFFGRID_ALLOW_UNSIGNED") == "1" {
		return nil, nil
	}
	path := os.Getenv("OFFGRID_IDENTITY_KEY")
	if path == "" {
		p, err := identity.DefaultPath()
		if err != nil {
			return nil, nil
		}
		path = p
	}
	id, err := identity.Load(path)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, identity.ErrNotFound):
		return nil, nil
	}
	return nil, fmt.Errorf("device key %s: %w", path, err)
}

// withDeviceSigner adds the device key to opt unless the caller already set one.
// It uses deviceSigner(), which provisions a key if absent.
func withDeviceSigner(opt sqlstore.Options) (sqlstore.Options, error) {
	if opt.Signer != nil {
		return opt, nil
	}
	sg, err := deviceSigner()
	if err != nil {
		return opt, err
	}
	if sg != nil {
		opt.Signer = sg
	}
	return opt, nil
}

// withDeviceSignerReadOnly adds an existing device key to opt if present,
// but never creates one.
func withDeviceSignerReadOnly(opt sqlstore.Options) (sqlstore.Options, error) {
	if opt.Signer != nil {
		return opt, nil
	}
	sg, err := deviceSignerReadOnly()
	if err != nil {
		return opt, err
	}
	if sg != nil {
		opt.Signer = sg
	}
	return opt, nil
}
