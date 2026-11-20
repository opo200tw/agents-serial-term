package main

// Headless watchdog (UM-GPM7-camera #2802). On 2026-11-19 a bench capture
// stopped writing RX four times while the process lived on with no
// "port lost" line. The logs show TX lines written minutes after the last
// RX row, so the state lock, the text log and port writes still worked:
// the reader was inside Port.Read, either getting timeouts with no data or
// parked in the kernel. Which one, and why, is not known, so this file
// makes the failure visible instead of guessing a fix:
//
//   - a Read that has not returned for --watchdog cannot be a quiet
//     device (Read returns every portReadTimeout without data), so the
//     reader is stalled: mark it and handle it as a lost port;
//   - PORT gone or now a different node while nothing arrives for
//     --watchdog means the open fd is orphaned: the same;
//   - a quiet port on the same node is left alone (USB storage mode, a
//     board that is off), since a reopen costs a flush and risks the #8
//     replay;
//   - every tick refreshes the heartbeat in the holder file, so a bench
//     script can tell "serial-term reads and nothing arrives" from
//     "serial-term is stuck" without parsing the log.

import (
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

// exitStalled is the exit code when the watchdog closed the port of a
// stalled reader and the reader still did not stop.
const exitStalled = 4

// watchdogTick is how often the watchdog checks and beats; readerStopWait
// is how long it waits for the reader to exit after closing the port;
// lockWait bounds the wait for st.mu before a marker is written without
// it. Vars for tests.
var (
	watchdogTick   = time.Second
	readerStopWait = 2 * time.Second
	lockWait       = time.Second
)

// monoNow is a monotonic clock reading in ns. The reader-stall check uses
// it because the monotonic clock stops while the Mac sleeps: a capture
// frozen by sleep must not wake up "stalled" (2026-11-19 18:28 to 19:03 in
// the #2802 logs was such a sleep).
var monoStart = time.Now()

func monoNow() int64 { return int64(time.Since(monoStart)) }

// portID tells one device node from another that later takes its path.
type portID struct{ rdev, ino uint64 }

// statPort is statPortID behind a var so tests can replace the node.
var statPort = statPortID

// watchReader watches one open of p until the reader exits (done) or the
// session stops. On a stall it writes the marker, records the cause for
// runHeadless's "port lost" line and closes p in the background (Close can
// block behind an in-flight read or write). It returns true only when the
// reader then did not exit within readerStopWait; the caller exits with
// exitStalled. id is PORT's node at open time, haveID false when that stat
// failed (no node check then).
func watchReader(p serial.Port, path string, id portID, haveID bool, opened time.Time, st *state, h *stdinHandler, after time.Duration, beat func(node string), done, stop <-chan struct{}) bool {
	tick := time.NewTicker(watchdogTick)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return false
		case <-done:
			return false
		case <-tick.C:
		}
		node := "ok"
		if !haveID {
			node = "unknown"
		} else if cur, err := statPort(path); err != nil {
			node = "gone"
		} else if cur != id {
			node = "replaced"
		}
		if beat != nil {
			beat(node)
		}
		if after <= 0 {
			continue
		}
		var cause error
		if since := time.Duration(monoNow() - st.lastPoll.Load()); since >= after {
			cause = fmt.Errorf("reader stalled: no read returned for %v", since.Round(100*time.Millisecond))
		} else if node == "gone" || node == "replaced" {
			// Wall clock, as lastRx is: a node replaced during a sleep
			// counts the sleep as quiet time, which is right.
			last := opened.Round(0)
			if rx := time.Unix(0, st.lastRx.Load()); rx.After(last) {
				last = rx
			}
			if quiet := time.Now().Round(0).Sub(last); quiet >= after {
				cause = fmt.Errorf("port node %s and no rx for %v", node, quiet.Round(100*time.Millisecond))
			}
		}
		if cause == nil {
			continue
		}
		st.watchdogEvent(fmt.Sprintf("--- watchdog: %v; closing %s (#2802) ---", cause, path))
		if h.recordLoss(p, cause) {
			go func() { _ = p.Close() }()
		}
		select {
		case <-done:
			return false
		case <-stop:
			return false
		case <-time.After(readerStopWait):
			st.watchdogEvent(fmt.Sprintf("--- watchdog: reader did not stop within %v of the close; exiting %d ---", readerStopWait, exitStalled))
			return true
		}
	}
}

// watchdogEvent writes a marker to the text log and syncs it. Unlike
// portEvent it never waits on mu for more than lockWait, since a reader
// stuck while holding mu is one of the stalls it reports, and it skips
// stdout, which may be the thing that blocks. A half-rendered ASCII line
// lives in asciiBuf, not in the file, so the marker cannot glue to it.
func (s *state) watchdogEvent(msg string) {
	deadline := time.Now().Add(lockWait)
	locked := s.mu.TryLock()
	for !locked && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		locked = s.mu.TryLock()
	}
	if locked {
		defer s.mu.Unlock()
	}
	if s.textLog != nil {
		fmt.Fprintln(s.textLog, time.Now().Format("2006-01-02 15:04:05.000 ")+msg)
		_ = s.textLog.Sync()
	}
}

// heartbeatFields is the holder-file heartbeat: Unix ms times, 0 = never.
func heartbeatFields(st *state, node string) string {
	now := time.Now()
	lastRx := int64(0)
	if ns := st.lastRx.Load(); ns != 0 {
		lastRx = ns / int64(time.Millisecond)
	}
	// lastPoll is monotonic: shown as the wall time that long ago.
	lastPoll := now.Add(-time.Duration(monoNow() - st.lastPoll.Load())).UnixMilli()
	var b strings.Builder
	fmt.Fprintf(&b, "heartbeat=%d\n", now.UnixMilli())
	fmt.Fprintf(&b, "last-poll=%d\n", lastPoll)
	fmt.Fprintf(&b, "last-rx=%d\n", lastRx)
	fmt.Fprintf(&b, "rx-bytes=%d\n", st.rxBytes.Load())
	fmt.Fprintf(&b, "node=%s\n", node)
	return b.String()
}
