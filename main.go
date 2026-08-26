// serial-term is a cross-platform serial terminal for debug use.
//
// RX: three display modes (ascii / hex / mixed hexdump), EOL markers in ASCII
// mode so device's CR / LF / CRLF choice is visible, optional timestamps and
// byte-for-byte --log capture.
//
// TX: line-buffered stdin. Each line is either a local `/command` or a
// payload sent to the device with the configured --tx-eol. This keeps the
// tool usable both interactively and from pipes / scripts.
//
// Output is designed to be both human-readable and machine-grep-friendly;
// RX and TX share the same rendered stream with `<` / `>` prefixes.
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"go.bug.st/serial"
)

type mode int

const (
	modeASCII mode = iota
	modeHex
	modeMixed
)

func (m mode) String() string {
	switch m {
	case modeASCII:
		return "ascii"
	case modeHex:
		return "hex"
	case modeMixed:
		return "mixed"
	}
	return "?"
}

func parseMode(s string) (mode, error) {
	switch strings.ToLower(s) {
	case "a", "ascii":
		return modeASCII, nil
	case "h", "hex":
		return modeHex, nil
	case "m", "mixed", "hexdump":
		return modeMixed, nil
	}
	return 0, fmt.Errorf("unknown mode %q (use ascii/hex/mixed)", s)
}

// eolSpec is the single source of truth for EOL handling: the flag name
// (`--tx-eol` / `/eol` arg), the bytes sent on the wire, and the display
// marker rendered in RX / TX panes. Keeping these three in one table
// avoids the drift risk flagged in the tri-vote (previously duplicated
// across parseEOL / formatEOL / recordTx marker-switch).
type eolSpec struct {
	name   string
	bytes  []byte
	marker string // shown after the line in ASCII / TX rendering
}

var eolSpecs = []eolSpec{
	{name: "none", bytes: nil, marker: ""},
	{name: "cr", bytes: []byte{'\r'}, marker: "<CR>"},
	{name: "lf", bytes: []byte{'\n'}, marker: "<LF>"},
	{name: "crlf", bytes: []byte{'\r', '\n'}, marker: "<CRLF>"},
}

func parseEOL(s string) ([]byte, error) {
	key := strings.ToLower(s)
	if key == "" {
		key = "none"
	}
	for _, e := range eolSpecs {
		if e.name == key {
			return e.bytes, nil
		}
	}
	return nil, fmt.Errorf("unknown tx-eol %q (use none/cr/lf/crlf)", s)
}

// formatEOL is the inverse of parseEOL: bytes → flag name.
func formatEOL(b []byte) string {
	for _, e := range eolSpecs {
		if bytes.Equal(e.bytes, b) {
			return e.name
		}
	}
	return "?"
}

// markerForEOL returns the display marker for a given EOL byte sequence,
// or "" if the bytes don't match any known EOL (shouldn't happen with
// validated --tx-eol input).
func markerForEOL(b []byte) string {
	for _, e := range eolSpecs {
		if bytes.Equal(e.bytes, b) {
			return e.marker
		}
	}
	return ""
}

// lineSink is where rendered lines go. In headless mode both rxOut/txOut/sysOut
// point at os.Stdout; in TUI mode they point at separate tview TextView writers.
type lineSink struct {
	rxOut  io.Writer
	txOut  io.Writer
	sysOut io.Writer
}

type state struct {
	mu         sync.Mutex
	mode       mode
	showTS     bool
	bytesIn    uint64
	bytesOut   uint64
	linesOut   uint64
	asciiBuf   strings.Builder
	hexLineOff uint64
	textLog    *os.File // mirrors every rendered line, includes timestamp
	sink       lineSink
	onChange   func() // called after mode/eol/ts change so UI can repaint status

	// Bench state. benchRxActive gates handleRx: while true, RX bytes go
	// to the sequence verifier instead of the UI render path. benchTxActive
	// gates the TX goroutine. They flip at different times so that after
	// TX stops, in-flight RX bytes keep draining into the bench counters
	// (not into the UI) during the grace window.
	benchRxActive   atomic.Bool
	benchTxActive   atomic.Bool
	benchTxBytes    atomic.Uint64
	benchRxBytes    atomic.Uint64
	benchMismatches atomic.Uint64
	benchExpected   uint64 // RX only; bench is single-reader so no lock needed
	benchFirstMism  atomic.Uint64
}

