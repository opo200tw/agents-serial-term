package main

import (
	"bytes"
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
	for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), req, 0); errno != 0 {
			t.Skipf("pty ioctl %#x: %v", req, errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Skipf("ptsname: %v", errno)
	}
	return string(name[:bytes.IndexByte(name[:], 0)]), master
}
