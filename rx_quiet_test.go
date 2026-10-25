//go:build darwin || linux

// End-to-end tests for UM-GPM7-camera #1608: a console line sent while the
// device is still transmitting a log line can be corrupted on the device
// side, and the device cannot prevent it. serial-term therefore holds each
// line until RX has been quiet for --rx-quiet (default 50ms), and sends it
// anyway after --rx-quiet-max (default 1s) of waiting.
//
// The pty master plays the device: the test writes an RX stream into it and
// reads serial-term's TX back from it, time-stamping both sides.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rxStream writes one byte to the master every interval until stop closes,
// and records the time of each write.
type rxStream struct {
	mu     sync.Mutex
	writes []time.Time
}

func (s *rxStream) run(t *testing.T, master *os.File, interval, length time.Duration, done chan<- struct{}) {
	defer close(done)
	end := time.Now().Add(length)
	for time.Now().Before(end) {
		if _, err := master.Write([]byte{'.'}); err != nil {
			t.Errorf("rx stream write: %v", err)
			return
		}
		s.mu.Lock()
		s.writes = append(s.writes, time.Now())
		s.mu.Unlock()
		time.Sleep(interval)
	}
}

// lastBefore returns the last stream write at or before at, and whether any
// stream write came after at (RX was still active when at happened).
func (s *rxStream) lastBefore(at time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last time.Time
	later := false
	for _, w := range s.writes {
		if !w.After(at) {
			last = w
		} else {
			later = true
		}
	}
	return last, later
}

// skipIfStalled skips the test when the stream itself paused for 50ms or
// more between from and to: on a loaded machine that pause is a real RX
// quiet gap, and serial-term is right to send in it.
func (s *rxStream) skipIfStalled(t *testing.T, from, to time.Time) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 1; i < len(s.writes); i++ {
		if s.writes[i].Before(from) || s.writes[i-1].After(to) {
			continue
		}
		if gap := s.writes[i].Sub(s.writes[i-1]); gap >= 50*time.Millisecond {
			t.Skipf("rx stream stalled for %v (test machine overloaded)", gap)
		}
	}
}

// waitTxByte reads the master until b arrives and returns its arrival time.
func waitTxByte(t *testing.T, master *os.File, b byte, timeout time.Duration) time.Time {
	t.Helper()
	got := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := master.Read(buf)
			if err != nil {
				return
			}
			if bytes.IndexByte(buf[:n], b) >= 0 {
				got <- time.Now()
				return
			}
		}
	}()
	select {
	case at := <-got:
		return at
	case <-time.After(timeout):
		t.Fatalf("TX byte %q never reached the device side within %v", b, timeout)
		return time.Time{}
	}
}

// startRxQuietChild starts serial-term headless on the pty slave with stdin
// on a read-write FIFO and extra flags, and waits for the session banner
// (the port is open by then).
func startRxQuietChild(t *testing.T, bin, slave string, extra ...string) (*os.File, string, *bytes.Buffer) {
	t.Helper()
	textLog := filepath.Join(t.TempDir(), "text.log")
	fifo := fifoRWStdin(t)
	args := append(append([]string{}, extra...), "--mode", "ascii", "--text-log", textLog, slave)
	var out bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdin = fifo
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pollFileFor(t, textLog, "session start", 5*time.Second)
	return fifo, textLog, &out
}

// TestSendWaitsForRxQuiet: with RX bytes arriving every 10ms (gaps well under
// 50ms) for 300ms, a line written to stdin 100ms in must reach the device
// only after RX has been quiet for at least 50ms. Before the fix it went out
// at once, while the stream was still running.
func TestSendWaitsForRxQuiet(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	fifo, textLog, _ := startRxQuietChild(t, bin, slave)

	var s rxStream
	streamDone := make(chan struct{})
	go s.run(t, master, 10*time.Millisecond, 300*time.Millisecond, streamDone)
	time.Sleep(100 * time.Millisecond)
	sentAt := time.Now()
	if _, err := fifo.WriteString("hello\n"); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	arrived := waitTxByte(t, master, 'h', 3*time.Second)
	<-streamDone
	s.skipIfStalled(t, sentAt, arrived)

	last, rxStillActive := s.lastBefore(arrived)
	gap := arrived.Sub(last)
	t.Logf("delay after stdin write %v, rx gap before first TX byte %v, rx still active %v",
		arrived.Sub(sentAt).Round(time.Millisecond), gap.Round(time.Millisecond), rxStillActive)
	if rxStillActive {
		t.Errorf("first TX byte reached the device while RX was still active (gap %v)", gap)
	}
	if gap < 50*time.Millisecond {
		t.Errorf("first TX byte came %v after the last RX byte, want >= 50ms of RX quiet", gap)
	}
	pollFileFor(t, textLog, "> hello", 2*time.Second)
	pollFileFor(t, textLog, "tx delayed", 2*time.Second)
	logTextLine(t, textLog, "tx delayed")
}

// logTextLine logs the first text-log line holding substr.
func logTextLine(t *testing.T, path, substr string) {
	t.Helper()
	data, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, substr) {
			t.Logf("text log: %s", l)
			return
		}
	}
}

