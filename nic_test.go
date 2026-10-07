package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds just enough of /sys for Discover.
func fakeSysfs(t *testing.T) string {
	root := t.TempDir()
	mk := func(path, data string) {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ln := func(target, link string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	mk("bus/pci/drivers/i40e/.keep", "")
	mk("bus/pci/drivers/igb/.keep", "")
	nic := func(iface, pci, vendor, device, driver string) {
		dev := "devices/pci0000:00/" + pci
		mk(dev+"/vendor", vendor)
		mk(dev+"/device", device)
		ln("bus/pci/drivers/"+driver, dev+"/driver")
		mk("class/net/"+iface+"/address", "aa:bb:cc:dd:ee:ff")
		mk("class/net/"+iface+"/operstate", "up")
		ln(dev, "class/net/"+iface+"/device")
	}
	nic("enp1s0f1", "0000:01:00.1", "0x8086", "0x1572", "i40e")
	nic("enp1s0f0", "0000:01:00.0", "0x8086", "0x1572", "i40e")
	nic("enp2s0f0", "0000:02:00.0", "0x8086", "0x1583", "i40e")
	nic("eno1", "0000:00:19.0", "0x8086", "0x1533", "igb")
	mk("class/net/lo/address", "00:00:00:00:00:00")
	return root
}

func TestDiscover(t *testing.T) {
	cards, err := Discover(fakeSysfs(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d cards, want 2: %+v", len(cards), cards)
	}
	c := cards[0]
	if c.Slot != "0000:01:00" || len(c.Ports) != 2 || c.Primary().Iface != "enp1s0f0" {
		t.Fatalf("card 0 = %+v", c)
	}
	if c.Primary().Device != 0x1572 || c.Primary().Model() != models[0x1572] {
		t.Fatalf("port = %+v", c.Primary())
	}

	for want, iface := range map[string]string{
		"enp1s0f1":     "enp1s0f1",
		"01:00.1":      "enp1s0f1",
		"0000:02:00.0": "enp2s0f0",
		"0000:01:00":   "enp1s0f0",
	} {
		p, ok := FindPort(cards, want)
		if !ok || p.Iface != iface {
			t.Errorf("FindPort(%q) = %q, %v; want %q", want, p.Iface, ok, iface)
		}
	}
	if _, ok := FindPort(cards, "eno1"); ok {
		t.Error("igb port matched")
	}
}
