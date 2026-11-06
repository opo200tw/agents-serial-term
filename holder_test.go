//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func useHolderDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := holderDir
	holderDir = func() string { return dir }
	t.Cleanup(func() { holderDir = old })
}

func TestPublishHolderRoundTrip(t *testing.T) {
	useHolderDir(t)
	unpublish := publishHolder("/dev/cu.usbserial-X", "rel.log")
	h, ok := readHolder("/dev/cu.usbserial-X")
	if !ok || h.pid != os.Getpid() || !filepath.IsAbs(h.textLog) || filepath.Base(h.textLog) != "rel.log" {
		t.Fatalf("readHolder = %+v, %v", h, ok)
	}
	unpublish()
	if _, ok := readHolder("/dev/cu.usbserial-X"); ok {
		t.Fatal("record still there after unpublish")
	}
}

// A holder that gave up must not delete the record of the process that took
// the port over meanwhile.
func TestUnpublishKeepsOtherPidRecord(t *testing.T) {
	useHolderDir(t)
	unpublish := publishHolder("/dev/ttyUSB0", "/a.log")
	path := holderFile("/dev/ttyUSB0")
	other := fmt.Sprintf("pid=%d\ntext-log=/b.log\n", os.Getppid())
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	unpublish()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("other holder's record removed: %v", err)
	}
}

func TestReadHolderIgnoresDeadPid(t *testing.T) {
	useHolderDir(t)
	if err := os.WriteFile(holderFile("/dev/ttyUSB1"), []byte("pid=999999\ntext-log=/x.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, ok := readHolder("/dev/ttyUSB1"); ok {
		t.Fatalf("stale record accepted: %+v", h)
	}
	var out, errOut bytes.Buffer
	if rc := reportBusy("/dev/ttyUSB1", true, &out, &errOut, nil); rc != 2 || out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("rc=%d out=%q err=%q", rc, out.String(), errOut.String())
	}
}

func TestReportBusyNoFollowNamesLog(t *testing.T) {
	useHolderDir(t)
	defer publishHolder("/dev/ttyUSB2", "/var/log/uart.txt")()
	var out, errOut bytes.Buffer
	rc := reportBusy("/dev/ttyUSB2", false, &out, &errOut, nil)
	if rc != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "its log: /var/log/uart.txt") {
		t.Fatalf("rc=%d out=%q err=%q", rc, out.String(), errOut.String())
	}
}

func TestReportBusyNoTextLog(t *testing.T) {
	useHolderDir(t)
	defer publishHolder("/dev/ttyUSB3", "")()
	var out, errOut bytes.Buffer
	if rc := reportBusy("/dev/ttyUSB3", true, &out, &errOut, nil); rc != 2 || !strings.Contains(errOut.String(), "no --text-log") {
		t.Fatalf("rc=%d err=%q", rc, errOut.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestFollowLogTailThenAppendThenHolderExit(t *testing.T) {
	oldPoll := followPoll
	followPoll = 10 * time.Millisecond
	defer func() { followPoll = oldPoll }()

	path := filepath.Join(t.TempDir(), "uart.txt")
	var old strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&old, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(old.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	alive := true
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- followLog(path, 3, out, make(chan struct{}), func() bool { mu.Lock(); defer mu.Unlock(); return alive })
	}()
	time.Sleep(50 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	fmt.Fprint(f, "new 1\n")
	time.Sleep(50 * time.Millisecond)
	fmt.Fprint(f, "last before exit\n")
	f.Close()
	mu.Lock()
	alive = false
	mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("followLog did not return after the holder exited")
	}
	want := "line 38\nline 39\nline 40\nnew 1\nlast before exit\n--- holder exited; port is free ---\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestFollowLogStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uart.txt")
	if err := os.WriteFile(path, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- followLog(path, 30, &syncBuf{}, stop, func() bool { return true }) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("followLog ignored stop")
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\n", 5, "a\nb\n"},
		{"", 3, ""},
	} {
		if got := string(lastLines([]byte(c.in), c.n)); got != c.want {
			t.Errorf("lastLines(%q,%d)=%q want %q", c.in, c.n, got, c.want)
		}
	}
}
