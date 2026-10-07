// xl710-unlock: let Intel X710/XL710 (i40e) NICs accept third-party SFP+ modules
// by clearing the module-qualification bit in the PHY capability table in NVM.
//
// The NVM access method and the bit to clear come from Wesley W. Terpstra's
// original C tools: https://github.com/terpstra/xl710-unlocker
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type NVM interface {
	ReadWords(start, count int) ([]uint16, error)
	WriteWord(addr int, v uint16) error
	UpdateChecksum() error
	Close() error
}

// imageNVM is a read-only NVM backed by a backup file.
type imageNVM struct{ words []uint16 }

func openImage(path string) (*imageNVM, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	w := make([]uint16, len(b)/2)
	for i := range w {
		w[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return &imageNVM{w}, nil
}

func (m *imageNVM) ReadWords(start, count int) ([]uint16, error) {
	if start+count > len(m.words) {
		count = len(m.words) - start
	}
	if start < 0 || count < 0 {
		return nil, fmt.Errorf("offset 0x%x is outside the image", start)
	}
	return m.words[start : start+count], nil
}
func (m *imageNVM) WriteWord(int, uint16) error { return errors.New("image files are read-only") }
func (m *imageNVM) UpdateChecksum() error       { return errors.New("image files are read-only") }
func (m *imageNVM) Close() error                { return nil }

const usage = `xl710-unlock - make Intel X710/XL710 NICs accept any SFP+ module

Usage:
  xl710-unlock [list]                 show Intel 700-series NICs on this machine
  xl710-unlock status [NIC]           find the PHY table and show lock state
  xl710-unlock unlock [NIC]           back up NVM, clear the qualification bit
  xl710-unlock lock   [NIC]           put the qualification bit back
  xl710-unlock checksum [NIC]         recompute the NVM checksum (recovery)
  xl710-unlock reset  [NIC]           global-reset the card so firmware reloads NVM
  xl710-unlock backup [NIC] [-o FILE] save the NVM shadow RAM to a file
  xl710-unlock dump   [NIC] [ADDR [COUNT]]  hex dump NVM words
  xl710-unlock version

NIC is an interface name (eth4, enp1s0f0) or PCI address (01:00.0). It can be
left out when there is only one card. All ports on a card share one NVM, so
unlocking any port unlocks the whole card.

Options:
  -n, --dry-run show what unlock/lock/checksum would do, change nothing
  -y            don't ask for confirmation
  --image FILE  read from a backup file instead of a NIC (status, dump,
                and unlock/lock with --dry-run)
  --base ADDR   use the PHY table at this word address instead of searching
  --misc N      word offset of Misc0 inside each record (default 8)
  --words N     how much NVM to read/scan, in words (default 0x8000)
  --no-backup   don't save a backup before writing

The firmware only reads the new setting when it restarts: unlock offers to
do that with a global reset of the card (the 'reset' command), otherwise
reboot. Reloading the i40e driver is NOT enough.
To return the card to factory NVM settings use Intel's nvmupdate64e -rd.
`

type opts struct {
	yes      bool
	dryRun   bool
	noBackup bool
	image    string
	out      string
	base     intFlag
	misc     int
	words    intFlag
}

// intFlag accepts hex (0x...) or decimal.
type intFlag struct {
	v   int
	set bool
}

func (f *intFlag) String() string { return fmt.Sprintf("0x%x", f.v) }
func (f *intFlag) Set(s string) error {
	v, err := strconv.ParseInt(s, 0, 32)
	f.v, f.set = int(v), true
	return err
}

var version = "dev" // set by the Makefile

// Swappable for tests.
var (
	sysfsRoot     = "/sys"
	openNVM       = OpenNVM
	isRoot        = func() bool { return os.Geteuid() == 0 }
	writeDelay    = time.Second
	verifyDelay   = 500 * time.Millisecond
	verifyTimeout = 15 * time.Second
	debugfsRoot   = "/sys/kernel/debug"

	stdinIsTerminal = func() bool { return isTerminal(os.Stdin) }
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintf(os.Stderr, "%v\n\n%s", err, usage)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type usageError struct{ error }

func parseArgs(args []string) (cmd string, o opts, pos []string, err error) {
	cmd = ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.dryRun, "n", false, "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")
	fs.BoolVar(&o.noBackup, "no-backup", false, "")
	fs.StringVar(&o.image, "image", "", "")
	fs.StringVar(&o.out, "o", "", "")
	fs.Var(&o.base, "base", "")
	fs.IntVar(&o.misc, "misc", DefaultMiscOff, "")
	o.words.v = DefaultWords
	fs.Var(&o.words, "words", "")

	// allow flags after positional arguments
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return "help", o, nil, nil
			}
			return cmd, o, nil, usageError{err}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if cmd == "" { // flags came before the command
		cmd = "list"
		if len(pos) > 0 {
			cmd, pos = pos[0], pos[1:]
		}
	}
	if o.misc < 0 || o.misc >= maxRecordLen {
		return cmd, o, nil, usageError{fmt.Errorf("--misc must be between 0 and %d", maxRecordLen-1)}
	}
	if o.words.v <= 0 || o.words.v > DefaultWords*16 {
		return cmd, o, nil, usageError{errors.New("--words out of range")}
	}
	return cmd, o, pos, nil
}

func run(args []string) error {
	cmd, o, pos, err := parseArgs(args)
	if err != nil {
		return err
	}
	maxPos := map[string]int{"list": 0, "ls": 0, "status": 1, "unlock": 1, "lock": 1, "checksum": 1, "reset": 1, "backup": 1, "dump": 3, "help": 99, "version": 0}
	if n, ok := maxPos[cmd]; ok && len(pos) > n {
		return usageError{fmt.Errorf("too many arguments for %s: %s", cmd, strings.Join(pos, " "))}
	}
	if o.dryRun && cmd != "unlock" && cmd != "lock" && cmd != "checksum" {
		return usageError{fmt.Errorf("--dry-run only applies to unlock, lock and checksum")}
	}
	switch cmd {
	case "list", "ls":
		return cmdList()
	case "status":
		return cmdStatus(o, pos)
	case "unlock":
		return cmdWrite(o, pos, false)
	case "lock":
		return cmdWrite(o, pos, true)
	case "checksum":
		return cmdChecksum(o, pos)
	case "reset":
		return cmdReset(o, pos)
	case "backup":
		return cmdBackup(o, pos)
	case "dump":
		return cmdDump(o, pos)
	case "help":
		fmt.Print(usage)
		return nil
	case "version":
		fmt.Println("xl710-unlock", version)
		return nil
	}
	return usageError{fmt.Errorf("unknown command %q", cmd)}
}

func cmdList() error {
	cards, err := Discover(sysfsRoot)
	if err != nil {
		return err
	}
	if len(cards) == 0 {
		return errNoCards
	}
	for _, c := range cards {
		p := c.Primary()
		fmt.Printf("%s  %s  [%04x:%04x]\n", c.Slot, p.Model(), p.Vendor, p.Device)
		if fw := Firmware(p.Iface); fw != "" {
			fmt.Printf("    firmware %s\n", fw)
		}
		for _, port := range c.Ports {
			fmt.Printf("    %-16s %s  %s  %s\n", port.Iface, port.PCI, port.MAC, port.State)
		}
	}
	return nil
}

var errNoCards = errors.New("no Intel 700-series (i40e) NICs found; is the i40e driver loaded? (lsmod | grep i40e)")

// pickPort resolves the NIC argument, or picks the only card present, or
// asks which one when there are several.
func pickPort(pos []string) (Port, error) {
	cards, err := Discover(sysfsRoot)
	if err != nil {
		return Port{}, err
	}
	if len(pos) > 0 {
		if p, ok := FindPort(cards, pos[0]); ok {
			return p, nil
		}
		if len(cards) == 0 {
			return Port{}, fmt.Errorf("%q is not an Intel 700-series port, and none were found", pos[0])
		}
		return Port{}, fmt.Errorf("%q is not an Intel 700-series port; found: %s", pos[0], describeCards(cards))
	}
	switch len(cards) {
	case 0:
		return Port{}, errNoCards
	case 1:
		return cards[0].Primary(), nil
	}
	if !stdinIsTerminal() {
		return Port{}, fmt.Errorf("several cards found, say which one: %s", describeCards(cards))
	}
	fmt.Println("Several cards found:")
	for i, c := range cards {
		fmt.Printf("  %d) %s  %s  (%s)\n", i+1, c.Slot, c.Primary().Model(), c.IfaceList())
	}
	n, _ := strconv.Atoi(prompt(fmt.Sprintf("Which one? [1-%d] ", len(cards))))
	if n < 1 || n > len(cards) {
		return Port{}, errors.New("no card selected")
	}
	return cards[n-1].Primary(), nil
}

func describeCards(cards []Card) string {
	var s []string
	for _, c := range cards {
		s = append(s, fmt.Sprintf("%s (%s)", c.Slot, c.IfaceList()))
	}
	return strings.Join(s, ", ")
}

func prompt(q string) string {
	fmt.Print(q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func requireRoot() error {
	if !isRoot() {
		return errors.New("NVM access needs root; re-run with sudo")
	}
	return nil
}

// open returns the NVM to work on and a short description of it.
func open(o opts, pos []string) (NVM, Port, error) {
	if o.image != "" {
		m, err := openImage(o.image)
		return m, Port{Iface: o.image}, err
	}
	p, err := pickPort(pos)
	if err != nil {
		return nil, p, err
	}
	p.Firmware = Firmware(p.Iface)
	fmt.Printf("%s  %s  %s  [%04x:%04x]", p.Iface, p.PCI, p.Model(), p.Vendor, p.Device)
	if p.Firmware != "" {
		fmt.Printf("  fw %s", p.Firmware)
	}
	fmt.Println()
	if err := requireRoot(); err != nil {
		return nil, p, err
	}
	m, err := openNVM(p)
	return m, p, err
}

func locate(w []uint16, o opts) (PHYTable, error) {
	if o.base.set {
		t, ok := tableAt(w, o.base.v, o.misc)
		if !ok {
			return t, fmt.Errorf("no PHY table at 0x%04x (expected a length word followed by repeated records)", o.base.v)
		}
		return t, nil
	}
	tables := FindPHYTables(w, o.misc)
	switch len(tables) {
	case 0:
		return PHYTable{}, errors.New("couldn't find the PHY capability table. Save a backup " +
			"('xl710-unlock backup') and open an issue, or find it by hand with 'dump' and pass --base")
	case 1:
		return tables[0], nil
	}
	var b strings.Builder
	b.WriteString("found more than one candidate PHY table:\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "  --base 0x%04x  %v, %v\n", t.Base, t, TableState(w, t))
		for _, a := range t.MiscAddrs() {
			fmt.Fprintf(&b, "      Misc0 0x%04x = 0x%04x\n", a, w[a])
		}
	}
	b.WriteString("pick the right one and pass it with --base")
	return PHYTable{}, errors.New(b.String())
}

func printTable(w []uint16, t PHYTable) {
	fmt.Printf("\nPHY capability table: %v\n", t)
	for i, a := range t.MiscAddrs() {
		state := "unlocked"
		if w[a]&QualifyBit != 0 {
			state = "locked (bit 11 set)"
		}
		fmt.Printf("  record %d  Misc0 @ 0x%04x = 0x%04x  %s\n", i, a, w[a], state)
	}
}

func cmdStatus(o opts, pos []string) error {
	m, p, err := open(o, pos)
	if err != nil {
		return err
	}
	defer m.Close()
	w, err := m.ReadWords(0, o.words.v)
	if err != nil {
		return err
	}
	t, err := locate(w, o)
	if err != nil {
		return err
	}
	printTable(w, t)
	s := TableState(w, t)
	fmt.Printf("\nStatus: %v\n", s)
	if s != Unlocked && o.image == "" {
		fmt.Printf("Run 'sudo %s unlock %s' to unlock.\n", progName(), p.Iface)
	}
	if s == Unlocked {
		fmt.Println("If modules are still rejected after a reboot: plain 1G SFP (non-plus) modules are\n" +
			"not supported by X710 at all, and some 10GBASE-T modules need extra power or settings.")
	}
	return nil
}

func backupPath(p Port) string {
	return fmt.Sprintf("xl710-nvm-%s-%s.bin", strings.ReplaceAll(p.PCI, ":", "_"), time.Now().Format("20060102-150405"))
}

func saveBackup(path string, w []uint16) error {
	b := make([]byte, 2*len(w))
	for i, v := range w {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return os.WriteFile(path, b, 0o644)
}

func cmdBackup(o opts, pos []string) error {
	m, p, err := open(o, pos)
	if err != nil {
		return err
	}
	defer m.Close()
	w, err := m.ReadWords(0, o.words.v)
	if err != nil {
		return err
	}
	out := o.out
	if out == "" {
		out = backupPath(p)
	}
	if err := saveBackup(out, w); err != nil {
		return err
	}
	fmt.Printf("Saved %d words to %s\n", len(w), out)
	return nil
}

func cmdDump(o opts, pos []string) error {
	addr, count := 0, 0x40
	// trailing numeric args are ADDR [COUNT]; anything before is the NIC
	var nums []int
	for len(pos) > 0 {
		v, err := strconv.ParseInt(pos[len(pos)-1], 0, 32)
		if err != nil || len(nums) == 2 {
			break
		}
		nums = append([]int{int(v)}, nums...)
		pos = pos[:len(pos)-1]
	}
	if len(nums) > 0 {
		addr = nums[0]
	}
	if len(nums) > 1 {
		count = nums[1]
	}
	m, _, err := open(o, pos)
	if err != nil {
		return err
	}
	defer m.Close()
	w, err := m.ReadWords(addr, count)
	if err != nil {
		return err
	}
	for i := 0; i < len(w); i += 8 {
		fmt.Printf("%04x:", addr+i)
		for j := i; j < i+8 && j < len(w); j++ {
			fmt.Printf(" %04x", w[j])
		}
		fmt.Println()
	}
	return nil
}

func cmdWrite(o opts, pos []string, lock bool) error {
	if o.image != "" && !o.dryRun {
		return errors.New("--image is read-only; add --dry-run to see what would change")
	}
	m, p, err := open(o, pos)
	if err != nil {
		return err
	}
	defer m.Close()
	w, err := m.ReadWords(0, o.words.v)
	if err != nil {
		return err
	}
	t, err := locate(w, o)
	if err != nil {
		return err
	}
	printTable(w, t)

	type change struct {
		addr     int
		old, new uint16
	}
	var changes []change
	for _, a := range t.MiscAddrs() {
		v := w[a] &^ QualifyBit
		if lock {
			v = w[a] | QualifyBit
		}
		if v != w[a] {
			changes = append(changes, change{a, w[a], v})
		}
	}
	if len(changes) == 0 {
		fmt.Printf("\nAlready %v, nothing to do.\n", TableState(w, t))
		fmt.Printf("(If an earlier run failed before updating the checksum, run '%s checksum %s'.)\n", progName(), p.Iface)
		return nil
	}

	verb := "unlock"
	if lock {
		verb = "lock"
	}
	target := p.Slot()
	if o.image != "" {
		target = o.image
	}
	fmt.Printf("\nWill %s %s by writing:\n", verb, target)
	for _, c := range changes {
		fmt.Printf("  0x%04x: 0x%04x -> 0x%04x\n", c.addr, c.old, c.new)
	}
	fmt.Println("and then updating the NVM checksum.")

	if o.dryRun {
		if !o.noBackup && o.image == "" {
			fmt.Println("Would save a backup to", backupPath(p))
		}
		fmt.Println("\nDry run: nothing written.")
		return nil
	}

	if !o.noBackup {
		path := backupPath(p)
		if err := saveBackup(path, w); err != nil {
			return fmt.Errorf("saving backup (use --no-backup to skip): %w", err)
		}
		fmt.Println("Backup saved to", path)
	}

	if !o.yes {
		if !stdinIsTerminal() {
			return errors.New("refusing to write without confirmation; pass -y")
		}
		if a := prompt("Write to NVM? [y/N] "); a != "y" && a != "Y" && a != "yes" {
			return errors.New("aborted, nothing written")
		}
	}

	// don't let ^C leave a half-written table with a stale checksum
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)

	for i, c := range changes {
		if err := m.WriteWord(c.addr, c.new); err != nil {
			if i == 0 {
				return fmt.Errorf("%w\nNothing was changed.", err)
			}
			return fmt.Errorf("%w\n%d of %d words were written but the checksum was not updated.\n"+
				"Re-run '%s %s %s', then reboot.", err, i, len(changes), progName(), verb, p.Iface)
		}
		time.Sleep(writeDelay)
	}
	if err := m.UpdateChecksum(); err != nil {
		return fmt.Errorf("%w\nThe words were written but the checksum was not updated.\n"+
			"Run '%s checksum %s' before rebooting.", err, progName(), p.Iface)
	}

	// Right after the checksum update the firmware is still committing to
	// flash and rejects reads for a moment (EINVAL on fw 6.80, not EBUSY).
	var after []uint16
	for deadline := time.Now().Add(verifyTimeout); ; {
		if after, err = m.ReadWords(t.Base, t.End()-t.Base); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("written and checksum updated, but couldn't read back to verify: %w\n"+
				"Check with '%s status %s'.", err, progName(), p.Iface)
		}
		time.Sleep(verifyDelay)
	}
	ok := true
	for _, c := range changes {
		if got := after[c.addr-t.Base]; got != c.new {
			fmt.Printf("  0x%04x reads back 0x%04x, expected 0x%04x\n", c.addr, got, c.new)
			ok = false
		}
	}
	if !ok {
		return errors.New("verification failed; the NVM may not have accepted the write")
	}
	fmt.Printf("\nDone, card is %sed in NVM.\n", verb)
	return offerReset(o, p)
}

// offerReset applies an NVM change without a reboot. Reloading i40e is not
// enough: the card's firmware reads the PHY settings when it initialises,
// and a global reset (GLOBR) makes it do that again.
func offerReset(o opts, p Port) error {
	fmt.Println("The card's firmware only picks this up when it restarts. That can be done now")
	fmt.Println("with a global reset of the card (all its ports drop for a few seconds).")
	if !o.yes {
		if !stdinIsTerminal() {
			fmt.Printf("Run '%s reset %s' or reboot to apply.\n", progName(), p.Iface)
			return nil
		}
		if a := prompt("Reset the card now? [y/N] "); a != "y" && a != "Y" && a != "yes" {
			fmt.Printf("Run '%s reset %s' or reboot to apply.\n", progName(), p.Iface)
			return nil
		}
	}
	return globalReset(p)
}

func globalReset(p Port) error {
	cmd := filepath.Join(debugfsRoot, "i40e", p.PCI, "command")
	if _, err := os.Stat(cmd); err != nil {
		return fmt.Errorf("can't reset the card: %s not found (is debugfs mounted? "+
			"'mount -t debugfs none /sys/kernel/debug'). Reboot instead", cmd)
	}
	if err := os.WriteFile(cmd, []byte("globr"), 0); err != nil {
		return fmt.Errorf("global reset via %s: %w. Reboot instead", cmd, err)
	}
	fmt.Println("Global reset requested; links will come back in a few seconds.")
	fmt.Println("Bring the ports up ('ip link set IFACE up') and check dmesg for 'unsupported SFP'.")
	return nil
}

func cmdReset(o opts, pos []string) error {
	if o.image != "" {
		return errors.New("--image can't be reset")
	}
	if o.dryRun {
		return usageError{errors.New("--dry-run doesn't apply to reset")}
	}
	p, err := pickPort(pos)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	fmt.Printf("%s  %s  %s  (%s)\n", p.Iface, p.PCI, p.Model(), p.Slot())
	return offerReset(o, p)
}

func progName() string { return filepath.Base(os.Args[0]) }

// cmdChecksum finishes a write that failed before the checksum update.
func cmdChecksum(o opts, pos []string) error {
	if o.image != "" {
		return errors.New("--image is read-only")
	}
	m, p, err := open(o, pos)
	if err != nil {
		return err
	}
	defer m.Close()
	if o.dryRun {
		fmt.Println("Would ask the firmware to recompute the NVM checksum.\n\nDry run: nothing written.")
		return nil
	}
	if !o.yes {
		if !stdinIsTerminal() {
			return errors.New("refusing to write without confirmation; pass -y")
		}
		if a := prompt("Recompute NVM checksum? [y/N] "); a != "y" && a != "Y" && a != "yes" {
			return errors.New("aborted, nothing written")
		}
	}
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	if err := m.UpdateChecksum(); err != nil {
		return err
	}
	fmt.Println("Checksum updated.")
	return offerReset(o, p)
}
