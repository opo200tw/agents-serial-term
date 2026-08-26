// Pure-function regression tests for serial-term. These cover the
// parsing / rendering invariants that are easy to silently break during
// refactor and that the post-merge audit on PR #333 had no automated
// guard for.
package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    mode
		wantErr bool
	}{
		{"ascii", modeASCII, false},
		{"a", modeASCII, false},
		{"HEX", modeHex, false}, // case-insensitive
		{"h", modeHex, false},
		{"mixed", modeMixed, false},
		{"hexdump", modeMixed, false}, // alias
		{"m", modeMixed, false},
		{"", 0, true},
		{"nope", 0, true},
	}
	for _, c := range cases {
		got, err := parseMode(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseMode(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("parseMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseEOL(t *testing.T) {
	cases := []struct {
		in      string
		want    []byte
		wantErr bool
	}{
		{"none", nil, false},
		{"", nil, false}, // empty aliased to "none"
		{"cr", []byte{'\r'}, false},
		{"CR", []byte{'\r'}, false}, // case-insensitive
		{"lf", []byte{'\n'}, false},
		{"crlf", []byte{'\r', '\n'}, false},
		{"bogus", nil, true},
	}
	for _, c := range cases {
		got, err := parseEOL(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseEOL(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if err == nil && !bytes.Equal(got, c.want) {
			t.Errorf("parseEOL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// formatEOL and markerForEOL must be the inverse of parseEOL for every
// name in eolSpecs — this guards the stringly-typed drift that motivated
// the single-table refactor.
func TestEOLRoundtrip(t *testing.T) {
	for _, spec := range eolSpecs {
		b, err := parseEOL(spec.name)
		if err != nil {
			t.Errorf("parseEOL(%q) unexpected error: %v", spec.name, err)
			continue
		}
		if !bytes.Equal(b, spec.bytes) {
			t.Errorf("parseEOL(%q) = %v, want %v", spec.name, b, spec.bytes)
		}
		if got := formatEOL(b); got != spec.name {
			t.Errorf("formatEOL(%v) = %q, want %q", b, got, spec.name)
		}
		if got := markerForEOL(b); got != spec.marker {
			t.Errorf("markerForEOL(%v) = %q, want %q", b, got, spec.marker)
		}
	}
}

func TestRenderASCII(t *testing.T) {
	cases := []struct {
		in   byte
		want string
	}{
		{'A', "A"},
		{' ', " "},
		{0x7E, "~"},    // last printable
		{0x7F, `\x7f`}, // DEL is not printable
		{0x00, `\x00`}, // null
		{0x0D, "<CR>"}, // CR marker
		{0x0A, "<LF>"}, // LF marker
		{0x09, `\t`},   // tab
		{0x1F, `\x1f`}, // control
		{0x80, `\x80`}, // high-bit
	}
	for _, c := range cases {
		if got := renderASCII(c.in); got != c.want {
			t.Errorf("renderASCII(%#x) = %q, want %q", c.in, got, c.want)
		}
	}
}

// hex mode's 16-byte accumulator was a bug before PR #333's /simplify
// pass — a 2-byte RX burst like `d<LF>` would sit invisibly until the
// next chunk pushed it past 16. Verify short-burst flush happens at the
// end of every handleRx call.
func TestRenderHexFlushShortBurst(t *testing.T) {
	var buf bytes.Buffer
	st := &state{
		mode: modeHex,
		sink: lineSink{rxOut: &buf, txOut: &buf, sysOut: &buf},
	}
	st.handleRx([]byte{'d', '\n'}) // 2 bytes, well under 16
	out := buf.String()
	if !strings.Contains(out, "< 64 0a") {
		t.Errorf("short hex burst not flushed; got %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("rendered line not newline-terminated: %q", out)
	}
}

// hex mode with a 16-byte aligned burst must flush exactly one line with
// no dangling bytes in the accumulator.
func TestRenderHexAlignedBurst(t *testing.T) {
	var buf bytes.Buffer
	st := &state{
		mode: modeHex,
		sink: lineSink{rxOut: &buf, txOut: &buf, sysOut: &buf},
	}
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(i)
	}
	st.handleRx(data)
	lines := strings.Count(buf.String(), "\n")
	if lines != 1 {
		t.Errorf("16-byte burst should produce exactly 1 line, got %d: %q", lines, buf.String())
	}
	if st.asciiBuf.Len() != 0 {
		t.Errorf("asciiBuf should be empty after aligned flush, got %q", st.asciiBuf.String())
	}
}

// ASCII mode: CR / LF / CRLF must each flush exactly one line with the
// correct marker, and CRLF must NOT produce two lines.
func TestRenderASCIIEOLMarkers(t *testing.T) {
	cases := []struct {
		in          string
		wantMarker  string
		wantPayload string
	}{
		{"hello\r\n", "<CRLF>", "hello"},
		{"hello\n", "<LF>", "hello"},
		{"hello\r", "<CR>", "hello"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		st := &state{
			mode: modeASCII,
			sink: lineSink{rxOut: &buf, txOut: &buf, sysOut: &buf},
		}
		st.handleRx([]byte(c.in))
		out := buf.String()
		if strings.Count(out, "\n") != 1 {
			t.Errorf("%q produced %d lines, want 1: %q", c.in, strings.Count(out, "\n"), out)
		}
		if !strings.Contains(out, c.wantMarker) {
			t.Errorf("%q missing marker %s: %q", c.in, c.wantMarker, out)
		}
		if !strings.Contains(out, c.wantPayload) {
			t.Errorf("%q missing payload %q: %q", c.in, c.wantPayload, out)
		}
	}
}

// safeClose must not panic on double-close. This guards the /quit race
// fixed by commit eea8ff63.
func TestSafeCloseIdempotent(t *testing.T) {
	ch := make(chan struct{})
	safeClose(ch)
	safeClose(ch)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			safeClose(ch)
		}()
	}
	wg.Wait()
}