// TestSendCapWhenRxNeverQuiet: a device that logs without a 50ms gap must
// not hold a line forever; it goes out at about the 1s cap, and the text
// log says the cap was hit.
func TestSendCapWhenRxNeverQuiet(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	fifo, textLog, _ := startRxQuietChild(t, bin, slave)

	var s rxStream
	streamDone := make(chan struct{})
	go s.run(t, master, 10*time.Millisecond, 1600*time.Millisecond, streamDone)
	time.Sleep(100 * time.Millisecond)
	sentAt := time.Now()
	if _, err := fifo.WriteString("hello\n"); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	arrived := waitTxByte(t, master, 'h', 3*time.Second)
	<-streamDone
	s.skipIfStalled(t, sentAt, arrived)
	delay := arrived.Sub(sentAt)
	_, rxStillActive := s.lastBefore(arrived)
	t.Logf("delay after stdin write %v, rx still active %v", delay.Round(time.Millisecond), rxStillActive)
	if !rxStillActive {
		t.Errorf("stream ended before the TX byte arrived; the cap was not exercised")
	}
	if delay < 900*time.Millisecond || delay > 1300*time.Millisecond {
		t.Errorf("line went out %v after stdin, want about the 1s cap", delay)
	}
	pollFileFor(t, textLog, "rx-quiet-max", 2*time.Second)
	logTextLine(t, textLog, "rx-quiet-max")
}

// TestSendIdlePortNoDelay: with no RX at all the line goes out at once.
func TestSendIdlePortNoDelay(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	fifo, textLog, _ := startRxQuietChild(t, bin, slave)

	sentAt := time.Now()
	if _, err := fifo.WriteString("hello\n"); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	delay := waitTxByte(t, master, 'h', 3*time.Second).Sub(sentAt)
	t.Logf("delay after stdin write %v", delay.Round(time.Millisecond))
	if delay > 200*time.Millisecond {
		t.Errorf("idle port: line went out %v after stdin, want no added delay", delay)
	}
	pollFileFor(t, textLog, "> hello", 2*time.Second)
	data, _ := os.ReadFile(textLog)
	if strings.Contains(string(data), "tx delayed") {
		t.Errorf("idle port: text log reports a delay:\n%s", data)
	}
}

// TestSendSOHImmediate: a line starting with SOH (0x01) skips the hold even
// while RX never goes quiet, and the SOH itself never reaches the device.
func TestSendSOHImmediate(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	fifo, textLog, _ := startRxQuietChild(t, bin, slave)

	var s rxStream
	streamDone := make(chan struct{})
	go s.run(t, master, 10*time.Millisecond, 600*time.Millisecond, streamDone)
	time.Sleep(100 * time.Millisecond)

	type txLine struct {
		at   time.Time
		wire []byte
	}
	got := make(chan txLine, 1)
	go func() {
		var wire []byte
		var first time.Time
		buf := make([]byte, 256)
		for {
			n, err := master.Read(buf)
			if err != nil {
				return
			}
			if first.IsZero() && n > 0 {
				first = time.Now()
			}
			wire = append(wire, buf[:n]...)
			if bytes.IndexByte(wire, '\n') >= 0 {
				got <- txLine{first, wire}
				return
			}
		}
	}()
	sentAt := time.Now()
	if _, err := fifo.WriteString("\x01hello\n"); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	var tx txLine
	select {
	case tx = <-got:
	case <-time.After(3 * time.Second):
		t.Fatalf("SOH line never reached the device side")
	}
	<-streamDone
	delay := tx.at.Sub(sentAt)
	_, rxStillActive := s.lastBefore(tx.at)
	t.Logf("delay after stdin write %v, rx still active %v, wire %q", delay.Round(time.Millisecond), rxStillActive, tx.wire)
	if !rxStillActive {
		t.Errorf("SOH line was held until RX stopped")
	}
	if delay > 50*time.Millisecond {
		t.Errorf("SOH line went out %v after stdin, want immediate", delay)
	}
	if bytes.IndexByte(tx.wire, 0x01) >= 0 {
		t.Errorf("SOH byte reached the device: %q", tx.wire)
	}
	if string(tx.wire) != "hello\n" {
		t.Errorf("wire = %q, want %q", tx.wire, "hello\n")
	}
	pollFileFor(t, textLog, "tx immediate (SOH)", 2*time.Second)
	pollFileFor(t, textLog, "> hello<LF>", 2*time.Second)
	logTextLine(t, textLog, "tx immediate")
}

// TestSendRxQuietDisabled: --rx-quiet 0 restores the old immediate send, so
// the line reaches the device while the stream is still running.
func TestSendRxQuietDisabled(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	fifo, _, out := startRxQuietChild(t, bin, slave, "--rx-quiet", "0")

	var s rxStream
	streamDone := make(chan struct{})
	go s.run(t, master, 10*time.Millisecond, 300*time.Millisecond, streamDone)
	time.Sleep(100 * time.Millisecond)
	sentAt := time.Now()
	if _, err := fifo.WriteString("hello\n"); err != nil {
		t.Fatalf("write fifo: %v", err)
	}
	arrived := waitTxByte(t, master, 'h', 3*time.Second)
	<-streamDone
	last, rxStillActive := s.lastBefore(arrived)
	t.Logf("delay after stdin write %v, rx gap %v, rx still active %v",
		arrived.Sub(sentAt).Round(time.Millisecond), arrived.Sub(last).Round(time.Millisecond), rxStillActive)
	if !rxStillActive {
		t.Errorf("--rx-quiet 0: line was held until RX stopped\n%s", out.String())
	}
}
