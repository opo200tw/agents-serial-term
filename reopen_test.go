// Regression tests for #1409: in headless mode a lost USB-UART must not
// leave serial-term alive with a dead fd and a silently stopped text log.
// The port is a fake injected through openPort, so no serial device or pty
// is touched.
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakePort stands in for a serial.Port. The embedded nil interface supplies
// the methods runHeadless never calls; calling one panics, which is what a
// test wants to hear about.
type fakePort struct {
	serial.Port
	rx        chan []byte
	readErr   chan error
	closed    chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	writeErr error
	written  bytes.Buffer
}

func newFakePort() *fakePort {
	return &fakePort{
		rx:      make(chan []byte, 4),
		readErr: make(chan error, 1),
		closed:  make(chan struct{}),
	}
}

// Read blocks like a real port with a read timeout: it returns data, an
// injected error, "closed" after Close, or (0, nil) on a short timeout.
// Returning (0, nil) at once would make readerLoop spin.
func (f *fakePort) Read(b []byte) (int, error) {
	select {
	case <-f.closed:
		return 0, errors.New("Port has been closed")
	case err := <-f.readErr:
		return 0, err
	case d := <-f.rx:
		return copy(b, d), nil
	case <-time.After(20 * time.Millisecond):
		return 0, nil
	}
}

func (f *fakePort) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.written.Write(b)
}

func (f *fakePort) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakePort) SetReadTimeout(time.Duration) error { return nil }

func (f *fakePort) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func (f *fakePort) writtenString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

// lockedBuffer is a goroutine-safe stdout stand-in.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// headlessRig wires a state with a real text log file, a handler on the
// first fake port, and runs runHeadless in a goroutine.
type headlessRig struct {
	st      *state
	h       *stdinHandler
	stop    chan struct{}
	logPath string
	stdout  *lockedBuffer
	result  chan int
}

func startHeadless(t *testing.T, p serial.Port, reopenTimeout time.Duration, open func(string, *serial.Mode) (serial.Port, error)) *headlessRig {
	t.Helper()
	oldOpen, oldInterval := openPort, reopenRetryInterval
	openPort = open
	reopenRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { openPort, reopenRetryInterval = oldOpen, oldInterval })

	logPath := filepath.Join(t.TempDir(), "text.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create text log: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	out := &lockedBuffer{}
	st := &state{
		mode:    modeASCII,
		textLog: f,
		sink:    lineSink{rxOut: out, txOut: out, sysOut: out},
	}
	stop := make(chan struct{})
	h := &stdinHandler{st: st, port: p, txEOL: []byte{'\n'}, stop: stop, reopenOnLoss: true}
	r := &headlessRig{st: st, h: h, stop: stop, logPath: logPath, stdout: out, result: make(chan int, 1)}
	go func() {
		r.result <- runHeadless(p, "/dev/fake", &serial.Mode{BaudRate: 115200}, st, nil, h, stop, reopenTimeout)
	}()
	t.Cleanup(func() { safeClose(stop) })
	return r
}

func (r *headlessRig) log() string {
	data, _ := os.ReadFile(r.logPath)
	return string(data)
}

func (r *headlessRig) waitLog(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(r.log(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in text log:\n%s", substr, r.log())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *headlessRig) waitResult(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case code := <-r.result:
		return code
	case <-time.After(within):
		t.Fatalf("runHeadless did not return within %v; text log:\n%s", within, r.log())
		return -1
	}
}

// assertInOrder checks each needle appears in s after the previous one.
func assertInOrder(t *testing.T, s string, needles ...string) {
	t.Helper()
	rest := s
	for _, n := range needles {
		i := strings.Index(rest, n)
		if i < 0 {
			t.Fatalf("%q missing or out of order in:\n%s", n, s)
		}
		rest = rest[i+len(n):]
	}
}

// A read error ends the reader; the port is reopened, data flows again and
// the markers land in the text log between the old and new data.
func TestReopenAfterReadError(t *testing.T) {
	p1, p2 := newFakePort(), newFakePort()
	var opens int
	var mu sync.Mutex
	r := startHeadless(t, p1, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		return p2, nil
	})

	p1.rx <- []byte("before\n")
	r.waitLog(t, "< before")
	p1.readErr <- errors.New("read /dev/fake: device not configured")
	r.waitLog(t, "--- port reopened: /dev/fake ---")
	p2.rx <- []byte("after\n")
	r.waitLog(t, "< after")

	assertInOrder(t, r.log(),
		"< before",
		"--- port lost: read /dev/fake: device not configured, reopening /dev/fake ---",
		"--- port reopened: /dev/fake ---",
		"< after")
	// Markers carry the session-start timestamp format in the text log.
	for _, line := range strings.Split(strings.TrimSpace(r.log()), "\n") {
		if strings.Contains(line, "--- port ") {
			if _, err := time.Parse("2006-01-02 15:04:05.000", line[:23]); err != nil {
				t.Errorf("marker line without a full timestamp: %q", line)
			}
		}
	}
	if !strings.Contains(r.stdout.String(), "--- port reopened: /dev/fake ---") {
		t.Errorf("stdout missing reopen marker:\n%s", r.stdout.String())
	}
	if !p1.isClosed() {
		t.Error("old port was not closed")
	}
	if r.h.curPort() != serial.Port(p2) {
		t.Error("stdinHandler still uses the old port")
	}
	mu.Lock()
	if opens != 1 {
		t.Errorf("openPort called %d times, want 1", opens)
	}
	mu.Unlock()

	safeClose(r.stop)
	if code := r.waitResult(t, time.Second); code != 0 {
		t.Errorf("exit code after stop = %d, want 0", code)
	}
	if !p2.isClosed() {
		t.Error("current port not closed on stop")
	}
}

