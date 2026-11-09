package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"offgrid/internal/identity"
)

const identityHelp = `offgrid identity: this device's cryptographic identity (Day 34).

  offgrid identity init [-key FILE]
        Make this device's key (ECDSA P-256) if there is none. Never replaces an existing one.
  offgrid identity show [-key FILE] [-json]
        Print the device ID, algorithm and public key. Changes nothing; with no key it fails (exit 3).

The key file is one line, readable by its owner only (mode 600), and Offgrid refuses to use it otherwise.
It is one key per DEVICE, not per person. The device ID is "dev_" and 32 hex characters of the SHA-256 of
the public key, so it can never disagree with the key. Where: -key, else $OFFGRID_IDENTITY_KEY, else the
config folder (~/.config/offgrid/device.key on Linux). The private key is never printed or logged.

Back the file up somewhere private: if it is lost the device gets a new ID, and a lost phone's key is revoked.`

func cmdIdentity(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, identityHelp)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprintln(stderr, identityHelp)
		return 0
	case "init":
		return cmdIdentityInit(args[1:], stdout, stderr)
	case "show":
		return cmdIdentityShow(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "error: unknown identity command %q (try init or show)\n", args[0])
	return 2
}

func identityFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("identity "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", os.Getenv("OFFGRID_IDENTITY_KEY"), "the key file (default: $OFFGRID_IDENTITY_KEY, else the config folder)")
	return fs, key
}

func resolveKeyPath(flagValue string, stderr io.Writer) (string, bool) {
	if flagValue != "" {
		return flagValue, true
	}
	p, err := identity.DefaultPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return "", false
	}
	return p, true
}

// identityExit maps a package error to an exit code: 3 for "there is no key", 1 for anything else.
func identityExit(err error, stderr io.Writer) int {
	if errors.Is(err, identity.ErrNotFound) {
		fmt.Fprintf(stderr, "error: %v; make one with: offgrid identity init\n", err)
		return exitNoDatabase
	}
	fmt.Fprintln(stderr, "error:", err)
	return 1
}

func cmdIdentityInit(args []string, stdout, stderr io.Writer) int {
	fs, key := identityFlags("init", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	path, ok := resolveKeyPath(*key, stderr)
	if !ok {
		return 1
	}
	id, created, err := identity.LoadOrCreate(path)
	if err != nil {
		return identityExit(err, stderr)
	}
	if created {
		fmt.Fprintf(stdout, "created a new identity\n  device ID: %s\n  key file:  %s\n\nBack the key file up somewhere private. If it is lost this device gets a new ID.\n", id.ID(), path)
	} else {
		fmt.Fprintf(stdout, "an identity already exists at %s (not changed)\n  device ID: %s\n", path, id.ID())
	}
	return 0
}

func cmdIdentityShow(args []string, stdout, stderr io.Writer) int {
	fs, key := identityFlags("show", stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q (show takes only flags)\n", fs.Arg(0))
		return 2
	}
	path, ok := resolveKeyPath(*key, stderr)
	if !ok {
		return 1
	}
	id, err := identity.Load(path) // never creates
	if err != nil {
		return identityExit(err, stderr)
	}
	mode := "unknown"
	if fi, err := os.Stat(path); err == nil {
		mode = fmt.Sprintf("%04o", fi.Mode().Perm())
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]string{
			"id": id.ID(), "algorithm": id.Algorithm(), "public_key": id.PublicKey(), "file": path, "mode": mode,
		}, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "device ID:   %s\nalgorithm:   %s\npublic key:  %s\nkey file:    %s (mode %s)\nprivate key: never shown\n", id.ID(), id.Algorithm(), id.PublicKey(), path, mode)
	return 0
}
