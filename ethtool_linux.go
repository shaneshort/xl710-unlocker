//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	siocEthtool     = 0x8946
	ethtoolGDrvInfo = 0x03
	ethtoolGEEPROM  = 0x0b
	ethtoolSEEPROM  = 0x0c

	// NVM update "magic" transaction flags understood by i40e when passed
	// through ETHTOOL_GEEPROM / ETHTOOL_SEEPROM (see i40e_nvm.c). Same values
	// and request layout as terpstra/xl710-unlocker's mytool.c / mypoke.c.
	nvmTransShift = 8
	nvmSA         = 0x3 // start + last command: single atomic transaction
	nvmCSUM       = 0x8

	chunkWords  = 0x800 // read 4KiB at a time
	busyTimeout = 15 * time.Second
)

type ifreq struct {
	name [syscall.IFNAMSIZ]byte
	data unsafe.Pointer
	_    [24 - unsafe.Sizeof(uintptr(0))]byte
}

type ethtoolNVM struct {
	iface string
	devid uint16
	fd    int
}

func OpenNVM(p Port) (NVM, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	return &ethtoolNVM{iface: p.Iface, devid: p.Device, fd: fd}, nil
}

func (e *ethtoolNVM) Close() error { return syscall.Close(e.fd) }

func (e *ethtoolNVM) ioctl(buf []byte) error {
	var ifr ifreq
	copy(ifr.name[:len(ifr.name)-1], e.iface)
	ifr.data = unsafe.Pointer(&buf[0])
	// After a write or checksum update i40e sits in a wait state until the
	// firmware's completion event arrives, and rejects further NVM requests
	// with EBUSY (they are not executed), so retry for a while.
	deadline := time.Now().Add(busyTimeout)
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(e.fd), siocEthtool, uintptr(unsafe.Pointer(&ifr)))
		switch {
		case errno == 0:
			return nil
		case (errno == syscall.EBUSY || errno == syscall.EAGAIN) && time.Now().Before(deadline):
			time.Sleep(100 * time.Millisecond)
		default:
			return explain(errno)
		}
	}
}

func explain(errno syscall.Errno) error {
	switch errno {
	case syscall.EPERM, syscall.EACCES:
		return fmt.Errorf("%w (run as root)", errno)
	case syscall.EBUSY, syscall.EAGAIN:
		return fmt.Errorf("%w (NVM is busy, try again in a few seconds)", errno)
	case syscall.EOPNOTSUPP:
		return fmt.Errorf("%w (driver doesn't support NVM access on this interface)", errno)
	case syscall.EFAULT, syscall.EINVAL:
		return fmt.Errorf("%w (driver rejected the request; is this really an i40e port?)", errno)
	}
	return errno
}

// eeprom builds a struct ethtool_eeprom followed by its data.
func (e *ethtoolNVM) eeprom(cmd, trans uint32, byteOff, byteLen int) []byte {
	buf := make([]byte, 16+byteLen)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], cmd)
	le.PutUint32(buf[4:], uint32(e.devid)<<16|trans<<nvmTransShift)
	le.PutUint32(buf[8:], uint32(byteOff))
	le.PutUint32(buf[12:], uint32(byteLen))
	return buf
}

func (e *ethtoolNVM) ReadWords(start, count int) ([]uint16, error) {
	words := make([]uint16, 0, count)
	for done := 0; done < count; {
		n := min(chunkWords, count-done)
		buf := e.eeprom(ethtoolGEEPROM, nvmSA, 2*(start+done), 2*n)
		if err := e.ioctl(buf); err != nil {
			return nil, fmt.Errorf("reading NVM at word 0x%04x: %w", start+done, err)
		}
		for i := 0; i < n; i++ {
			words = append(words, binary.LittleEndian.Uint16(buf[16+2*i:]))
		}
		done += n
	}
	return words, nil
}

func (e *ethtoolNVM) WriteWord(addr int, v uint16) error {
	buf := e.eeprom(ethtoolSEEPROM, nvmSA, 2*addr, 2)
	binary.LittleEndian.PutUint16(buf[16:], v)
	if err := e.ioctl(buf); err != nil {
		return fmt.Errorf("writing NVM word 0x%04x: %w", addr, err)
	}
	return nil
}

func (e *ethtoolNVM) UpdateChecksum() error {
	if err := e.ioctl(e.eeprom(ethtoolSEEPROM, nvmCSUM|nvmSA, 0, 2)); err != nil {
		return fmt.Errorf("updating NVM checksum: %w", err)
	}
	return nil
}

// Firmware returns the version string ethtool -i would show.
func Firmware(iface string) string {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return ""
	}
	defer syscall.Close(fd)
	e := &ethtoolNVM{iface: iface, fd: fd}
	// struct ethtool_drvinfo: cmd, driver[32], version[32], fw_version[32], ...
	buf := make([]byte, 196)
	binary.LittleEndian.PutUint32(buf, ethtoolGDrvInfo)
	if e.ioctl(buf) != nil {
		return ""
	}
	return strings.TrimRight(string(buf[68:100]), "\x00")
}

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