// When every reopen fails until the timeout, the loop writes the failure
// marker and returns exitPortLost (3).
func TestReopenGivesUp(t *testing.T) {
	p1 := newFakePort()
	r := startHeadless(t, p1, 200*time.Millisecond, func(string, *serial.Mode) (serial.Port, error) {
		return nil, errors.New("open /dev/fake: no such file or directory")
	})

	p1.readErr <- errors.New("read /dev/fake: device not configured")
	if code := r.waitResult(t, 2*time.Second); code != exitPortLost {
		t.Fatalf("exit code = %d, want %d", code, exitPortLost)
	}
	assertInOrder(t, r.log(),
		"--- port lost: read /dev/fake: device not configured, reopening /dev/fake ---",
		"--- port lost, reopen failed after ",
		": open /dev/fake: no such file or directory; exiting ---")
}

// --reopen-timeout 0 disables reopening: a loss exits 3 without any open.
func TestReopenDisabled(t *testing.T) {
	p1 := newFakePort()
	r := startHeadless(t, p1, 0, func(string, *serial.Mode) (serial.Port, error) {
		t.Error("openPort called with reopening disabled")
		return nil, errors.New("unexpected")
	})
	p1.readErr <- errors.New("EOF-ish")
	if code := r.waitResult(t, time.Second); code != exitPortLost {
		t.Fatalf("exit code = %d, want %d", code, exitPortLost)
	}
	if !strings.Contains(r.log(), "reopen disabled") {
		t.Errorf("failure marker missing:\n%s", r.log())
	}
}

// A tx write failing with ENXIO goes through the same reopen path, and the
// marker names the tx error rather than the "port closed" the reader sees.
func TestTxENXIOTriggersReopen(t *testing.T) {
	p1, p2 := newFakePort(), newFakePort()
	p1.writeErr = syscall.ENXIO
	r := startHeadless(t, p1, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		return p2, nil
	})

	r.h.handleLine("hello")
	r.waitLog(t, "--- port reopened: /dev/fake ---")
	assertInOrder(t, r.log(),
		"[tx] "+syscall.ENXIO.Error(),
		"--- port lost: "+syscall.ENXIO.Error()+", reopening /dev/fake ---",
		"--- port reopened: /dev/fake ---")

	r.h.handleLine("again")
	if got := p2.writtenString(); got != "again\n" {
		t.Errorf("new port got %q, want %q", got, "again\n")
	}
}

func TestIsPortGone(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{syscall.ENXIO, true},
		{syscall.EIO, true},
		{&os.PathError{Op: "write", Path: "/dev/x", Err: syscall.ENXIO}, true},
		{errors.New("write /dev/cu.x: device not configured"), true},
		{errors.New("read /dev/ttyUSB0: input/output error"), true},
		{syscall.EAGAIN, false},
		{errors.New("Port has been closed"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isPortGone(c.err); got != c.want {
			t.Errorf("isPortGone(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// SIGINT/SIGTERM and /quit close stop; a reopen wait must notice within 1 s
// even with a long --reopen-timeout.
func TestStopDuringReopenWait(t *testing.T) {
	p1 := newFakePort()
	r := startHeadless(t, p1, 30*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		return nil, errors.New("open /dev/fake: no such file or directory")
	})
	p1.readErr <- errors.New("read /dev/fake: device not configured")
	r.waitLog(t, "reopening /dev/fake ---")
	time.Sleep(100 * time.Millisecond) // let a few attempts fail

	t0 := time.Now()
	safeClose(r.stop)
	if code := r.waitResult(t, time.Second); code != 0 {
		t.Errorf("exit code after stop = %d, want 0", code)
	}
	if took := time.Since(t0); took >= time.Second {
		t.Errorf("stop took %v during reopen wait", took)
	}
	if strings.Contains(r.log(), "reopen failed") {
		t.Errorf("stop was reported as a reopen failure:\n%s", r.log())
	}
}
