// Regression tests for #8: after an open that follows a long close, the macOS
// FTDI path hands read(2) the same USB packet payload thousands of times a
// second. The guard in readerLoop must drop those copies and nothing a real
// UART could have sent.
package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"go.bug.st/serial"
)

// unit62 is the replayed 62-byte FTDI packet payload from the bench capture.
var unit62 = []byte(" gap max=13, lag block0->queue n=324 min=48 avg=49 max=61 ms\r\n")

// fresh62 is the real packet that broke the flood in the capture; the driver
// then replayed it too.
var fresh62 = []byte("[26-11-06 17:51:37.134000][D][0x000000][USER] [I][WIFI] radio:")

type scriptChunk struct {
	data []byte
	dt   time.Duration // fake time that passes before this read returns
}

// scriptPort returns one scripted chunk per Read, advancing the fake clock,
// then io.EOF so readerLoop exits.
type scriptPort struct {
	serial.Port
	chunks []scriptChunk
	now    *time.Time
}

func (p *scriptPort) Read(b []byte) (int, error) {
	if len(p.chunks) == 0 {
		return 0, io.EOF
	}
	c := p.chunks[0]
	p.chunks = p.chunks[1:]
	*p.now = p.now.Add(c.dt)
	return copy(b, c.data), nil
}

// runScript feeds chunks through readerLoop at 115200 baud and returns the
// raw bytes passed on (the --log stream) and the system lines.
func runScript(t *testing.T, chunks []scriptChunk) (raw []byte, sys string) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	old := guardNow
	guardNow = func() time.Time { return now }
	defer func() { guardNow = old }()

	var rawBuf, rxBuf, sysBuf bytes.Buffer
	st := &state{mode: modeASCII, baud: 115200,
		sink: lineSink{rxOut: &rxBuf, txOut: io.Discard, sysOut: &sysBuf}}
	done := make(chan struct{})
	readerLoop(&scriptPort{chunks: chunks, now: &now}, &rawBuf, st, func(e error) { t.Errorf("reader error: %v", e) }, make(chan struct{}), done)
	<-done
	return rawBuf.Bytes(), sysBuf.String()
}

func repeatChunk(data []byte, n int, dt time.Duration) []scriptChunk {
	out := make([]scriptChunk, n)
	for i := range out {
		out[i] = scriptChunk{data: data, dt: dt}
	}
	return out
}

// The bench flood: one packet ~11,000 times a second, a real packet that the
// driver also replays, then the real rest of that line.
func TestReplayGuardDropsFlood(t *testing.T) {
	var chunks []scriptChunk
	chunks = append(chunks, repeatChunk(unit62, 2000, 90*time.Microsecond)...)
	chunks = append(chunks, repeatChunk(fresh62, 29, 100*time.Microsecond)...)
	tail := []byte(" rssi -53 dBm ch 149 tx 110 txdrop 0 rx 3732 rxdrop 20 rxovf 0\r\n")
	chunks = append(chunks, scriptChunk{data: tail, dt: 3 * time.Millisecond})

	raw, sys := runScript(t, chunks)

	// The guard may pass up to its open backlog allowance before it trips.
	maxUnit := replayOpenAllowance/len(unit62) + 1
	if got := bytes.Count(raw, unit62); got > maxUnit {
		t.Errorf("replayed unit passed %d times, want <= %d (flood not dropped)", got, maxUnit)
	}
	if got := bytes.Count(raw, fresh62); got != 1 {
		t.Errorf("fresh packet passed %d times, want exactly 1", got)
	}
	if !bytes.HasSuffix(raw, tail) {
		t.Errorf("real tail after the flood missing; raw ends %q", raw[max(0, len(raw)-80):])
	}
	if !strings.Contains(sys, "replay guard") {
		t.Errorf("no replay-guard marker in system output: %q", sys)
	}
}

// A device that reprints an identical line at full baud is real output.
func TestReplayGuardKeepsIdenticalLinesAtBaud(t *testing.T) {
	perPacket := time.Duration(len(unit62)) * 10 * time.Second / 115200
	chunks := repeatChunk(unit62, 300, perPacket)
	raw, sys := runScript(t, chunks)
	if want := bytes.Repeat(unit62, 300); !bytes.Equal(raw, want) {
		t.Errorf("passed %d bytes, want %d (identical lines at baud dropped)", len(raw), len(want))
	}
	if strings.Contains(sys, "replay guard") {
		t.Errorf("guard fired on real output: %q", sys)
	}
}

// After a reader stall the driver hands over its backlog in one read; that
// backlog may be periodic (a device repeating one line) and is still real.
func TestReplayGuardKeepsBacklogAfterStall(t *testing.T) {
	perPacket := time.Duration(len(unit62)) * 10 * time.Second / 115200
	chunks := repeatChunk(unit62, 10, perPacket)
	backlog := bytes.Repeat(unit62, 1024/len(unit62)+1)[:1024]
	chunks = append(chunks, scriptChunk{data: backlog, dt: 500 * time.Millisecond})
	chunks = append(chunks, scriptChunk{data: backlog, dt: 0})
	raw, _ := runScript(t, chunks)
	if want := len(unit62)*10 + 2*1024; len(raw) != want {
		t.Errorf("passed %d bytes, want %d (stall backlog dropped)", len(raw), want)
	}
}

// USB CDC ports deliver at USB speed regardless of baud, so the guard is off.
func TestReplayGuardOnlyOnUSBSerialBridges(t *testing.T) {
	for path, want := range map[string]int{
		"/dev/cu.usbserial-A5069RR4":  115200,
		"/dev/tty.usbserial-A5069RR4": 115200,
		"/dev/cu.usbmodem2022_04191":  0,
		"/dev/ttyACM0":                0,
		"COM3":                        0,
	} {
		if got := replayGuardBaud(path, 115200); got != want {
			t.Errorf("replayGuardBaud(%q) = %d, want %d", path, got, want)
		}
	}
}

// Rate alone never drops: a fast burst of distinct lines all passes.
func TestReplayGuardKeepsFastDistinctData(t *testing.T) {
	var chunks []scriptChunk
	var want []byte
	for i := 0; i < 400; i++ {
		line := []byte(fmt.Sprintf("[26-11-06 17:51:%02d.%06d] line %d\r\n", i%60, i*997, i))
		chunks = append(chunks, scriptChunk{data: line, dt: 10 * time.Microsecond})
		want = append(want, line...)
	}
	raw, _ := runScript(t, chunks)
	if !bytes.Equal(raw, want) {
		t.Errorf("passed %d bytes, want %d (distinct data dropped)", len(raw), len(want))
	}
}