func (s *state) setMode(m mode) {
	s.mu.Lock()
	if s.mode == m {
		s.mu.Unlock()
		return
	}
	s.flushPartialLocked()
	s.mode = m
	cb := s.onChange
	s.writeSysLocked(fmt.Sprintf("--- mode=%s ---", m))
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (s *state) toggleTS() {
	s.mu.Lock()
	s.showTS = !s.showTS
	cb := s.onChange
	s.writeSysLocked(fmt.Sprintf("--- timestamps=%v ---", s.showTS))
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (s *state) writeSysLocked(msg string) {
	if s.sink.sysOut != nil {
		fmt.Fprintln(s.sink.sysOut, msg)
	}
	if s.textLog != nil {
		fmt.Fprintln(s.textLog, time.Now().Format("2006-01-02 15:04:05.000 ")+msg)
		// No Sync(): tail -f reads via the page cache and doesn't need it.
		// We fsync on shutdown in main(); per-line Sync at 300 KB/s is
		// thousands of fsync calls/sec and will stall the RX loop.
	}
}

// sysMsg is the lock-acquiring wrapper for writeSysLocked, called from
// stdinHandler / UI input field where the caller does not already hold mu.
func (s *state) sysMsg(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeSysLocked(msg)
}

// flushPartialLocked emits any pending ASCII partial line. Called under mu.
func (s *state) flushPartialLocked() {
	if s.asciiBuf.Len() > 0 {
		s.writeLineLocked("<", s.asciiBuf.String(), "")
		s.asciiBuf.Reset()
	}
}

// writeLineLocked writes one rendered line with optional direction/meta.
// dir is "<" for RX, ">" for TX, "" for system messages. Routes the line to
// the correct sink (rxOut / txOut / sysOut) and mirrors to text-log if set.
func (s *state) writeLineLocked(dir, payload, eolMarker string) {
	s.linesOut++
	var prefix string
	if s.showTS {
		prefix = time.Now().Format("15:04:05.000 ")
	}
	dirField := ""
	if dir != "" {
		dirField = dir + " "
	}
	var line string
	if eolMarker != "" {
		line = prefix + dirField + payload + eolMarker + "\n"
	} else {
		line = prefix + dirField + payload + "\n"
	}
	var w io.Writer
	switch dir {
	case "<":
		w = s.sink.rxOut
	case ">":
		w = s.sink.txOut
	default:
		w = s.sink.sysOut
	}
	if w != nil {
		io.WriteString(w, line)
	}
	if s.textLog != nil {
		// text-log always carries a full date+time regardless of stdout
		// --ts state, so the file is self-contained for post-mortem.
		var datePrefix string
		if s.showTS {
			datePrefix = time.Now().Format("2006-01-02 ")
		} else {
			datePrefix = time.Now().Format("2006-01-02 15:04:05.000 ")
		}
		io.WriteString(s.textLog, datePrefix+line)
		// Intentionally no Sync: per-line fsync at 300 KB/s is catastrophic
		// for RX throughput. main() fsyncs on shutdown.
	}
}

func renderASCII(b byte) string {
	switch {
	case b == 0x0D:
		return "<CR>"
	case b == 0x0A:
		return "<LF>"
	case b == 0x09:
		return `\t`
	case b >= 0x20 && b < 0x7F:
		return string(rune(b))
	default:
		return fmt.Sprintf(`\x%02x`, b)
	}
}

func (s *state) handleRx(p []byte) {
	if s.benchRxActive.Load() {
		s.consumeBenchRx(p)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytesIn += uint64(len(p))
	switch s.mode {
	case modeASCII:
		s.renderASCIILocked(p)
	case modeHex:
		s.renderHexLocked(p)
	case modeMixed:
		s.renderMixedLocked(p)
	}
}

// consumeBenchRx verifies each byte matches the rolling (seq & 0xff) pattern
// and increments counters. Called only from the reader goroutine, so
// benchExpected is not locked.
func (s *state) consumeBenchRx(p []byte) {
	for _, got := range p {
		want := byte(s.benchExpected & 0xff)
		if got != want {
			n := s.benchMismatches.Add(1)
			if n == 1 {
				s.benchFirstMism.Store(s.benchExpected)
			}
			// Resync on the observed byte so one real loss isn't amplified
			// into cascading mismatches on every subsequent byte.
			s.benchExpected = uint64(got) + 1
		} else {
			s.benchExpected++
		}
	}
	s.benchRxBytes.Add(uint64(len(p)))
}

func (s *state) renderASCIILocked(p []byte) {
	i := 0
	for i < len(p) {
		b := p[i]
		if b == 0x0D && i+1 < len(p) && p[i+1] == 0x0A {
			s.writeLineLocked("<", s.asciiBuf.String(), "<CRLF>")
			s.asciiBuf.Reset()
			i += 2
			continue
		}
		if b == 0x0D || b == 0x0A {
			marker := "<CR>"
			if b == 0x0A {
				marker = "<LF>"
			}
			s.writeLineLocked("<", s.asciiBuf.String(), marker)
			s.asciiBuf.Reset()
			i++
			continue
		}
		s.asciiBuf.WriteString(renderASCII(b))
		i++
	}
}

func (s *state) renderHexLocked(p []byte) {
	for _, b := range p {
		if s.asciiBuf.Len() > 0 {
			s.asciiBuf.WriteByte(' ')
		}
		fmt.Fprintf(&s.asciiBuf, "%02x", b)
		if (s.asciiBuf.Len()+1)/3 >= 16 {
			s.writeLineLocked("<", s.asciiBuf.String(), "")
			s.asciiBuf.Reset()
		}
	}
	// Flush a partial (<16 byte) line at the end of each RX burst so small
	// inputs like `d<LF>` (2 bytes) show up immediately instead of being
	// stuck in the 16-byte accumulator until more data arrives.
	if s.asciiBuf.Len() > 0 {
		s.writeLineLocked("<", s.asciiBuf.String(), "")
		s.asciiBuf.Reset()
	}
}

func (s *state) renderMixedLocked(p []byte) {
	for len(p) > 0 {
		take := 16
		if len(p) < 16 {
			take = len(p)
		}
		chunk := p[:take]
		p = p[take:]
		dump := hex.Dump(chunk)
		for _, l := range strings.Split(strings.TrimRight(dump, "\n"), "\n") {
			if len(l) < 9 {
				continue
			}
			adjusted := fmt.Sprintf("%08x%s", s.hexLineOff, l[8:])
			s.writeLineLocked("<", adjusted, "")
			s.hexLineOff += 16
		}
	}
}

// recordTx logs a sent payload to the display with a `>` prefix.
func (s *state) recordTx(payload []byte, eol []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytesOut += uint64(len(payload) + len(eol))
	var b strings.Builder
	for _, c := range payload {
		b.WriteString(renderASCII(c))
	}
	s.writeLineLocked(">", b.String(), markerForEOL(eol))
}

// listPorts returns all serial ports the OS reports (go.bug.st/serial handles
// macOS /dev/cu.*, Linux /dev/ttyACM*/ttyUSB*, Windows COM<n>).
//
// On macOS every USB modem appears twice — /dev/cu.<X> (call-out) and
// /dev/tty.<X> (call-in, blocks waiting for DCD). We drop tty.* when a cu.*
// counterpart exists so the picker doesn't show each device twice.
func listPorts() ([]string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	cuSuffixes := make(map[string]bool)
	for _, p := range ports {
		if strings.HasPrefix(p, "/dev/cu.") {
			cuSuffixes[strings.TrimPrefix(p, "/dev/cu.")] = true
		}
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if strings.HasPrefix(p, "/dev/tty.") && cuSuffixes[strings.TrimPrefix(p, "/dev/tty.")] {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// selectPort returns a chosen serial port path. Selection rules:
//   - 0 ports   → error
//   - 1 port    → auto-select, print notice to stderr
//   - N ports   → interactive prompt on stdin/stderr; if stdin is not a tty
//     (pipe/background), error out with the list so caller can pass PORT.
func selectPort() (string, error) {
	ports, err := listPorts()
	if err != nil {
		return "", fmt.Errorf("list ports: %w", err)
	}
	if len(ports) == 0 {
		return "", errors.New("no serial ports found")
	}
	if len(ports) == 1 {
		fmt.Fprintf(os.Stderr, "serial-term: using sole detected port %s\n", ports[0])
		return ports[0], nil
	}
	if !isTerminal(os.Stdin) {
		return "", fmt.Errorf("multiple serial ports detected; pass PORT explicitly (or use --list-ports).\nDetected: %s",
			strings.Join(ports, ", "))
	}
	fmt.Fprintln(os.Stderr, "serial ports:")
	for i, p := range ports {
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, p)
	}
	fmt.Fprintf(os.Stderr, "select [1-%d]: ", len(ports))
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", errors.New("stdin closed before selection")
	}
	choice := strings.TrimSpace(scanner.Text())
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(ports) {
		return "", fmt.Errorf("bad selection %q", choice)
	}
	return ports[n-1], nil
}

// readerLoop pulls bytes off the port until stop is closed. errSink receives
// any non-EOF, non-shutdown error so the caller can route it to the UI sys
// pane, stderr, or wherever makes sense.
func readerLoop(p serial.Port, logF io.Writer, st *state, errSink func(error), stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	// If a bench was in progress when the port went away (USB unplug, cable
	// yank), clear both bench flags so a subsequent /bench doesn't get
	// "already running" forever on the leaked TX-active state.
	defer func() {
		if st.benchTxActive.Load() || st.benchRxActive.Load() {
			st.benchTxActive.Store(false)
			st.benchRxActive.Store(false)
			st.sysMsg("--- bench aborted: reader exited (port gone?) ---")
		}
	}()
	buf := make([]byte, 1024)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, err := p.Read(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if errSink != nil {
				errSink(err)
			}
			return
		}
		if n == 0 {
			continue
		}
		data := buf[:n]
		if logF != nil {
			_, _ = logF.Write(data)
			// No Sync here — main() syncs on shutdown.
		}
		st.handleRx(data)
	}
}

type stdinHandler struct {
	st    *state
	port  serial.Port
	txEOL []byte
	stop  chan struct{}
}

func (h *stdinHandler) run() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<14), 1<<20) // 1 MiB max line
	for scanner.Scan() {
		line := scanner.Text()
		if h.handleLine(line) {
			return // /quit
		}
	}
	// EOF on stdin: do NOT close the connection; the user may have piped in
	// a command script and still want to observe subsequent device output.
	<-h.stop
}

