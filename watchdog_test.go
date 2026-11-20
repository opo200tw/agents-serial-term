//go:build !windows

// Watchdog tests for UM-GPM7-camera #2802 on fake ports: a stalled reader,
// a reader that will not stop, a replaced or vanished PORT node, a quiet
// healthy port, and the heartbeat. The node is a fake stat, the port a
// fake; stall_e2e_test.go runs the built binary on ptys.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.bug.st/serial"
)

const testWatch = 300 * time.Millisecond

func fastWatchdog(t *testing.T) {
	t.Helper()
	oldTick, oldStop, oldLock := watchdogTick, readerStopWait, lockWait
	watchdogTick, readerStopWait, lockWait = 20*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { watchdogTick, readerStopWait, lockWait = oldTick, oldStop, oldLock })
}

// fakeNode stands in for stat(PORT).
type fakeNode struct {
	mu  sync.Mutex
	id  portID
	err error
}

func (n *fakeNode) set(id portID, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.id, n.err = id, err
}

func useFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	n := &fakeNode{id: portID{rdev: 1, ino: 1}}
	old := statPort
	statPort = func(string) (portID, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.id, n.err
	}
	t.Cleanup(func() { statPort = old })
	return n
}

// stallPort reads like fakePort until stall closes. After that Read never
// returns, until Close (unless ignoreClose) or release.
type stallPort struct {
	*fakePort
	stall       chan struct{}
	release     chan struct{}
	ignoreClose bool
}

func newStallPort(t *testing.T, ignoreClose bool) *stallPort {
	s := &stallPort{fakePort: newFakePort(), stall: make(chan struct{}), release: make(chan struct{}), ignoreClose: ignoreClose}
	t.Cleanup(func() { close(s.release) })
	return s
}

func (s *stallPort) Read(b []byte) (int, error) {
	select {
	case <-s.stall:
	default:
		return s.fakePort.Read(b)
	}
	closed := s.closed
	if s.ignoreClose {
		closed = nil
	}
	select {
	case <-closed:
		return 0, errors.New("Port has been closed")
	case <-s.release:
		return 0, errors.New("released")
	}
}

// A Read that stops returning is reported and handled as a lost port: the
// watchdog line, then the usual port lost / reopened pair, and data flows
// on the new port. Before #2802 nothing was written and the log stopped.
func TestWatchdogReaderStallIsPortLost(t *testing.T) {
	fastWatchdog(t)
	useFakeNode(t)
	p1, p2 := newStallPort(t, false), newFakePort()
	r := startHeadlessWatched(t, p1, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		return p2, nil
	}, testWatch, nil)

	p1.rx <- []byte("before\n")
	r.waitLog(t, "< before")
	close(p1.stall)
	r.waitLog(t, "--- port reopened: /dev/fake ---")
	p2.rx <- []byte("after\n")
	r.waitLog(t, "< after")
	assertInOrder(t, r.log(),
		"< before",
		"--- watchdog: reader stalled: no read returned for ",
		"; closing /dev/fake (#2802) ---",
		"--- port lost: reader stalled: no read returned for ",
		"--- port reopened: /dev/fake ---",
		"< after")
	if !p1.isClosed() {
		t.Error("stalled port was not closed")
	}
	safeClose(r.stop)
	if code := r.waitResult(t, time.Second); code != 0 {
		t.Errorf("exit code after stop = %d, want 0", code)
	}
}

// A reader that does not stop even after the close ends the run with
// exitStalled and a second marker, instead of hanging forever.
func TestWatchdogWedgedReaderExits(t *testing.T) {
	fastWatchdog(t)
	useFakeNode(t)
	p := newStallPort(t, true)
	close(p.stall)
	r := startHeadlessWatched(t, p, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		t.Error("reopen attempted for a reader that never stopped")
		return nil, errors.New("unexpected")
	}, testWatch, nil)
	if code := r.waitResult(t, 3*time.Second); code != exitStalled {
		t.Fatalf("exit code = %d, want %d", code, exitStalled)
	}
	assertInOrder(t, r.log(),
		"--- watchdog: reader stalled: no read returned for ",
		"--- watchdog: reader did not stop within 300ms of the close; exiting 4 ---")
}

