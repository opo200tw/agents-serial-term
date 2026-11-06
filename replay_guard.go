package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// RX replay guard (#8). After a port sat closed while the device kept
// printing, the macOS FTDI driver (or the chip) can hand read(2) the same
// USB packet payload thousands of times a second for seconds: 62-byte
// copies at ~700 KB/s on a 115200-baud line whose ceiling is 11.5 KB/s.
// The copies are inside the bytes read(2) returns, so no flush or Read
// change in serial-term can stop them; this guard drops them.
//
// A read is dropped only when both hold:
//   - it is over the line's byte budget: more bytes arrived than the baud
//     rate could carry, plus a backlog allowance, and
//   - it continues the received stream with a period of at most
//     replayMaxPeriod bytes (each byte equals the byte P earlier).
//
// Rate alone would drop a real packet that lands mid-flood; repetition
// alone would drop a device that reprints the same line. Real UART data can
// only be over budget after a reader stall longer than the allowance covers,
// and then only periodic backlog is dropped.

// replayGuardBaud returns the baud rate the guard runs at for this port, or
// 0 (guard off). The byte budget only holds with a real UART behind the
// port, so the guard runs only on macOS USB-UART bridges (cu./tty.usbserial-*,
// the FTDI driver's naming); USB CDC ports (usbmodem, ttyACM) ignore baud and
// deliver at USB speed.
func replayGuardBaud(path string, baud int) int {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "cu.usbserial") || strings.HasPrefix(base, "tty.usbserial") {
		return baud
	}
	return 0
}

// guardNow is the guard's clock; tests stand in a fake one.
var guardNow = time.Now

const (
	// replayAllowance is the backlog a real port can hand over at once
	// beyond the baud budget: chip FIFO plus driver and tty queues after a
	// short reader stall. It is also how much of a flood passes before the
	// guard trips.
	replayAllowance = 4096
	// replayOpenAllowance is the backlog right after an open: openAndFlush
	// has just emptied the host queues, so only the chip FIFO (256 bytes on
	// an FT232R) can still be pending.
	replayOpenAllowance = 512
	// replayMaxPeriod covers one bulk-IN payload of full-speed (62) and
	// high-speed (510) FTDI parts.
	replayMaxPeriod = 512
)

type replayGuard struct {
	rate   float64 // max bytes/s the line can carry
	tokens float64 // bytes the line could still deliver right now
	cap    float64
	last   time.Time
	hist   []byte // last replayMaxPeriod received bytes, dropped ones included
	period int    // period of the current drop episode, 0 = none

	// Episode stats for the summary line.
	epStart time.Time
	epBytes int
	epReads int
}

// newReplayGuard returns nil (guard off) for an unknown baud rate.
func newReplayGuard(baud int) *replayGuard {
	if baud <= 0 {
		return nil
	}
	// baud/8 ignores start and stop bits: 25% above the 8N1 byte rate and
	// above any framing serial-term can set.
	rate := float64(baud) / 8
	return &replayGuard{rate: rate, tokens: replayOpenAllowance, cap: rate + replayAllowance, last: guardNow()}
}

// check reports whether p must be dropped. When an episode starts or ends it
// also returns the marker line to log.
func (g *replayGuard) check(p []byte) (drop bool, note string) {
	now := guardNow()
	g.tokens += now.Sub(g.last).Seconds() * g.rate
	g.last = now
	if g.tokens > g.cap {
		g.tokens = g.cap
	}
	g.tokens -= float64(len(p))
	over := g.tokens < 0
	if over {
		g.tokens = 0 // dropped bytes do not run up a debt real data must repay
	}
	if over {
		if per := g.continuation(p); per > 0 {
			if g.period == 0 {
				g.epStart, g.epBytes, g.epReads = now, 0, 0
				note = fmt.Sprintf("--- rx replay guard: dropping a %d-byte unit repeated faster than the baud rate allows (#8) ---", per)
			}
			g.period = per
			g.epBytes += len(p)
			g.epReads++
			drop = true
		}
	}
	if !drop {
		note = g.endEpisode(now)
	}
	g.hist = append(g.hist, p...)
	if n := len(g.hist); n > replayMaxPeriod {
		g.hist = append(g.hist[:0], g.hist[n-replayMaxPeriod:]...)
	}
	return drop, note
}

// endEpisode closes a drop episode and returns its summary, or "".
func (g *replayGuard) endEpisode(now time.Time) string {
	if g.period == 0 {
		return ""
	}
	g.period = 0
	return fmt.Sprintf("--- rx replay guard: dropped %d bytes in %d reads over %v ---",
		g.epBytes, g.epReads, now.Sub(g.epStart).Round(time.Millisecond))
}

// continuation returns the smallest period P <= replayMaxPeriod with which
// every byte of p equals the byte P earlier in the received stream, or 0.
// With less than P bytes of history, p must hold two full periods itself.
func (g *replayGuard) continuation(p []byte) int {
	s := append(append(make([]byte, 0, len(g.hist)+len(p)), g.hist...), p...)
	start := len(g.hist) // first byte of p in s
	try := func(per int) bool {
		if start >= per {
			return bytes.Equal(s[start:], s[start-per:len(s)-per])
		}
		return len(p) >= 2*per && bytes.Equal(p[per:], p[:len(p)-per])
	}
	if g.period > 0 && try(g.period) {
		return g.period
	}
	for per := 1; per <= replayMaxPeriod; per++ {
		if try(per) {
			return per
		}
	}
	return 0
}
