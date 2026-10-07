package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// memNVM is a writable in-memory NVM that records what was done to it.
type memNVM struct {
	words      []uint16
	port       Port
	writes     map[int]uint16
	checksums  int
	dropWrites bool
	failWrite  int // fail the Nth write (1-based); 0 = never
	failCsum   bool
	failReads  int // fail this many reads after the checksum update
}

func (m *memNVM) ReadWords(start, count int) ([]uint16, error) {
	if m.checksums > 0 && m.failReads > 0 {
		m.failReads--
		return nil, errors.New("invalid argument")
	}
	return append([]uint16(nil), m.words[start:start+count]...), nil
}
func (m *memNVM) WriteWord(addr int, v uint16) error {
	if m.failWrite == len(m.writes)+1 {
		return errors.New("injected write failure")
	}
	m.writes[addr] = v
	if !m.dropWrites {
		m.words[addr] = v
	}
	return nil
}
func (m *memNVM) UpdateChecksum() error {
	if m.failCsum {
		return errors.New("injected checksum failure")
	}
	m.checksums++
	return nil
}
func (m *memNVM) Close() error { return nil }

// setup points the CLI at a fake /sys with two cards and a fake NVM, and
// runs the test from an empty directory so backups can be checked.
func setup(t *testing.T) *memNVM {
	m := &memNVM{words: image(0x6940, recFW8, 4), writes: map[int]uint16{}}
	sys := fakeSysfs(t)
	oldSys, oldOpen, oldRoot, oldDelay, oldTerm, oldVD := sysfsRoot, openNVM, isRoot, writeDelay, stdinIsTerminal, verifyDelay
	sysfsRoot, writeDelay, verifyDelay = sys, 0, 0
	oldDebugfs := debugfsRoot
	debugfsRoot = filepath.Join(sys, "kernel/debug")
	for _, pci := range []string{"0000:01:00.0", "0000:01:00.1", "0000:02:00.0"} {
		dir := filepath.Join(debugfsRoot, "i40e", pci)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "command"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stdinIsTerminal = func() bool { return false }
	isRoot = func() bool { return true }
	openNVM = func(p Port) (NVM, error) { m.port = p; return m, nil }
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sysfsRoot, openNVM, isRoot, writeDelay, stdinIsTerminal, verifyDelay = oldSys, oldOpen, oldRoot, oldDelay, oldTerm, oldVD
		debugfsRoot = oldDebugfs
		if err := os.Chdir(wd); err != nil {
			t.Error(err)
		}
	})
	return m
}

func backups(t *testing.T) []string {
	b, _ := filepath.Glob("xl710-nvm-*.bin")
	return b
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args    []string
		cmd     string
		pos     []string
		yes, dr bool
		image   string
		base    int
	}{
		{nil, "list", nil, false, false, "", 0},
		{[]string{"unlock", "eth4", "-y", "-n"}, "unlock", []string{"eth4"}, true, true, "", 0},
		{[]string{"unlock", "--dry-run", "01:00.0"}, "unlock", []string{"01:00.0"}, false, true, "", 0},
		{[]string{"-n", "unlock"}, "unlock", nil, false, true, "", 0},
		{[]string{"--image", "x.bin", "status"}, "status", nil, false, false, "x.bin", 0},
		{[]string{"dump", "enp1s0f0", "0x6940", "16"}, "dump", []string{"enp1s0f0", "0x6940", "16"}, false, false, "", 0},
		{[]string{"status", "--base", "0x6941"}, "status", nil, false, false, "", 0x6941},
		{[]string{"status", "--", "-weird-iface"}, "status", []string{"-weird-iface"}, false, false, "", 0},
	}
	for _, c := range cases {
		cmd, o, pos, err := parseArgs(c.args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if cmd != c.cmd || strings.Join(pos, " ") != strings.Join(c.pos, " ") || o.yes != c.yes || o.dryRun != c.dr || o.image != c.image || o.base.v != c.base {
			t.Errorf("%q: got cmd=%q pos=%q yes=%v dry=%v image=%q base=%x", c.args, cmd, pos, o.yes, o.dryRun, o.image, o.base.v)
		}
	}
	for _, bad := range [][]string{{"status", "--bogus"}, {"status", "--base", "zz"}, {"status", "--misc", "99"}} {
		if _, _, _, err := parseArgs(bad); !errors.As(err, new(usageError)) {
			t.Errorf("%q: want usage error, got %v", bad, err)
		}
	}
	if err := run([]string{"frobnicate"}); !errors.As(err, new(usageError)) {
		t.Errorf("unknown command: got %v", err)
	}
	if err := run([]string{"status", "eth0", "eth1"}); !errors.As(err, new(usageError)) {
		t.Errorf("extra args: got %v", err)
	}
}

