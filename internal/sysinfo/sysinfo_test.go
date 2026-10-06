package sysinfo

import "testing"

func TestParseCPUModel(t *testing.T) {
	in := "processor\t: 0\nvendor_id\t: AuthenticAMD\n" +
		"model name\t: AMD Ryzen(TM) 5 PRO  5650U with Radeon(TM) Graphics\n" +
		"processor\t: 1\nmodel name\t: something else\n"
	want := "AMD Ryzen(TM) 5 PRO 5650U with Radeon(TM) Graphics"
	if got := ParseCPUModel(in); got != want {
		t.Errorf("got %q, want %q (first CPU only, whitespace collapsed)", got, want)
	}
	if got := ParseCPUModel("nothing useful"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseMemTotalKiB(t *testing.T) {
	in := "MemTotal:       15728640 kB\nMemFree:         1000 kB\n"
	if got := ParseMemTotalKiB(in); got != 15728640 {
		t.Errorf("got %d, want 15728640", got)
	}
	if got := ParseMemTotalKiB("garbage"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestParseOSName(t *testing.T) {
	in := "NAME=\"Zorin OS\"\nPRETTY_NAME=\"Zorin OS 18\"\nID=zorin\n"
	if got := ParseOSName(in); got != "Zorin OS 18" {
		t.Errorf("got %q, want %q", got, "Zorin OS 18")
	}
}

func TestDetectDoesNotPanic(t *testing.T) {
	if info := Detect(); info.Threads < 1 {
		t.Errorf("Threads = %d, want at least 1", info.Threads)
	}
}
