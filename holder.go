package main

// When a port is already held by another serial-term, a second one cannot
// open it (the holder sets TIOCEXCL, the open fails with "Serial port busy").
// Instead of only failing, the second one tells who holds it and, when run
// from a terminal, prints the holder's --text-log and follows it.
//
// The holder publishes itself in a small file per port (holderFile) right
// after its port opens: its pid and its --text-log path. A file whose pid is
// gone is stale and ignored, so a holder killed with SIGKILL leaves nothing
// that misleads the next run.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
)

// holderDir holds one file per open port. A fixed /tmp path, not
// os.TempDir(): TMPDIR differs between shells and sandboxes on macOS, and
// the reader must find what another process wrote. A var for tests.
var holderDir = func() string {
	return fmt.Sprintf("/tmp/serial-term-%d", os.Getuid())
}

// followPoll is how often followLog checks the log for new bytes and the
// holder for being alive. A var for tests.
var followPoll = 200 * time.Millisecond

// followTailLines is how many existing log lines are printed before following.
const followTailLines = 30

// followTailBytes bounds the read for those lines: a long capture's log can
// be hundreds of MB.
const followTailBytes = 256 * 1024

type holderInfo struct {
	pid     int
	textLog string
}

func holderFile(port string) string {
	base := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, filepath.Base(port))
	return filepath.Join(holderDir(), base+".holder")
}

// publishHolder records this process as the holder of port and returns the
// function that removes the record again. The remover only deletes a file
// that still names this pid: while this process waits to reopen a lost
// port, another serial-term can take the port and publish itself, and this
// one giving up must not delete that record.
func publishHolder(port, textLog string) func() {
	if runtime.GOOS == "windows" {
		return func() {}
	}
	if textLog != "" {
		if abs, err := filepath.Abs(textLog); err == nil {
			textLog = abs
		}
	}
	path := holderFile(port)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	body := fmt.Sprintf("pid=%d\ntext-log=%s\n", os.Getpid(), textLog)
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return func() {}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return func() {}
	}
	return func() {
		if h, err := parseHolder(path); err == nil && h.pid == os.Getpid() {
			_ = os.Remove(path)
		}
	}
}

func parseHolder(path string) (holderInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return holderInfo{}, err
	}
	defer f.Close()
	var h holderInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			h.pid, _ = strconv.Atoi(v)
		case "text-log":
			h.textLog = v
		}
	}
	if h.pid <= 0 {
		return holderInfo{}, errors.New("no pid")
	}
	return h, nil
}

// readHolder returns the live serial-term holding port, if one published
// itself.
func readHolder(port string) (holderInfo, bool) {
	h, err := parseHolder(holderFile(port))
	if err != nil || !pidAlive(h.pid) {
		return holderInfo{}, false
	}
	return h, true
}

func isPortBusy(err error) bool {
	var pe *serial.PortError
	return errors.As(err, &pe) && pe.Code() == serial.PortBusy
}

// reportBusy runs after the open of port failed with "busy". It returns the
// exit status. With no live serial-term holder on record (screen, minicom,
// an older serial-term) it adds nothing and returns 2 as before. Otherwise
// it names the holder; follow (stdin and stdout are a terminal) prints the
// holder's log and follows it until stop closes or the holder exits, and
// returns 0. Without follow it stays a startup failure (2) so scripts that
// start serial-term on a busy port do not hang.
func reportBusy(port string, follow bool, out, errOut io.Writer, stop <-chan struct{}) int {
	h, ok := readHolder(port)
	if !ok {
		return 2
	}
	if h.textLog == "" {
		fmt.Fprintf(errOut, "%s is held by serial-term pid %d, which has no --text-log; nothing to show\n", port, h.pid)
		return 2
	}
	if !follow {
		fmt.Fprintf(errOut, "%s is held by serial-term pid %d; its log: %s\n", port, h.pid, h.textLog)
		return 2
	}
	fmt.Fprintf(out, "--- %s is held by serial-term pid %d; following its log %s (Ctrl-C to stop) ---\n", port, h.pid, h.textLog)
	err := followLog(h.textLog, followTailLines, out, stop, func() bool { return pidAlive(h.pid) })
	if err != nil {
		fmt.Fprintf(errOut, "follow %s: %v\n", h.textLog, err)
		return 2
	}
	return 0
}

// followLog writes the last n lines of path to out, then every new byte
// appended to it, until stop closes or alive reports false. On the holder
// exiting, whatever it wrote last is printed first.
func followLog(path string, n int, out io.Writer, stop <-chan struct{}, alive func() bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	off := fi.Size() - followTailBytes
	if off < 0 {
		off = 0
	}
	tail := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && err != io.EOF {
		return err
	}
	_, _ = out.Write(lastLines(tail, n))
	off = fi.Size()

	buf := make([]byte, 64*1024)
	drain := func() {
		if fi, err := f.Stat(); err == nil && fi.Size() < off {
			off = 0 // truncated or rewritten
		}
		for {
			k, err := f.ReadAt(buf, off)
			if k > 0 {
				_, _ = out.Write(buf[:k])
				off += int64(k)
			}
			if err != nil || k == 0 {
				return
			}
		}
	}
	tick := time.NewTicker(followPoll)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			drain()
			return nil
		case <-tick.C:
		}
		drain()
		if !alive() {
			drain()
			fmt.Fprintln(out, "--- holder exited; port is free ---")
			return nil
		}
	}
}

// lastLines returns the last n complete-or-trailing lines of b.
func lastLines(b []byte, n int) []byte {
	if n <= 0 {
		return nil
	}
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			n--
			if n == 0 {
				return b[i+1:]
			}
		}
	}
	return b
}
