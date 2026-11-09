package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func idKey(t *testing.T) string {
	t.Setenv("OFFGRID_IDENTITY_KEY", "")
	return filepath.Join(t.TempDir(), "cfg", "device.key")
}

// Reads never create (the Day 31 rule): `identity show` on a missing key is exit 3 and leaves nothing.
func TestIdentityShowNeverCreatesTheKey(t *testing.T) {
	key := idKey(t)
	code, out, errOut := runCLI(t, "", "identity", "show", "-key", key)
	if code != exitNoDatabase {
		t.Fatalf("exit %d, want %d (stderr %q)", code, exitNoDatabase, errOut)
	}
	if !strings.Contains(errOut, "no identity key") || !strings.Contains(errOut, key) || !strings.Contains(errOut, "offgrid identity init") {
		t.Errorf("stderr should say there is no key, name the file and say how to make one: %q", errOut)
	}
	if out != "" {
		t.Errorf("stdout %q", out)
	}
	if _, err := os.Stat(filepath.Dir(key)); err == nil {
		t.Error("show created the folder")
	}
}

func TestIdentityInitThenShowAcrossRuns(t *testing.T) {
	key := idKey(t)
	code, out, errOut := runCLI(t, "", "identity", "init", "-key", key)
	if code != 0 || !strings.Contains(out, "created") || !strings.Contains(out, "dev_") {
		t.Fatalf("init: exit %d\n%s\n%s", code, out, errOut)
	}
	var first string
	for i := 0; i < 3; i++ { // each call is a fresh "start" of the program
		code, out, errOut := runCLI(t, "", "identity", "show", "-key", key)
		if code != 0 {
			t.Fatalf("show %d: exit %d\n%s", i, code, errOut)
		}
		for _, want := range []string{"device ID:", "dev_", "algorithm:   ecdsa-p256", "public key:  ecdsa-p256:", key, "private key: never shown"} {
			if !strings.Contains(out, want) {
				t.Errorf("show output lacks %q:\n%s", want, out)
			}
		}
		if i == 0 {
			first = out
		} else if out != first {
			t.Errorf("show changed between runs:\n%s\nvs\n%s", first, out)
		}
	}
	// init again: nothing changes, and it says so.
	before, _ := os.ReadFile(key)
	code, out, _ = runCLI(t, "", "identity", "init", "-key", key)
	after, _ := os.ReadFile(key)
	if code != 0 || !strings.Contains(out, "already exists") || string(before) != string(after) {
		t.Errorf("second init: exit %d, changed=%v\n%s", code, string(before) != string(after), out)
	}
}

func TestIdentityShowJSON(t *testing.T) {
	key := idKey(t)
	runCLI(t, "", "identity", "init", "-key", key)
	code, out, errOut := runCLI(t, "", "identity", "show", "-key", key, "-json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var v struct {
		ID, Algorithm, PublicKey, File string
		Mode                           string
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(out), &raw)
	for _, k := range []string{"id", "algorithm", "public_key", "file", "mode"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("JSON lacks %q: %s", k, out)
		}
	}
	if !strings.HasPrefix(raw["id"].(string), "dev_") || raw["algorithm"] != "ecdsa-p256" {
		t.Errorf("bad JSON: %s", out)
	}
}

func TestIdentityWrongPermissionsRefuseEverywhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	key := idKey(t)
	runCLI(t, "", "identity", "init", "-key", key)
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(key)
	for _, args := range [][]string{{"identity", "show", "-key", key}, {"identity", "show", "-key", key, "-json"}, {"identity", "init", "-key", key}} {
		code, out, errOut := runCLI(t, "", args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		if !strings.Contains(errOut, "chmod 600") || !strings.Contains(errOut, key) {
			t.Errorf("%v: stderr should name the file and the fix: %q", args, errOut)
		}
		if out != "" {
			t.Errorf("%v: printed %q before refusing", args, out)
		}
	}
	if after, _ := os.ReadFile(key); string(after) != string(before) {
		t.Error("the file was changed")
	}
	// After the fix it works, with the same identity.
	os.Chmod(key, 0o600)
	if code, _, errOut := runCLI(t, "", "identity", "show", "-key", key); code != 0 {
		t.Fatalf("after chmod: exit %d %s", code, errOut)
	}
}

