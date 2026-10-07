package main

import "fmt"

// The PHY capability table in the X710/XL710 shadow RAM is a run of
// records, one per PHY. Each record starts with a length word (number of
// words that follow), so records are laid out at a stride of length+1
// words. The "PHY Capabilities Misc0" word sits at a fixed offset inside
// the record (8 on every firmware seen so far), and bit 11 of it is the
// "only accept qualified modules" flag.
//
//   X710 fw 5.x:  0x6870: 000b 0022 0083 1871 0000 0000 3303 000b [2b0c] 0a00 0a1e 0003
//   X710 fw 6.x:  0x693f: 000c 0022 0083 1871 0000 0000 3303 000b [6b0c] 0a00 0a1e 0p03 0000  (p = port)
//   X710 fw 8.x+: 0x6940: 000d 0222 0083 1871 0000 0000 3303 000b [6b0c] 0a00 0a1e 0003 0000 0064
//   X722 fw 3.33: 0x65b6: 000c ffc2 000a 1871 0004 0005 3303 000b [270c] 0a00 0000 0006 e0be
//
// Records on one card can differ (port numbers, per-PHY settings), but in
// every known layout Misc0 is preceded two words earlier by 3303 and
// followed by 0a00. We anchor on that rather than on repetition alone, so a
// table entered mid-record (e.g. at the second 000b) is rejected.
//
// Older instructions had people grep a dump for 000b/000d by hand.

const (
	QualifyBit     = 0x0800 // bit 11 of Misc0: module qualification enforced
	DefaultMiscOff = 8
	DefaultWords   = 0x8000 // shadow RAM size in 16-bit words

	sigBefore = 0x3303 // word at Misc0-2
	sigAfter  = 0x0a00 // word at Misc0+1

	minRecordLen = DefaultMiscOff + 1 // record must contain the Misc0 word
	maxRecordLen = 0x20
	minRecords   = 2
	maxRecords   = 8
)

type PHYTable struct {
	Base    int // word address of the first record's length word
	Stride  int // words per record (length word + payload)
	Count   int // number of records
	MiscOff int // word offset of Misc0 inside a record
}

func (t PHYTable) MiscAddrs() []int {
	addrs := make([]int, t.Count)
	for i := range addrs {
		addrs[i] = t.Base + i*t.Stride + t.MiscOff
	}
	return addrs
}

func (t PHYTable) End() int { return t.Base + t.Count*t.Stride }

func (t PHYTable) String() string {
	return fmt.Sprintf("%d records at 0x%04x, %d words apart, Misc0 at +%d", t.Count, t.Base, t.Stride, t.MiscOff)
}

// isRecord reports whether a PHY record with length word l starts at addr.
func isRecord(w []uint16, addr, l, miscOff int) bool {
	if addr < 0 || addr+l+1 > len(w) || int(w[addr]) != l || miscOff < 2 || miscOff+1 > l {
		return false
	}
	return w[addr+miscOff-2] == sigBefore && w[addr+miscOff+1] == sigAfter
}

// tableAt checks whether a plausible PHY table starts at addr.
func tableAt(w []uint16, addr, miscOff int) (PHYTable, bool) {
	if addr < 0 || addr >= len(w) {
		return PHYTable{}, false
	}
	l := int(w[addr])
	if l < minRecordLen || l > maxRecordLen || !isRecord(w, addr, l, miscOff) {
		return PHYTable{}, false
	}
	stride := l + 1
	count := 1
	for count < maxRecords && isRecord(w, addr+count*stride, l, miscOff) {
		count++
	}
	if count < minRecords {
		return PHYTable{}, false
	}
	return PHYTable{Base: addr, Stride: stride, Count: count, MiscOff: miscOff}, true
}

// FindPHYTables scans a shadow RAM image for candidate PHY tables.
func FindPHYTables(w []uint16, miscOff int) []PHYTable {
	var found []PHYTable
	for a := 0; a < len(w); a++ {
		if t, ok := tableAt(w, a, miscOff); ok {
			found = append(found, t)
			a = t.End() - 1
		}
	}
	return found
}

// LockState summarises the qualification bit across a table.
type LockState int

const (
	Unlocked LockState = iota
	Locked
	Mixed
)

func (s LockState) String() string {
	return [...]string{"UNLOCKED", "LOCKED", "PARTIALLY UNLOCKED"}[s]
}

func TableState(w []uint16, t PHYTable) LockState {
	set := 0
	for _, a := range t.MiscAddrs() {
		if w[a]&QualifyBit != 0 {
			set++
		}
	}
	switch set {
	case 0:
		return Unlocked
	case t.Count:
		return Locked
	}
	return Mixed
}
