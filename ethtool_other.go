//go:build !linux

package main

import (
	"errors"
	"os"
)

func OpenNVM(Port) (NVM, error) {
	return nil, errors.New("NVM access needs Linux with the i40e driver (use --image to inspect a backup)")
}

func Firmware(string) string { return "" }

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
