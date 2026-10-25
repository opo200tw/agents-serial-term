//go:build darwin || linux

// End-to-end regression tests for issue #1027: SIGTERM/SIGINT must end
// serial-term and run the shutdown path (port close, reader drain, log
// Sync, final stats) even when stdin is a FIFO opened read-write — the
// `0<>fifo` shell pattern. A read-write FIFO never delivers EOF, so the
// pre-fix main() parked forever inside stdinHandler.run()'s
// bufio.Scanner.Scan() and only SIGKILL could stop the process, skipping
// the shutdown Sync() of --log / --text-log.
//
// The serial PORT is the slave side of a pty the test opens itself, so no
// real serial device (/dev/cu.*, /dev/tty.usb*) is ever touched. On
// platforms without a usable pty the tests skip instead of failing.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signalShutdownTimeout is the wall-clock budget the fixed process gets
// to exit after the signal. The pre-fix process never exited at all.
const signalShutdownTimeout = 2 * time.Second

// buildBinary compiles the serial-term binary into the test's temp dir
// and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "serial-term")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// fifoRWStdin creates a FIFO and opens it read-write, reproducing the
// `0<>fifo` scenario of #1027: an O_RDWR FIFO read never reaches EOF
// even with no writer attached, so bufio.Scanner.Scan() can block on it
// forever.
func fifoRWStdin(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open fifo: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// startChild launches the built binary with the given stdin and a port
// path, capturing stdout/stderr into buffers that may be read after
// cmd.Wait() (exec flushes its copy goroutines there).
func startChild(t *testing.T, bin string, stdin *os.File, port string, textLog, rawLog string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(bin, "--text-log", textLog, "--log", rawLog, port)
	cmd.Stdin = stdin
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil { // still running: kill and reap
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, &out, &errOut
}

// waitExit races cmd.Wait() against the deadline and returns the Wait
// error (nil for a clean exit).
func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("serial-term did not exit within %v of shutdown (#1027: shutdown was wedged in scanner.Scan on the never-EOF stdin)", timeout)
		return nil // unreachable
	}
}