func (h *stdinHandler) handleLine(line string) bool {
	if !strings.HasPrefix(line, "/") {
		return h.sendPayload([]byte(line))
	}
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	switch strings.ToLower(cmd) {
	case "h", "hex":
		h.st.setMode(modeHex)
	case "a", "ascii":
		h.st.setMode(modeASCII)
	case "m", "mixed":
		h.st.setMode(modeMixed)
	case "ts", "t":
		h.st.toggleTS()
	case "eol":
		if arg == "" {
			h.st.sysMsg(fmt.Sprintf("--- tx-eol=%q ---", string(h.txEOL)))
			return false
		}
		eol, err := parseEOL(arg)
		if err != nil {
			h.st.sysMsg(fmt.Sprintf("--- %v ---", err))
			return false
		}
		h.txEOL = eol
		h.st.sysMsg(fmt.Sprintf("--- tx-eol=%s ---", arg))
		if h.st.onChange != nil {
			h.st.onChange()
		}
	case "hex-send", "x":
		raw, err := hex.DecodeString(strings.Join(strings.Fields(arg), ""))
		if err != nil {
			h.st.sysMsg(fmt.Sprintf("--- bad hex: %v ---", err))
			return false
		}
		return h.sendRaw(raw, nil)
	case "bench":
		// /bench [seconds] [chunk_bytes]
		dur := 5.0
		chunk := 4096
		fields := strings.Fields(arg)
		if len(fields) >= 1 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil && v > 0 {
				dur = v
			}
		}
		if len(fields) >= 2 {
			if v, err := strconv.Atoi(fields[1]); err == nil && v > 0 {
				chunk = v
			}
		}
		h.startBench(time.Duration(dur*float64(time.Second)), chunk)
	case "bench-stop":
		h.stopBench()
	case "quit", "q", "exit":
		// safeClose: if a SIGINT/SIGTERM handler already closed the channel,
		// a bare close() here would panic. The signal goroutine and /quit
		// both race to shut down, either path may win.
		safeClose(h.stop)
		return true
	case "help", "?", "":
		h.st.sysMsg(`--- commands:
    /hex | /h          switch display to hex
    /ascii | /a        switch display to ASCII with EOL markers
    /mixed | /m        switch display to hexdump -C style
    /ts | /t           toggle timestamps on every line
    /eol [MODE]        get/set TX EOL (none/cr/lf/crlf)
    /hex-send HEX      send raw bytes (no EOL), e.g. /hex-send 01 02 ff
    /bench [SEC [CHK]] loopback throughput test (short TX↔RX first).
                       default 5s / 4096 B chunks
    /bench-stop        abort a running /bench
    /quit | /q         exit
    any other line     sent to the device + tx-eol
---`)
	default:
		h.st.sysMsg(fmt.Sprintf("--- unknown /%s (/help for list) ---", cmd))
	}
	return false
}