func TestIdentityCorruptKeyIsNotReplaced(t *testing.T) {
	key := idKey(t)
	os.MkdirAll(filepath.Dir(key), 0o700)
	os.WriteFile(key, []byte("SECRETSENTINEL garbage\n"), 0o600)
	for _, sub := range []string{"show", "init"} {
		code, out, errOut := runCLI(t, "", "identity", sub, "-key", key)
		if code != 1 || out != "" || !strings.Contains(errOut, "corrupt") {
			t.Errorf("%s: exit %d out %q err %q", sub, code, out, errOut)
		}
		if strings.Contains(errOut, "SECRETSENTINEL") {
			t.Errorf("%s: the error repeats the file: %q", sub, errOut)
		}
	}
	if b, _ := os.ReadFile(key); string(b) != "SECRETSENTINEL garbage\n" {
		t.Error("a corrupt key was replaced")
	}
}

func TestIdentityKeyPathFromEnvironmentAndFlagWins(t *testing.T) {
	envKey := idKey(t)
	t.Setenv("OFFGRID_IDENTITY_KEY", envKey)
	if code, _, e := runCLI(t, "", "identity", "init"); code != 0 {
		t.Fatalf("init via env: %d %s", code, e)
	}
	if _, err := os.Stat(envKey); err != nil {
		t.Fatalf("the key is not at $OFFGRID_IDENTITY_KEY: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other.key")
	code, _, _ := runCLI(t, "", "identity", "show", "-key", other)
	if code != exitNoDatabase {
		t.Errorf("-key should win over the environment: exit %d", code)
	}
}

func TestIdentityUsageErrors(t *testing.T) {
	key := idKey(t)
	for _, args := range [][]string{{"identity"}, {"identity", "frobnicate"}, {"identity", "show", "extra"}, {"identity", "init", "-nope"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _, errOut := runCLI(t, "", "identity", "help"); code != 0 || !strings.Contains(errOut+"", "identity init") {
		t.Errorf("help: exit %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Dir(key)); err == nil {
		t.Error("usage errors created files")
	}
}

// No key bytes in anything the command prints, on success or on any failure path.
func TestIdentityCommandNeverPrintsTheKey(t *testing.T) {
	key := idKey(t)
	var all strings.Builder
	run := func(args ...string) {
		_, o, e := runCLI(t, "", args...)
		all.WriteString(o + e)
	}
	run("identity", "show", "-key", key)
	run("identity", "init", "-key", key)
	file, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	enc := strings.Fields(string(file))[3]
	der, _ := base64.RawURLEncoding.DecodeString(enc)
	needles := []string{enc, base64.StdEncoding.EncodeToString(der), base64.RawStdEncoding.EncodeToString(der), string(file)}
	run("identity", "show", "-key", key)
	run("identity", "show", "-key", key, "-json")
	run("identity", "init", "-key", key)
	if runtime.GOOS != "windows" {
		os.Chmod(key, 0o666)
		run("identity", "show", "-key", key)
		os.Chmod(key, 0o600)
	}
	os.WriteFile(key, append([]byte("x"), file...), 0o600)
	run("identity", "show", "-key", key)
	for _, n := range needles {
		if len(n) < 20 {
			t.Fatal("needle too short")
		}
		if strings.Contains(all.String(), n) {
			t.Errorf("key material appears in the command's output:\n%s", all.String())
		}
	}
	if !strings.Contains(all.String(), "dev_") {
		t.Error("expected the device ID in the output")
	}
}