// pollFileFor waits until the file at path contains substr.
func pollFileFor(t *testing.T, path, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), substr) {
			return
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(path)
			t.Fatalf("timed out after %v waiting for %q in %s; content so far: %q",
				timeout, substr, path, string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fatalWithOutput reports err together with everything the child printed,
// so port-open failures on a runner are diagnosable.
func fatalWithOutput(t *testing.T, msg string, err error, out, errOut *bytes.Buffer) {
	t.Helper()
	t.Fatalf("%s: %v\n--- stdout ---\n%s\n--- stderr ---\n%s",
		msg, err, out.String(), errOut.String())
}

// assertShutdownCompleted checks everything the graceful shutdown path of
// main() must produce: the "session start" banner in --text-log, the
// final stats line on stdout (printed after the log Sync() calls), and
// an existing (empty here — nothing was ever received) raw --log.
func assertShutdownCompleted(t *testing.T, textLogPath, rawLogPath string, out *bytes.Buffer, wantBytesOut string) {
	t.Helper()

	data, err := os.ReadFile(textLogPath)
	if err != nil {
		t.Fatalf("read text log: %v", err)
	}
	if !strings.Contains(string(data), "session start") {
		t.Errorf("text log missing \"session start\" line:\n%s", data)
	}
	if !strings.Contains(string(data), "> hello") {
		t.Errorf("text log missing the tx'ed line \"> hello\":\n%s", data)
	}

	// The final stats line is the last thing main() prints in headless
	// mode — after port close, reader drain and both Sync() calls — so
	// seeing it proves the whole shutdown sequence ran.
	if !strings.Contains(out.String(), "bytes in, "+wantBytesOut+" bytes out") {
		t.Errorf("stdout missing final stats line (want %q in it):\n%s", wantBytesOut+" bytes", out.String())
	}

	fi, err := os.Stat(rawLogPath)
	if err != nil {
		t.Fatalf("raw --log missing after shutdown: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("raw --log should be empty (nothing received), has %d bytes", fi.Size())
	}
}

// testSignalShutdown runs one full session against a pty port with stdin
// on a read-write FIFO, sends sig, and asserts a clean, prompt shutdown.
func testSignalShutdown(t *testing.T, bin string, sig syscall.Signal) {
	t.Helper()

	textLogPath := filepath.Join(t.TempDir(), "text.log")
	rawLogPath := filepath.Join(t.TempDir(), "raw.log")
	fifo := fifoRWStdin(t)
	slavePath, _ := openPty(t)

	cmd, out, errOut := startChild(t, bin, fifo, slavePath, textLogPath, rawLogPath)

	// Wait for the session to come up (the banner is written + fsynced
	// before the run loop starts), then send one line so the session also
	// has tx state to flush.
	pollFileFor(t, textLogPath, "session start", 5*time.Second)
	if _, err := fifo.WriteString("hello\n"); err != nil {
		t.Fatalf("write to fifo: %v", err)
	}
	// Only signal once the line was rendered (deterministic byte counts).
	pollFileFor(t, textLogPath, "> hello", 5*time.Second)

	t0 := time.Now()
	if err := cmd.Process.Signal(sig); err != nil {
		fatalWithOutput(t, "send "+sig.String(), err, out, errOut)
	}
	if err := waitExit(t, cmd, signalShutdownTimeout); err != nil {
		fatalWithOutput(t, "serial-term exited uncleanly on "+sig.String(), err, out, errOut)
	}
	if took := time.Since(t0); took > signalShutdownTimeout {
		t.Errorf("%s shutdown took %v", sig, took)
	}

	assertShutdownCompleted(t, textLogPath, rawLogPath, out, "6")
}

// TestSignalShutdownStdinFifoRW is the #1027 regression: both signals
// must work when stdin is a raced read-write FIFO that never EOFs.
func TestSignalShutdownStdinFifoRW(t *testing.T) {
	bin := buildBinary(t)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		sig := sig
		t.Run(sig.String(), func(t *testing.T) {
			testSignalShutdown(t, bin, sig)
		})
	}
}

// TestQuitStillExits checks pre-existing semantics kept after the #1027
// fix: /quit closes the session even when stdin is a read-write FIFO.
func TestQuitStillExits(t *testing.T) {
	bin := buildBinary(t)
	textLogPath := filepath.Join(t.TempDir(), "text.log")
	rawLogPath := filepath.Join(t.TempDir(), "raw.log")
	fifo := fifoRWStdin(t)
	slavePath, _ := openPty(t)

	cmd, out, errOut := startChild(t, bin, fifo, slavePath, textLogPath, rawLogPath)
	pollFileFor(t, textLogPath, "session start", 5*time.Second)
	if _, err := fifo.WriteString("/quit\n"); err != nil {
		t.Fatalf("write to fifo: %v", err)
	}
	if err := waitExit(t, cmd, signalShutdownTimeout); err != nil {
		fatalWithOutput(t, "/quit did not exit cleanly", err, out, errOut)
	}
	data, err := os.ReadFile(textLogPath)
	if err != nil {
		t.Fatalf("read text log: %v", err)
	}
	if !strings.Contains(string(data), "session start") {
		t.Errorf("text log missing \"session start\" line:\n%s", data)
	}
	if !strings.Contains(out.String(), "bytes in, 0 bytes out") {
		t.Errorf("stdout missing final stats line:\n%s", out.String())
	}
}

// TestStdinEOFDoesNotShutdown pins another kept semantic: EOF on stdin
// (a closed pipe) must NOT end the session — the user may pipe a command
// script and keep watching device output. Only an explicit signal (or
// /quit) may shut it down afterwards.
func TestStdinEOFDoesNotShutdown(t *testing.T) {
	bin := buildBinary(t)
	textLogPath := filepath.Join(t.TempDir(), "text.log")
	rawLogPath := filepath.Join(t.TempDir(), "raw.log")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Skipf("pipe: %v", err)
	}
	t.Cleanup(func() { pr.Close(); pw.Close() })
	slavePath, _ := openPty(t)

	cmd, out, errOut := startChild(t, bin, pr, slavePath, textLogPath, rawLogPath)
	pollFileFor(t, textLogPath, "session start", 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// EOF should reach the scanner but leave the session running.
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	select {
	case werr := <-done:
		fatalWithOutput(t, "serial-term exited on stdin EOF (must stay running)", werr, out, errOut)
	default:
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case werr := <-done:
		if werr != nil {
			fatalWithOutput(t, "SIGTERM after EOF did not exit cleanly", werr, out, errOut)
		}
	case <-time.After(signalShutdownTimeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("SIGTERM after EOF did not end the process within %v", signalShutdownTimeout)
	}
	if !strings.Contains(out.String(), "bytes in") {
		t.Errorf("stdout missing final stats line:\n%s", out.String())
	}
}
