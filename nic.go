package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const intelVendor = 0x8086

// Intel 700-series device IDs handled by the i40e driver.
var models = map[uint16]string{
	0x1572: "X710 10GbE SFP+ (X710-DA2/DA4)",
	0x1574: "XL710 QEMU",
	0x1580: "XL710 40GbE backplane",
	0x1581: "X710 10GbE backplane",
	0x1583: "XL710 40GbE QSFP+ (XL710-QDA2)",
	0x1584: "XL710 40GbE QSFP+ (XL710-QDA1)",
	0x1585: "X710 10GbE QSFP+",
	0x1586: "X710 10GBASE-T",
	0x1587: "XL710 20GbE backplane",
	0x1588: "XL710 20GbE backplane",
	0x1589: "X710 10GBASE-T4",
	0x158a: "XXV710 25GbE backplane",
	0x158b: "XXV710 25GbE SFP28",
	0x104e: "X710 10GbE SFP+",
	0x104f: "X710 10GbE backplane",
	0x15ff: "X710 10GBASE-T",
	0x37ce: "X722 backplane",
	0x37cf: "X722 QSFP+",
	0x37d0: "X722 SFP+",
	0x37d1: "X722 1GbE",
	0x37d2: "X722 10GBASE-T",
	0x37d3: "X722 SFP+",
}

type Port struct {
	Iface    string
	PCI      string // 0000:01:00.0
	MAC      string
	State    string
	Driver   string
	Vendor   uint16
	Device   uint16
	Firmware string
}

func (p Port) Model() string {
	if m, ok := models[p.Device]; ok {
		return m
	}
	return "Intel 700-series (unknown model)"
}

// Slot is the PCI address without the function number. All ports on one
// card share an NVM, so this is what identifies "the card".
func (p Port) Slot() string {
	if i := strings.LastIndexByte(p.PCI, '.'); i > 0 {
		return p.PCI[:i]
	}
	return p.PCI
}

type Card struct {
	Slot  string
	Ports []Port
}

func (c Card) Primary() Port { return c.Ports[0] }

func (c Card) IfaceList() string {
	names := make([]string, len(c.Ports))
	for i, p := range c.Ports {
		names[i] = p.Iface
	}
	return strings.Join(names, ", ")
}

func readSys(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readHex16(path string) uint16 {
	v, _ := strconv.ParseUint(readSys(path), 0, 16)
	return uint16(v)
}

// Discover finds network interfaces backed by the i40e driver (or any Intel
// 700-series device ID we know of) by walking /sys/class/net.
func Discover(sysfs string) ([]Card, error) {
	ifaces, err := os.ReadDir(filepath.Join(sysfs, "class/net"))
	if err != nil {
		return nil, fmt.Errorf("cannot list network interfaces (this only works on Linux): %w", err)
	}
	bySlot := map[string]*Card{}
	for _, e := range ifaces {
		dir := filepath.Join(sysfs, "class/net", e.Name())
		dev, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		if err != nil {
			continue // virtual interface
		}
		p := Port{
			Iface:  e.Name(),
			PCI:    filepath.Base(dev),
			MAC:    readSys(filepath.Join(dir, "address")),
			State:  readSys(filepath.Join(dir, "operstate")),
			Vendor: readHex16(filepath.Join(dev, "vendor")),
			Device: readHex16(filepath.Join(dev, "device")),
		}
		if drv, err := filepath.EvalSymlinks(filepath.Join(dev, "driver")); err == nil {
			p.Driver = filepath.Base(drv)
		}
		_, known := models[p.Device]
		if p.Driver != "i40e" && !(p.Vendor == intelVendor && known) {
			continue
		}
		c := bySlot[p.Slot()]
		if c == nil {
			c = &Card{Slot: p.Slot()}
			bySlot[p.Slot()] = c
		}
		c.Ports = append(c.Ports, p)
	}
	cards := make([]Card, 0, len(bySlot))
	for _, c := range bySlot {
		sort.Slice(c.Ports, func(i, j int) bool { return c.Ports[i].PCI < c.Ports[j].PCI })
		cards = append(cards, *c)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Slot < cards[j].Slot })
	return cards, nil
}

// FindPort matches an interface name, PCI address (with or without the
// 0000: domain) or card slot.
func FindPort(cards []Card, want string) (Port, bool) {
	for _, c := range cards {
		if want == c.Slot || "0000:"+want == c.Slot {
			return c.Primary(), true
		}
		for _, p := range c.Ports {
			if want == p.Iface || want == p.PCI || "0000:"+want == p.PCI {
				return p, true
			}
		}
	}
	return Port{}, false
}
