package main

import "testing"

// Records taken from real dumps: Wesley Terpstra's original card
// (github.com/terpstra/xl710-unlocker) and terpstra/xl710-unlocker#9.
var (
	recFW5 = []uint16{0x000b, 0x0022, 0x0083, 0x1871, 0x0000, 0x0000, 0x3303, 0x000b, 0x2b0c, 0x0a00, 0x0a1e, 0x0003}
	recFW8 = []uint16{0x000d, 0x0222, 0x0083, 0x1871, 0x0000, 0x0000, 0x3303, 0x000b, 0x6b0c, 0x0a00, 0x0a1e, 0x0003, 0x0000, 0x0064}
)

// fw 6.80 X710-DA2: records differ by a port number at +11.
func imageFW6() []uint16 {
	w := image(0, nil, 0)
	for i := 0; i < 4; i++ {
		rec := []uint16{0x000c, 0x0022, 0x0083, 0x1871, 0x0000, 0x0000, 0x3303, 0x000b, 0x6b0c, 0x0a00, 0x0a1e, uint16(i)<<8 | 0x03, 0x0000}
		copy(w[0x693f+i*13:], rec)
	}
	copy(w[0x693f+4*13:], []uint16{0x0003, 0x1500, 0x0000, 0x0001})
	return w
}

func image(base int, rec []uint16, n int) []uint16 {
	w := make([]uint16, DefaultWords)
	// noise that contains the old grep targets, to make sure we don't trip on it
	for i := range w {
		w[i] = uint16(i*7919) ^ 0x5a5a
	}
	for i := 0x100; i < 0x140; i++ {
		w[i] = 0x000b
	}
	for i := 0; i < n; i++ {
		copy(w[base+i*len(rec):], rec)
	}
	return w
}

func TestFindPHYTables(t *testing.T) {
	cases := []struct {
		name string
		base int
		rec  []uint16
		want PHYTable
	}{
		{"fw5", 0x6870, recFW5, PHYTable{Base: 0x6870, Stride: 12, Count: 4, MiscOff: 8}},
		{"fw8", 0x6940, recFW8, PHYTable{Base: 0x6940, Stride: 14, Count: 4, MiscOff: 8}},
		{"fw9", 0x6941, recFW8, PHYTable{Base: 0x6941, Stride: 14, Count: 4, MiscOff: 8}},
		{"fw6.80", 0x693f, nil, PHYTable{Base: 0x693f, Stride: 13, Count: 4, MiscOff: 8}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := image(c.base, c.rec, 4)
			if c.rec == nil {
				w = imageFW6()
			}
			got := FindPHYTables(w, DefaultMiscOff)
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("got %+v, want [%+v]", got, c.want)
			}
			if s := TableState(w, got[0]); s != Locked {
				t.Fatalf("state %v, want LOCKED", s)
			}
			addrs := got[0].MiscAddrs()
			if addrs[0] != c.base+8 || addrs[3] != c.base+3*c.want.Stride+8 {
				t.Fatalf("misc addrs %x", addrs)
			}
		})
	}
}

func TestPartialUnlockStillFound(t *testing.T) {
	w := image(0x6940, recFW8, 4)
	w[0x6940+14+8] &^= QualifyBit
	got := FindPHYTables(w, DefaultMiscOff)
	if len(got) != 1 || got[0].Count != 4 {
		t.Fatalf("got %+v", got)
	}
	if s := TableState(w, got[0]); s != Mixed {
		t.Fatalf("state %v, want PARTIALLY UNLOCKED", s)
	}
	for _, a := range got[0].MiscAddrs() {
		w[a] &^= QualifyBit
	}
	if s := TableState(w, got[0]); s != Unlocked {
		t.Fatalf("state %v, want UNLOCKED", s)
	}
}

// Dell EDGE3400 onboard X722: records differ in several words, and Dell
// ships them with bit 11 already clear.
func TestX722(t *testing.T) {
	w := image(0, nil, 0)
	copy(w[0x65b6:], []uint16{
		0x000c, 0xffc2, 0x000a, 0x1871, 0x0004, 0x0005, 0x3303, 0x000b, 0x270c, 0x0a00, 0x0000, 0x0006, 0xe0be,
		0x000c, 0xffc2, 0x000a, 0x1871, 0x0004, 0x0005, 0x3303, 0x000b, 0x270c, 0x0a00, 0x0000, 0x0046, 0xe0be,
		0x000c, 0xff80, 0x000a, 0x1870, 0x0004, 0x0005, 0x3303, 0x0003, 0x070c, 0x0a00, 0x0000, 0x0402, 0xdebe,
		0x000c, 0xff80, 0x000a, 0x1870, 0x0004, 0x0005, 0x3303, 0x0003, 0x070c, 0x0a00, 0x0000, 0x0402, 0xdebe,
		0x016a, 0xf0fe, 0x0001})
	// the X722 image also has long runs of 16-word records that the old
	// repetition-only scanner reported as two dozen candidate tables
	for i := 0; i < 0x200; i += 16 {
		copy(w[0x4ff5+i:], []uint16{0x000f, 0, 0, 0, 0, 0x0003, 0, 0, 0x000f, 0, 0, 0, 0, 0x0003, 0, 0})
	}
	got := FindPHYTables(w, DefaultMiscOff)
	want := PHYTable{Base: 0x65b6, Stride: 13, Count: 4, MiscOff: 8}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
	if s := TableState(w, got[0]); s != Unlocked {
		t.Fatalf("state %v, want UNLOCKED", s)
	}
}

// Codex review: entering the fw5 table at its second 000b (+7) produced
// rotated "records" whose Misc0 was really +3 (0x1871) of the next record.
func TestRotatedBaseRejected(t *testing.T) {
	w := image(0x6870, recFW5, 4)
	for k := 1; k < len(recFW5); k++ {
		if tb, ok := tableAt(w, 0x6870+k, DefaultMiscOff); ok {
			t.Errorf("table accepted at +%d: %+v", k, tb)
		}
	}
}

func TestTableAtRejectsJunk(t *testing.T) {
	w := image(0x6940, recFW8, 4)
	if _, ok := tableAt(w, 0x100, DefaultMiscOff); ok {
		t.Fatal("run of identical words accepted as a table")
	}
	if _, ok := tableAt(w, 0x6941, DefaultMiscOff); ok {
		t.Fatal("misaligned base accepted")
	}
}
