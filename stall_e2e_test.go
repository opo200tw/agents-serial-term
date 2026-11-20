//go:build darwin || linux

// End-to-end tests for UM-GPM7-camera #2802 with the built binary on ptys
// the test opens itself (no real serial device is touched): a capture
// whose reader is stuck, or whose PORT node was replaced, must not go on
// living with a log that silently stopped; a quiet healthy port must be
// left alone and keep a fresh heartbeat.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// watchArgs keeps the e2e runs short; the default is 10s.
var watchArgs = []string{"--watchdog", "1s"}

func startStallChild(t *testing.T, bin string, stdout *os.File, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, append(append([]string{}, watchArgs...), args...)...)
	cmd.Stdin = fifoRWStdin(t)
	if stdout != nil {
		cmd.Stdout = stdout
	}
	errPath := filepath.Join(t.TempDir(), "stderr")
	errF, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { errF.Close() })
	cmd.Stderr = errF
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

func waitCode(t *testing.T, cmd *exec.Cmd, within time.Duration, logPath string) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return cmd.ProcessState.ExitCode()
	case <-time.After(within):
		data, _ := os.ReadFile(logPath)
		t.Fatalf("serial-term still running after %v; text log tail:\n%s", within, tailString(string(data), 1500))
		return -1
	}
}

func tailString(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// The reader stuck under the state lock (stdout is a pipe nobody reads, so
// a rendered line blocks): before #2802 the text log stopped with no word
// and the process lived on. Now the watchdog writes its markers and the
// process exits 4.
func TestE2EStuckReaderExits(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	logPath := filepath.Join(t.TempDir(), "text.log")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pr.Close(); pw.Close() })
	cmd := startStallChild(t, bin, pw, "--mode", "hex", "--text-log", logPath, slave)
	pollFileFor(t, logPath, "session start", 5*time.Second)
	go func() {
		chunk := []byte(strings.Repeat("0123456789abcdef", 64))
		for i := 0; i < 1024; i++ {
			if _, err := master.Write(chunk); err != nil {
				return
			}
		}
	}()
	code := waitCode(t, cmd, 10*time.Second, logPath)
	data, _ := os.ReadFile(logPath)
	if code != exitStalled {
		t.Fatalf("exit code %d, want %d; log tail:\n%s", code, exitStalled, tailString(string(data), 1500))
	}
	assertInOrder(t, string(data),
		"--- watchdog: reader stalled: no read returned for ",
		"--- watchdog: reader did not stop within ")
}

// PORT is a symlink to one pty; it is repointed to a second pty while the
// first stays open and quiet, as a re-enumerated device would leave an
// orphaned fd. The capture reopens the path and logs the second pty.
func TestE2ENodeReplacedReopens(t *testing.T) {
	bin := buildBinary(t)
	slave1, master1 := openPty(t)
	slave2, master2 := openPty(t)
	link := filepath.Join(t.TempDir(), "stall-e2e-"+strconv.Itoa(os.Getpid()))
	if err := os.Symlink(slave1, link); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "text.log")
	cmd := startStallChild(t, bin, nil, "--mode", "ascii", "--reopen-timeout", "5s", "--text-log", logPath, link)
	pollFileFor(t, logPath, "session start", 5*time.Second)
	if _, err := master1.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	pollFileFor(t, logPath, "< one", 5*time.Second)

	if err := os.Symlink(slave2, link+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link+".new", link); err != nil {
		t.Fatal(err)
	}
	pollFileFor(t, logPath, "--- port reopened: "+link+" ---", 8*time.Second)
	if _, err := master2.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	pollFileFor(t, logPath, "< two", 5*time.Second)
	data, _ := os.ReadFile(logPath)
	assertInOrder(t, string(data),
		"< one",
		"--- watchdog: port node replaced and no rx for ",
		"--- port lost: port node replaced and no rx for ",
		"--- port reopened: "+link+" ---",
		"< two")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := waitCode(t, cmd, 3*time.Second, logPath); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
}

// A quiet pty on the node that was opened: no marker, still running, and
// the holder record carries a fresh heartbeat; rx shows up in it.
func TestE2EQuietPortHeartbeat(t *testing.T) {
	bin := buildBinary(t)
	slave, master := openPty(t)
	logPath := filepath.Join(t.TempDir(), "text.log")
	cmd := startStallChild(t, bin, nil, "--text-log", logPath, slave)
	pollFileFor(t, logPath, "session start", 5*time.Second)
	time.Sleep(3 * time.Second) // three watchdog periods of silence

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "watchdog") || strings.Contains(string(data), "port lost") {
		t.Fatalf("quiet port was acted on:\n%s", data)
	}
	if cmd.ProcessState != nil {
		t.Fatal("serial-term exited on a quiet port")
	}
	f := holderFields(t, slave)
	if f["pid"] != strconv.Itoa(cmd.Process.Pid) || f["node"] != "ok" || f["last-rx"] != "0" {
		t.Fatalf("holder record = %v", f)
	}
	if age := time.Now().UnixMilli() - fieldMs(f, "heartbeat"); age < 0 || age > 2500 {
		t.Errorf("heartbeat %d ms old", age)
	}
	if _, err := master.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	waitHolder(t, slave, func(f map[string]string) bool { return f["rx-bytes"] == "3" && f["last-rx"] != "0" })

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := waitCode(t, cmd, 3*time.Second, logPath); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	if _, err := os.Stat(holderFile(slave)); err == nil {
		t.Error("holder record left after a clean exit")
	}
}