func (h *stdinHandler) sendPayload(p []byte) bool {
	return h.sendRaw(p, h.txEOL)
}

func (h *stdinHandler) sendRaw(p, eol []byte) bool {
	if h.st.benchRxActive.Load() || h.st.benchTxActive.Load() {
		h.st.sysMsg("--- bench in progress; TX ignored (use /bench-stop to abort) ---")
		return false
	}
	wire := make([]byte, 0, len(p)+len(eol))
	wire = append(wire, p...)
	wire = append(wire, eol...)
	if _, err := h.port.Write(wire); err != nil {
		// Surface to the visible sys pane (TUI) or stdout (headless) so the
		// user sees it. Do NOT exit the UI: the port may recover (USB
		// reconnect), or the user may want to inspect the prior session
		// state before quitting manually.
		h.st.sysMsg(fmt.Sprintf("[tx] %v", err))
		return false
	}
	h.st.recordTx(p, eol)
	return false
}

// startBench spawns a loopback throughput run. TX goroutine writes a rolling
// byte pattern; the shared reader feeds bytes into consumeBenchRx via
// handleRx while benchRxActive is set. After the TX duration, the RX flag
// stays on for an extra grace period so in-flight bytes drain into the
// bench counters instead of polluting the UI.
func (h *stdinHandler) startBench(duration time.Duration, chunkSize int) {
	if !h.st.benchTxActive.CompareAndSwap(false, true) {
		h.st.sysMsg("--- bench already running; /bench-stop to abort ---")
		return
	}
	h.st.benchRxActive.Store(true)
	// Reset counters.
	h.st.benchTxBytes.Store(0)
	h.st.benchRxBytes.Store(0)
	h.st.benchMismatches.Store(0)
	h.st.benchFirstMism.Store(^uint64(0))
	h.st.benchExpected = 0

	h.st.sysMsg(fmt.Sprintf("--- bench: start  duration=%.1fs chunk=%d (RX/TX display paused) ---",
		duration.Seconds(), chunkSize))

	startedAt := time.Now()
	txDone := make(chan struct{})

	// TX goroutine: write pattern until benchTxActive is cleared.
	go func() {
		defer close(txDone)
		buf := make([]byte, chunkSize)
		seq := uint64(0)
		for h.st.benchTxActive.Load() {
			for i := range buf {
				buf[i] = byte(seq & 0xff)
				seq++
			}
			n, err := h.port.Write(buf)
			if err != nil {
				h.st.sysMsg(fmt.Sprintf("--- bench: tx error: %v ---", err))
				h.st.benchTxActive.Store(false)
				return
			}
			h.st.benchTxBytes.Add(uint64(n))
		}
	}()

	// Timer goroutine: stop TX after duration, drain RX, publish results.
	go func() {
		t := time.NewTimer(duration)
		defer t.Stop()
		select {
		case <-t.C:
		case <-txDone: // tx errored out; bail early
		}
		h.st.benchTxActive.Store(false)
		<-txDone // make sure TX goroutine has exited
		// Drain in-flight RX into bench counters before flipping back to
		// the normal render path. 500ms is well above the typical UART
		// queue depth at any reasonable baud.
		time.Sleep(500 * time.Millisecond)
		h.st.benchRxActive.Store(false)

		elapsed := time.Since(startedAt).Seconds()
		tx := h.st.benchTxBytes.Load()
		rx := h.st.benchRxBytes.Load()
		mm := h.st.benchMismatches.Load()
		fmPos := h.st.benchFirstMism.Load()
		rxBps := float64(rx) / elapsed
		note := ""
		switch {
		case rx == 0:
			note = "  NOTE: rx=0 — loopback not wired? (short TX↔RX)"
		case mm > 0:
			note = fmt.Sprintf("  first mismatch @ expected pos %d", fmPos)
		case tx != rx:
			note = fmt.Sprintf("  tx-rx=%d (in-flight beyond drain window)", int64(tx)-int64(rx))
		}
		h.st.sysMsg(fmt.Sprintf(
			"--- bench: end  tx=%d  rx=%d  mm=%d  t=%.2fs  rx=%.3f MB/s%s ---",
			tx, rx, mm, elapsed, rxBps/(1024*1024), note))
	}()
}

