// Regression test for #8: input the driver buffered before the open must be
// dropped right after every successful open, or a capture starts by logging
// stale bytes.
package main

import (
	"errors"
	"testing"

	"go.bug.st/serial"
)

// resetCountPort counts ResetInputBuffer calls. The embedded nil interface
// panics on any other method, which openAndFlush must not call.
type resetCountPort struct {
	serial.Port
	resets int
}

func (p *resetCountPort) ResetInputBuffer() error {
	p.resets++
	return nil
}

func TestOpenAndFlushResetsInputOnce(t *testing.T) {
	fp := &resetCountPort{}
	open := func(string, *serial.Mode) (serial.Port, error) { return fp, nil }
	p, err := openAndFlush(open, "/dev/fake", &serial.Mode{BaudRate: 115200})
	if err != nil {
		t.Fatalf("openAndFlush: %v", err)
	}
	if p != fp {
		t.Fatalf("openAndFlush returned %v, want the opened port", p)
	}
	if fp.resets != 1 {
		t.Errorf("ResetInputBuffer called %d times, want 1", fp.resets)
	}
}

func TestOpenAndFlushSkipsResetOnOpenError(t *testing.T) {
	fp := &resetCountPort{}
	wantErr := errors.New("no such device")
	open := func(string, *serial.Mode) (serial.Port, error) { return nil, wantErr }
	p, err := openAndFlush(open, "/dev/fake", &serial.Mode{BaudRate: 115200})
	if !errors.Is(err, wantErr) {
		t.Fatalf("openAndFlush err = %v, want %v", err, wantErr)
	}
	if p != nil {
		t.Errorf("openAndFlush returned port %v on error, want nil", p)
	}
	if fp.resets != 0 {
		t.Errorf("ResetInputBuffer called %d times on open error, want 0", fp.resets)
	}
}