// The marker still lands when the stuck reader holds st.mu (a blocked
// stdout write under the lock is one way to get there).
func TestWatchdogMarkerWithoutLock(t *testing.T) {
	fastWatchdog(t)
	useFakeNode(t)
	p := newStallPort(t, true)
	r := startHeadlessWatched(t, p, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		return nil, errors.New("unexpected")
	}, testWatch, nil)
	r.st.mu.Lock() // released at test end only
	t.Cleanup(r.st.mu.Unlock)
	close(p.stall)
	if code := r.waitResult(t, 3*time.Second); code != exitStalled {
		t.Fatalf("exit code = %d, want %d", code, exitStalled)
	}
	if !strings.Contains(r.log(), "--- watchdog: reader did not stop") {
		t.Fatalf("marker missing with st.mu held:\n%s", r.log())
	}
}

// A quiet port on the node that was opened is left alone: no marker, no
// close, no exit (USB storage mode and a board that is off are quiet).
func TestWatchdogQuietPortLeftAlone(t *testing.T) {
	fastWatchdog(t)
	useFakeNode(t)
	p := newFakePort()
	r := startHeadlessWatched(t, p, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		t.Error("reopen on a quiet healthy port")
		return nil, errors.New("unexpected")
	}, testWatch, nil)
	time.Sleep(5 * testWatch)
	if strings.Contains(r.log(), "watchdog") || strings.Contains(r.log(), "port lost") || p.isClosed() {
		t.Fatalf("quiet healthy port was acted on:\n%s", r.log())
	}
	safeClose(r.stop)
	if code := r.waitResult(t, time.Second); code != 0 {
		t.Errorf("exit code after stop = %d, want 0", code)
	}
}

// PORT replaced by another node while nothing arrives: the open fd is
// orphaned, so it is handled as a lost port and the new node is opened.
func TestWatchdogNodeReplacedWhileQuiet(t *testing.T) {
	fastWatchdog(t)
	node := useFakeNode(t)
	p1, p2 := newFakePort(), newFakePort()
	r := startHeadlessWatched(t, p1, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		return p2, nil
	}, testWatch, nil)
	p1.rx <- []byte("before\n")
	r.waitLog(t, "< before")
	node.set(portID{rdev: 2, ino: 2}, nil)
	r.waitLog(t, "--- port reopened: /dev/fake ---")
	p2.rx <- []byte("after\n")
	r.waitLog(t, "< after")
	assertInOrder(t, r.log(),
		"< before",
		"--- watchdog: port node replaced and no rx for ",
		"--- port lost: port node replaced and no rx for ",
		"--- port reopened: /dev/fake ---",
		"< after")
	// The new open took the new node as its own: no second loss.
	time.Sleep(3 * testWatch)
	if n := strings.Count(r.log(), "--- watchdog:"); n != 1 {
		t.Errorf("%d watchdog lines, want 1:\n%s", n, r.log())
	}
}

// A replaced node that still delivers data is left alone: the fd works.
func TestWatchdogNodeReplacedButRxFlows(t *testing.T) {
	fastWatchdog(t)
	node := useFakeNode(t)
	p := newFakePort()
	r := startHeadlessWatched(t, p, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		t.Error("reopen while rx flows")
		return nil, errors.New("unexpected")
	}, testWatch, nil)
	node.set(portID{rdev: 2, ino: 2}, nil)
	for end := time.Now().Add(4 * testWatch); time.Now().Before(end); {
		p.rx <- []byte("x\n")
		time.Sleep(testWatch / 6)
	}
	if strings.Contains(r.log(), "watchdog") || p.isClosed() {
		t.Fatalf("acted on a replaced node that still delivers rx:\n%s", r.log())
	}
}

