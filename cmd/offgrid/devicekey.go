package main

import (
	"errors"
	"fmt"
	"os"

	"offgrid/internal/identity"
	"offgrid/internal/store/sqlstore"
)

// deviceSigner loads this device's key so every op the command writes is
// signed (Day 36). The key is $OFFGRID_IDENTITY_KEY, else the config folder's
// device.key, the same place `offgrid identity init` makes it.
//
//   - no key file: (nil, nil). Ops are written unsigned, as before, and the sync
//     commands say so. Signing is opt-in until Day 37 makes it the default.
//   - a key file that is too open, corrupt or from a newer Offgrid: an error, and
//     the command does not run. Falling back to unsigned ops because the key is
//     damaged would turn signing off without anyone deciding to.
func deviceSigner() (sqlstore.Signer, error) {
	path := os.Getenv("OFFGRID_IDENTITY_KEY")
	if path == "" {
		p, err := identity.DefaultPath()
		if err != nil {
			return nil, nil // no config folder, so no key to find
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
	return nil, fmt.Errorf("device key %s: %w\nRefusing to run rather than write unsigned ops. Fix the file, or move it away to run without signing", path, err)
}

// withDeviceSigner adds the device key to opt unless the caller already set one.
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