func (h *stdinHandler) stopBench() {
	if !h.st.benchTxActive.Load() && !h.st.benchRxActive.Load() {
		h.st.sysMsg("--- bench not running ---")
		return
	}
	h.st.benchTxActive.Store(false)
	h.st.sysMsg("--- bench: stop requested ---")
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: serial-term [flags] [PORT]

flags:
  --baud N        baud rate (default 115200)
  --mode M        initial display: ascii | hex | mixed (default mixed)
  --log FILE      append raw received bytes (byte-for-byte, binary)
  --text-log FILE append rendered display lines with full date+time stamp
                  (text, ideal for tail -f / grep / archival)
  --ts            start stdout with HH:MM:SS.mmm prefix on each line
  --tx-eol E      append to each TX line: none | cr | lf | crlf (default lf)
  --list-ports    list detected serial ports and exit

interactive commands on stdin (type then Enter):
  /hex /ascii /mixed     switch display mode
  /ts                    toggle timestamps
  /eol [MODE]            get/set TX EOL
  /hex-send HEX...       send raw bytes (no EOL appended)
  /quit                  exit
  any other line         sent to the device + tx-eol

PORT:
  macOS    /dev/cu.usbmodem*       (CDC-ACM: Arduino, bare USB devices)
           /dev/cu.usbserial-*     (FTDI FT232, CP210x, CH34x, ...)
  Linux    /dev/ttyACM<n>          (CDC-ACM)
           /dev/ttyUSB<n>          (FTDI / CP210x / CH34x / PL2303)
  Windows  COM<n>                  (all)
  If omitted: single detected port auto-selected; on multiple, shows an
  interactive picker on stdin. Use --list-ports to just print the list
  (serial.GetPortsList covers everything the OS reports — the patterns
  above are only examples, not a whitelist).`)
}

func main() {
	baud := flag.Int("baud", 115200, "")
	modeStr := flag.String("mode", "mixed", "")
	logPath := flag.String("log", "", "")
	textLogPath := flag.String("text-log", "", "")
	ts := flag.Bool("ts", false, "")
	txEOLStr := flag.String("tx-eol", "lf", "")
	listOnly := flag.Bool("list-ports", false, "")
	flag.Usage = usage
	flag.Parse()

	if *listOnly {
		ports, err := listPorts()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if len(ports) == 0 {
			fmt.Fprintln(os.Stderr, "no serial ports found")
			os.Exit(1)
		}
		for _, p := range ports {
			fmt.Println(p)
		}
		return
	}

	portPath := flag.Arg(0)
	if portPath == "" {
		p, err := selectPort()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		portPath = p
	}

	m, err := parseMode(*modeStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	txEOL, err := parseEOL(*txEOLStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	p, err := serial.Open(portPath, &serial.Mode{BaudRate: *baud})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", portPath, err)
		// macOS FTDI driver occasionally wedges so that any tcsetattr() returns
		// EINVAL — userspace can't recover (even `stty` fails on the same call).
		// Surfacing the actionable fix saves the user a debugging detour.
		if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "invalid argument") {
			fmt.Fprintln(os.Stderr, "hint: macOS FTDI driver appears wedged — replug the USB cable to recover.")
		}
		os.Exit(2)
	}
	_ = p.SetReadTimeout(250 * time.Millisecond)

	var logF *os.File
	if *logPath != "" {
		logF, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log: %v\n", err)
			os.Exit(2)
		}
		defer logF.Close()
	}

	var textLogF *os.File
	if *textLogPath != "" {
		textLogF, err = os.OpenFile(*textLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open text-log: %v\n", err)
			os.Exit(2)
		}
		defer textLogF.Close()
		fmt.Fprintf(textLogF, "%s --- session start: %s @ %d, mode=%s, tx-eol=%s ---\n",
			time.Now().Format("2006-01-02 15:04:05.000"), portPath, *baud, m, *txEOLStr)
		_ = textLogF.Sync()
	}

	st := &state{
		mode:    m,
		showTS:  *ts,
		textLog: textLogF,
		sink:    lineSink{rxOut: os.Stdout, txOut: os.Stdout, sysOut: os.Stdout},
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	// SIGHUP can kill a backgrounded serial-term when the launching shell
	// exits (macOS sends SIGHUP to the pgroup when the session leader goes
	// away). Ignore it so `serial-term ... &` in a script survives after
	// the script returns.
	signal.Ignore(syscall.SIGHUP)
	go func() {
		<-sig
		safeClose(stop)
	}()

	useUI := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	h := &stdinHandler{st: st, port: p, txEOL: txEOL, stop: stop}

	if useUI {
		runUI(st, p, logF, h, stop, done, portPath, *baud, *logPath, *textLogPath)
	} else {
		fmt.Fprintf(os.Stdout, "serial-term: %s @ %d  mode=%s  tx-eol=%s  (/help for cmds)\n",
			portPath, *baud, m, *txEOLStr)
		go readerLoop(p, logF, st, func(e error) {
			fmt.Fprintf(os.Stderr, "[reader] %v\n", e)
		}, stop, done)
		h.run()
	}

	safeClose(stop)
	_ = p.Close()
	<-done
	// Shutdown fsync — covers every buffered line written at high rate.
	if logF != nil {
		_ = logF.Sync()
	}
	if textLogF != nil {
		_ = textLogF.Sync()
	}

	st.mu.Lock()
	st.flushPartialLocked()
	if !useUI {
		fmt.Fprintf(os.Stdout, "--- %d bytes in, %d bytes out, %d lines rendered ---\n",
			st.bytesIn, st.bytesOut, st.linesOut)
	}
	st.mu.Unlock()
}

// isTerminal uses a real isatty(3) ioctl, not just "is this a char device",
// because /dev/null is also a character device — os.ModeCharDevice would
// misclassify it as a TTY and trigger tview's `open /dev/tty` path, which
// fails ("device not configured") when the process has no controlling tty.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func safeClose(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}
