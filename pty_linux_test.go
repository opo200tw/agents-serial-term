package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPty opens a pseudo-terminal pair and returns the slave path, used as
// the serial PORT so tests never touch a real device, and the master, which
// plays the device: bytes written to it are RX for serial-term, and TX from
// serial-term is read from it. The master stays open until the test ends.
func openPty(t *testing.T) (string, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock, ptn int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Skipf("unlockpt: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		t.Skipf("ptsname: %v", errno)
	}
	return fmt.Sprintf("/dev/pts/%d", ptn), master
}