func TestNICSelection(t *testing.T) {
	m := setup(t)

	// two cards, no NIC given, stdin is not a terminal under go test
	err := run([]string{"status"})
	if err == nil || !strings.Contains(err.Error(), "several cards") || !strings.Contains(err.Error(), "enp1s0f0, enp1s0f1") {
		t.Fatalf("want 'several cards' listing ports, got %v", err)
	}

	for arg, want := range map[string]string{"enp1s0f1": "enp1s0f1", "01:00.1": "enp1s0f1", "02:00.0": "enp2s0f0", "0000:01:00": "enp1s0f0"} {
		m.port = Port{}
		if err := run([]string{"status", arg}); err != nil {
			t.Fatalf("status %s: %v", arg, err)
		}
		if m.port.Iface != want {
			t.Errorf("status %s opened %q, want %q", arg, m.port.Iface, want)
		}
	}
	if err := run([]string{"status", "02:00.0"}); err != nil {
		t.Fatal(err)
	}
	if m.port.Device != 0x1583 {
		t.Errorf("device id %04x not taken from sysfs", m.port.Device)
	}

	err = run([]string{"status", "eno1"})
	if err == nil || !strings.Contains(err.Error(), "not an Intel 700-series port") || !strings.Contains(err.Error(), "0000:01:00") {
		t.Fatalf("igb port: %v", err)
	}

	isRoot = func() bool { return false }
	if err := run([]string{"status", "enp1s0f0"}); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("non-root: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	m := setup(t)
	if err := run([]string{"unlock", "enp1s0f0", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if len(m.writes) != 0 || m.checksums != 0 {
		t.Fatalf("dry run wrote %v, %d checksums", m.writes, m.checksums)
	}
	if b := backups(t); len(b) != 0 {
		t.Fatalf("dry run left backups %v", b)
	}
	// -y must not turn a dry run into a real one
	if err := run([]string{"unlock", "enp1s0f0", "-n", "-y"}); err != nil || len(m.writes) != 0 {
		t.Fatalf("-n -y wrote %v (err %v)", m.writes, err)
	}
}

func TestDryRunImage(t *testing.T) {
	setup(t)
	img := filepath.Join(t.TempDir(), "nvm.bin")
	if err := saveBackup(img, image(0x6870, recFW5, 4)); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"unlock", "--image", img}); err == nil {
		t.Fatal("unlock --image without --dry-run should be refused")
	}
	if err := run([]string{"unlock", "--image", img, "-n"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"dump", "--image", img, "0x6870", "12"}); err != nil {
		t.Fatal(err)
	}
}

func TestUnlockThenLock(t *testing.T) {
	m := setup(t)
	orig := append([]uint16(nil), m.words...)

	if err := run([]string{"unlock", "enp1s0f1"}); err == nil || !strings.Contains(err.Error(), "-y") {
		t.Fatalf("unlock without -y on a non-terminal: %v", err)
	}
	if len(m.writes) != 0 {
		t.Fatal("wrote without confirmation")
	}

	if err := run([]string{"unlock", "enp1s0f1", "-y"}); err != nil {
		t.Fatal(err)
	}
	want := map[int]uint16{0x6948: 0x630c, 0x6956: 0x630c, 0x6964: 0x630c, 0x6972: 0x630c}
	if !reflect.DeepEqual(m.writes, want) || m.checksums != 1 {
		t.Fatalf("writes %x checksums %d", m.writes, m.checksums)
	}
	for i := range orig {
		if _, changed := want[i]; !changed && m.words[i] != orig[i] {
			t.Fatalf("word 0x%04x changed", i)
		}
	}
	b := backups(t)
	if len(b) != 1 || !strings.Contains(b[0], "0000_01_00.1") {
		t.Fatalf("backups %v", b)
	}
	img, _ := openImage(b[0])
	if !reflect.DeepEqual(img.words, orig) {
		t.Fatal("backup doesn't match pre-unlock NVM")
	}

	// second unlock is a no-op
	m.writes = map[int]uint16{}
	if err := run([]string{"unlock", "enp1s0f1", "-y", "--no-backup"}); err != nil || len(m.writes) != 0 || m.checksums != 1 {
		t.Fatalf("re-unlock: err %v writes %x", err, m.writes)
	}

	if err := run([]string{"lock", "enp1s0f1", "-y", "--no-backup"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.words, orig) {
		t.Fatal("lock didn't restore the original NVM")
	}
}

func TestVerifyFailure(t *testing.T) {
	m := setup(t)
	m.dropWrites = true
	err := run([]string{"unlock", "enp1s0f0", "-y", "--no-backup"})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("got %v", err)
	}
}

func TestDumpArgs(t *testing.T) {
	m := setup(t)
	if err := run([]string{"dump", "enp2s0f0", "0x6940", "4"}); err != nil {
		t.Fatal(err)
	}
	if m.port.Iface != "enp2s0f0" {
		t.Fatalf("dump opened %q", m.port.Iface)
	}
}

func TestPartialFailureRecovery(t *testing.T) {
	m := setup(t)
	m.failWrite = 3
	err := run([]string{"unlock", "enp1s0f0", "-y", "--no-backup"})
	if err == nil || !strings.Contains(err.Error(), "2 of 4 words") || !strings.Contains(err.Error(), "unlock enp1s0f0") {
		t.Fatalf("got %v", err)
	}
	if m.checksums != 0 {
		t.Fatal("checksum updated after a failed write")
	}
	// re-running finishes the remaining words and the checksum
	m.failWrite = 0
	if err := run([]string{"unlock", "enp1s0f0", "-y", "--no-backup"}); err != nil {
		t.Fatal(err)
	}
	if len(m.writes) != 4 || m.checksums != 1 {
		t.Fatalf("writes %x checksums %d", m.writes, m.checksums)
	}

	// checksum fails after all words are written: re-running unlock is a
	// no-op, so point the user at the checksum command
	m = setup(t)
	m.failCsum = true
	err = run([]string{"unlock", "enp1s0f0", "-y", "--no-backup"})
	if err == nil || !strings.Contains(err.Error(), "checksum enp1s0f0") {
		t.Fatalf("got %v", err)
	}
	m.failCsum = false
	if err := run([]string{"checksum", "enp1s0f0", "-n"}); err != nil || m.checksums != 0 {
		t.Fatalf("checksum -n: err %v, %d checksums", err, m.checksums)
	}
	if err := run([]string{"checksum", "enp1s0f0"}); err == nil {
		t.Fatal("checksum without -y on a non-terminal should be refused")
	}
	if err := run([]string{"checksum", "enp1s0f0", "-y"}); err != nil || m.checksums != 1 {
		t.Fatalf("checksum: err %v, %d checksums", err, m.checksums)
	}
}

func TestDryRunOnlyWhereMeaningful(t *testing.T) {
	setup(t)
	for _, args := range [][]string{{"backup", "enp1s0f0", "-n", "-y", "-o", "x.bin"}, {"status", "-n"}, {"dump", "-n"}} {
		if err := run(args); !errors.As(err, new(usageError)) {
			t.Errorf("%q: want usage error, got %v", args, err)
		}
	}
	if _, err := os.Stat("x.bin"); err == nil {
		t.Fatal("backup -n created a file")
	}
}

// fw 6.80 rejects reads with EINVAL for a moment after the checksum update.
func TestVerifyRetriesAfterChecksum(t *testing.T) {
	m := setup(t)
	m.failReads = 3
	if err := run([]string{"unlock", "enp1s0f0", "-y", "--no-backup"}); err != nil {
		t.Fatal(err)
	}
	if m.failReads != 0 {
		t.Fatal("verify didn't retry")
	}
}

func resetRequested(t *testing.T, pci string) bool {
	b, _ := os.ReadFile(filepath.Join(debugfsRoot, "i40e", pci, "command"))
	return string(b) == "globr"
}

func TestReset(t *testing.T) {
	setup(t)
	// -y also confirms the reset at the end of unlock
	if err := run([]string{"unlock", "enp1s0f1", "-y"}); err != nil {
		t.Fatal(err)
	}
	if !resetRequested(t, "0000:01:00.1") {
		t.Fatal("unlock -y didn't reset the card")
	}

	setup(t)
	if err := run([]string{"reset", "01:00.0"}); err != nil {
		t.Fatal(err)
	}
	if resetRequested(t, "0000:01:00.0") {
		t.Fatal("reset without confirmation on a non-terminal")
	}
	if err := run([]string{"reset", "01:00.0", "-y"}); err != nil || !resetRequested(t, "0000:01:00.0") {
		t.Fatalf("reset -y: %v", err)
	}
	if err := run([]string{"reset", "01:00.0", "-n"}); !errors.As(err, new(usageError)) {
		t.Fatalf("reset -n: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(debugfsRoot, "i40e")); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"reset", "02:00.0", "-y"}); err == nil || !strings.Contains(err.Error(), "debugfs") {
		t.Fatalf("missing debugfs: %v", err)
	}
}