// PORT gone with no rx: lost port; when it does not come back the run
// ends with the usual reopen failure (exit 3).
func TestWatchdogNodeGone(t *testing.T) {
	fastWatchdog(t)
	node := useFakeNode(t)
	p := newFakePort()
	r := startHeadlessWatched(t, p, 200*time.Millisecond, func(string, *serial.Mode) (serial.Port, error) {
		return nil, errors.New("open /dev/fake: no such file or directory")
	}, testWatch, nil)
	p.rx <- []byte("up\n")
	r.waitLog(t, "< up") // the open's stat has run
	node.set(portID{}, syscall.ENOENT)
	if code := r.waitResult(t, 3*time.Second); code != exitPortLost {
		t.Fatalf("exit code = %d, want %d", code, exitPortLost)
	}
	assertInOrder(t, r.log(),
		"--- watchdog: port node gone and no rx for ",
		"--- port lost: port node gone and no rx for ",
		"--- port lost, reopen failed after ")
}

// PORT that could not be stat'ed at open (no node to compare): no node
// check, so a quiet port is left alone.
func TestWatchdogNoNodeAtOpen(t *testing.T) {
	fastWatchdog(t)
	node := useFakeNode(t)
	node.set(portID{}, syscall.ENOENT)
	p := newFakePort()
	r := startHeadlessWatched(t, p, 2*time.Second, func(string, *serial.Mode) (serial.Port, error) {
		t.Error("reopen without a node to compare")
		return nil, errors.New("unexpected")
	}, testWatch, nil)
	time.Sleep(5 * testWatch)
	if strings.Contains(r.log(), "watchdog") || p.isClosed() {
		t.Fatalf("acted with no node at open:\n%s", r.log())
	}
}

func holderFields(t *testing.T, port string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(holderFile(port))
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

func waitHolder(t *testing.T, port string, ok func(map[string]string) bool) map[string]string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f := holderFields(t, port)
		if f != nil && ok(f) {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder record never matched; last: %v", f)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fieldMs(f map[string]string, k string) int64 {
	v, _ := strconv.ParseInt(f[k], 10, 64)
	return v
}

// The heartbeat refreshes this process's holder record every tick, keeps
// pid and text-log, and reports rx; it never creates a record or takes
// over one that names another pid.
func TestHeartbeat(t *testing.T) {
	fastWatchdog(t)
	useHolderDir(t)
	useFakeNode(t)
	const port = "/dev/fake"
	unpublish := publishHolder(port, "/x/text.log")
	t.Cleanup(unpublish)
	p := newFakePort()
	r := startHeadlessWatched(t, p, 2*time.Second, nil, testWatch, func(st *state, node string) {
		refreshHolder(port, "/x/text.log", heartbeatFields(st, node))
	})

	f := waitHolder(t, port, func(f map[string]string) bool { return f["heartbeat"] != "" })
	if f["pid"] != strconv.Itoa(os.Getpid()) || f["text-log"] != "/x/text.log" || f["node"] != "ok" || f["last-rx"] != "0" {
		t.Fatalf("first heartbeat = %v", f)
	}
	if age := time.Now().UnixMilli() - fieldMs(f, "heartbeat"); age < 0 || age > 1000 {
		t.Errorf("heartbeat %d ms old", age)
	}
	if age := time.Now().UnixMilli() - fieldMs(f, "last-poll"); age < 0 || age > 1000 {
		t.Errorf("last-poll %d ms old", age)
	}
	p.rx <- []byte("hi\n")
	f = waitHolder(t, port, func(f map[string]string) bool { return f["rx-bytes"] == "3" })
	if age := time.Now().UnixMilli() - fieldMs(f, "last-rx"); age < 0 || age > 1000 {
		t.Errorf("last-rx %d ms old after rx: %v", age, f)
	}
	h1 := fieldMs(f, "heartbeat")
	f = waitHolder(t, port, func(f map[string]string) bool { return fieldMs(f, "heartbeat") > h1 })

	// Another pid's record (it took the port during a reopen wait) is kept.
	other := fmt.Sprintf("pid=%d\ntext-log=/other.log\n", os.Getppid())
	if err := os.WriteFile(holderFile(port), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * watchdogTick)
	if data, _ := os.ReadFile(holderFile(port)); string(data) != other {
		t.Errorf("other pid's record rewritten:\n%s", data)
	}
	// No record: none is created.
	_ = os.Remove(holderFile(port))
	time.Sleep(5 * watchdogTick)
	if _, err := os.Stat(holderFile(port)); err == nil {
		t.Error("heartbeat created a holder record")
	}
	safeClose(r.stop)
	r.waitResult(t, time.Second)
}
