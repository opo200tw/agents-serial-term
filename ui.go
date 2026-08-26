package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"go.bug.st/serial"
)

// runUI brings up the tview layout:
//
//	┌─ status (1 line) ──────────────────────────────────────┐
//	│  port=...  mode=...  tx-eol=...  log=...  text-log=... │
//	├─ RX (large, auto-scroll) ──────────────────────────────┤
//	│  < ...                                                 │
//	│  < ...                                                 │
//	├─ TX history (smaller) ─────────────────────────────────┤
//	│  > PING<LF>                                            │
//	├─ input (1 line) ───────────────────────────────────────┤
//	│  > _                                                   │
//	└────────────────────────────────────────────────────────┘
//
// Slash commands work in the input field. Ctrl-C / /quit exits.
func runUI(st *state, port serial.Port, logF interface {
	Write(p []byte) (int, error)
}, h *stdinHandler, stop chan struct{}, done chan struct{},
	portPath string, baud int, logPath, textLogPath string) {

	app := tview.NewApplication()

	rxView := tview.NewTextView()
	rxView.SetDynamicColors(false).
		SetScrollable(true).
		SetChangedFunc(func() { app.Draw() }).
		SetBorder(true).
		SetTitle(" RX (device → host) ")

	txView := tview.NewTextView()
	txView.SetDynamicColors(false).
		SetScrollable(true).
		SetChangedFunc(func() { app.Draw() }).
		SetBorder(true).
		SetTitle(" TX (host → device) ")

	sysView := tview.NewTextView()
	sysView.SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() { app.Draw() }).
		SetBorder(true).
		SetTitle(" log / system ")

	status := tview.NewTextView()
	status.SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)

	options := tview.NewTextView()
	options.SetDynamicColors(true).
		SetBorder(true).
		SetTitle(" options ")
	options.SetText(strings.TrimSpace(`
[yellow]display modes[-]   /hex  /ascii  /mixed
[yellow]ts[-]              /ts
[yellow]tx EOL[-]           /eol none|cr|lf|crlf
[yellow]send raw bytes[-]  /hex-send 50 49 4e 47 0a
[yellow]loopback bench[-]  /bench [sec [chunk]]   /bench-stop
[yellow]help[-]            /help
[yellow]quit[-]            /quit  or  Ctrl-C
[yellow]any other line[-]  sent to device + tx-eol
`))

	input := tview.NewInputField()
	input.SetLabel(" > ").
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetFieldTextColor(tcell.ColorWhite)

	statusText := func() string {
		st.mu.Lock()
		mode := st.mode
		ts := st.showTS
		st.mu.Unlock()
		logTxt := logPath
		if logTxt == "" {
			logTxt = "(none)"
		}
		textLogTxt := textLogPath
		if textLogTxt == "" {
			textLogTxt = "(none)"
		}
		return fmt.Sprintf(
			" [yellow]port[-]=%s  [yellow]baud[-]=%d  [yellow]mode[-]=%s  [yellow]tx-eol[-]=%s  [yellow]ts[-]=%v  [yellow]--log[-]=%s  [yellow]--text-log[-]=%s",
			portPath, baud, mode, formatEOL(h.txEOL), ts, logTxt, textLogTxt)
	}
	// Initial paint without app — safe before Run.
	status.SetText(statusText())
	updateStatus := func() {
		// Always spawn a goroutine: QueueUpdateDraw blocks until the UI loop
		// processes it. Calling it from the UI goroutine itself (e.g. from
		// the input field's Done handler) would deadlock.
		go app.QueueUpdateDraw(func() { status.SetText(statusText()) })
	}

	// Wire TextViews as state sinks. Writes from background goroutines must
	// be safe; tview.TextView.Write is goroutine-safe (it queues a draw).
	st.sink = lineSink{rxOut: rxView, txOut: txView, sysOut: sysView}
	st.onChange = updateStatus

	// Pre-fill sysView with banner / file paths so they're visible immediately;
	// these writes are safe before Run because TextView.Write does not require
	// the Application loop.
	fmt.Fprintf(sysView, "--- session start: %s @ %d, mode=%s, tx-eol=%s ---\n",
		portPath, baud, st.mode, formatEOL(h.txEOL))
	if textLogPath != "" {
		fmt.Fprintf(sysView, "--- text-log: %s ---\n", textLogPath)
	}
	if logPath != "" {
		fmt.Fprintf(sysView, "--- raw log: %s ---\n", logPath)
	}
	// Note: text-log session-start banner is already written by main.go when
	// it opens textLogF — do not duplicate it here.

	input.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := input.GetText()
		input.SetText("")
		if h.handleLine(text) {
			app.Stop()
		}
	})

	// Layout: status (1), main (flex), input (1)
	// Main: left = RX (3) | right = (TX (1) over sys (1) over options (1))
	rightCol := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(txView, 0, 1, false).
		AddItem(sysView, 0, 1, false).
		AddItem(options, 11, 0, false)

	main := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(rxView, 0, 2, false).
		AddItem(rightCol, 0, 1, false)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(status, 1, 0, false).
		AddItem(main, 0, 1, false).
		AddItem(input, 1, 0, true)

	// Spawn the reader goroutine; its writes land in rxView via st.sink.
	// errSink routes port errors to the sys pane instead of stderr (hidden
	// under tview's alternate screen).
	var logWriter io.Writer
	if logF != nil {
		logWriter = logF
	}
	go readerLoop(port, logWriter, st, func(e error) { st.sysMsg(fmt.Sprintf("[reader] %v", e)) }, stop, done)

	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyCtrlC {
			app.Stop()
			return nil
		}
		return ev
	})

	// Guard against tview/tcell panics. A panic inside Run() would
	// otherwise bypass main()'s shutdown path, losing the text-log's
	// trailing buffer and leaving the terminal in raw mode.
	runErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				// Put tcell back in cooked mode before we touch stderr.
				app.Stop()
				err = fmt.Errorf("tview panic: %v", r)
			}
		}()
		return app.SetRoot(root, true).EnableMouse(true).Run()
	}()
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "ui: %v\n", runErr)
		if st.textLog != nil {
			_ = st.textLog.Sync()
		}
	}
	safeClose(stop)
}

// formatEOL lives in main.go alongside parseEOL so both use the same
// eolSpecs table — no duplicate lookup logic here.
