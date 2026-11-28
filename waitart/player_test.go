package waitart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	envColumns = "COLUMNS"
	envNoColor = "NO_COLOR"
	sgrTrue    = "\x1b[38;2;"
	sgr256     = "\x1b[38;5;"
)

// fakeTerm is a terminal writer for tests: it records every byte.
type fakeTerm struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (f *fakeTerm) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(b)
}

func (f *fakeTerm) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

// onTerminal is a Config that sees any writer as an 80-column terminal and reads only the given
// environment.
func onTerminal(vars map[string]string) Config {
	return Config{
		LookupEnv: env(vars),
		Terminal:  func(io.Writer) (int, bool) { return 80, true },
	}
}

func scene(t testing.TB, name string) Scene {
	t.Helper()
	sc, ok := Builtin().Lookup(name)
	if !ok {
		t.Fatalf("no built-in scene %q", name)
	}
	return sc
}

// withDelay returns a copy of a built-in scene whose frames all stay for d.
func withDelay(t testing.TB, name string, d time.Duration) Scene {
	t.Helper()
	sc := scene(t, name)
	sc.Frames = slices.Clone(sc.Frames)
	for i := range sc.Frames {
		sc.Frames[i].Delay = d
	}
	return sc
}

// fast returns a scene whose frames change every millisecond.
func fast(t testing.TB, name string) Scene { return withDelay(t, name, time.Millisecond) }

// still returns a scene whose frames never change during a test, so only the line writer draws.
func still(t testing.TB, name string) Scene { return withDelay(t, name, time.Minute) }

func TestOffCasesDrawNothing(t *testing.T) {
	notTerm := func(io.Writer) (int, bool) { return 0, false }
	narrow := func(io.Writer) (int, bool) { return Width - 1, true }
	unknown := func(io.Writer) (int, bool) { return 0, true }
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	bad := scene(t, "relay")
	bad.Frames = slices.Clone(bad.Frames)
	bad.Frames[0].Rows = []Row{{Text: "\x1b[2J", Mask: "    "}}
	for _, tc := range []struct {
		name string
		cfg  Config
		sc   Scene
		ctx  context.Context
	}{
		{name: "zero Config", cfg: Config{Terminal: unknown}},
		{name: "no LookupEnv", cfg: Config{Terminal: func(io.Writer) (int, bool) { return 80, true }}},
		{name: "not a terminal", cfg: Config{LookupEnv: env(nil), Terminal: notTerm}},
		{name: "default check on a plain writer", cfg: Config{LookupEnv: env(nil)}},
		{name: "terminal narrower than a frame", cfg: Config{LookupEnv: env(nil), Terminal: narrow}},
		{
			name: "unknown width, narrow COLUMNS",
			cfg:  Config{LookupEnv: env(map[string]string{envColumns: "40"}), Terminal: unknown},
		},
		{name: envNoColor, cfg: onTerminal(map[string]string{envNoColor: "1"})},
		{name: "TERM=dumb", cfg: onTerminal(map[string]string{"TERM": "dumb"})},
		{name: "caller opt-out", cfg: func() Config { c := onTerminal(nil); c.Off = true; return c }()},
		{name: "invalid scene", cfg: onTerminal(nil), sc: bad},
		{name: "context already ended", cfg: onTerminal(nil), ctx: cancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc, ctx := tc.sc, tc.ctx
			if sc.Name == "" {
				sc = fast(t, "relay")
			}
			if ctx == nil {
				ctx = t.Context()
			}
			var w fakeTerm
			if tc.sc.Name == "" && tc.ctx == nil && tc.cfg.Enabled(&w) {
				t.Fatal("Enabled = true, want false")
			}
			p := tc.cfg.Start(ctx, &w, sc, "Waiting")
			if p != nil {
				p.Stop()
				t.Fatal("Start returned a player, want nil")
			}
			out := p.Wrap(&w)
			_, _ = fmt.Fprintln(out, "real output")
			p.SetStatus("ignored")
			p.Stop()
			if got := w.String(); got != "real output\n" {
				t.Fatalf("writer got %q, want only the real output", got)
			}
		})
	}
}

func TestOnCases(t *testing.T) {
	unknown := func(io.Writer) (int, bool) { return 0, true }
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"plain terminal", onTerminal(nil)},
		{"empty NO_COLOR", onTerminal(map[string]string{envNoColor: ""})},
		{"TERM=xterm-256color", onTerminal(map[string]string{"TERM": "xterm-256color"})},
		{"unknown width", Config{LookupEnv: env(nil), Terminal: unknown}},
		{"unknown width, wide COLUMNS", Config{LookupEnv: env(map[string]string{envColumns: "100"}), Terminal: unknown}},
		{"unknown width, bad COLUMNS", Config{LookupEnv: env(map[string]string{envColumns: "wide"}), Terminal: unknown}},
		{"exactly Width columns", Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) { return Width, true }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w fakeTerm
			p := tc.cfg.Start(t.Context(), &w, fast(t, "hourglass"), "Waiting")
			if p == nil {
				t.Fatal("Start returned nil, want a player")
			}
			p.Stop()
			if !strings.Contains(w.String(), "Waiting") {
				t.Fatalf("no frame drawn: %q", w.String())
			}
		})
	}
}

func TestStopIsIdempotentAndRestoresCursor(t *testing.T) {
	var w fakeTerm
	sc := fast(t, "hourglass")
	p := onTerminal(nil).Start(t.Context(), &w, sc, "Building")
	time.Sleep(10 * time.Millisecond)
	p.Stop()
	p.Stop()
	out := w.String()
	if !strings.HasPrefix(out, hideCursor) {
		t.Fatalf("output does not start by hiding the cursor: %q", out[:min(len(out), 20)])
	}
	if !strings.HasSuffix(out, clearSeq(sc.Height())+showCursor) {
		t.Fatalf("output does not end by clearing the frame and showing the cursor: %q", out[max(0, len(out)-40):])
	}
	if n := strings.Count(out, showCursor); n != 1 {
		t.Fatalf("cursor shown %d times, want 1", n)
	}
	time.Sleep(5 * time.Millisecond)
	if w.String() != out {
		t.Fatal("the player wrote after Stop")
	}
	var nilPlayer *Player
	nilPlayer.Stop()
}

// TestContextEndRestoresCursor cancels the context, as signal.NotifyContext does on Ctrl-C, and checks
// that the player clears the frame and shows the cursor without a call to Stop.
func TestContextEndRestoresCursor(t *testing.T) {
	var w fakeTerm
	ctx, cancel := context.WithCancel(t.Context())
	sc := fast(t, "wave")
	p := onTerminal(nil).Start(ctx, &w, sc, "Waiting for approval")
	cancel()
	<-p.done
	if !strings.HasSuffix(w.String(), clearSeq(sc.Height())+showCursor) {
		t.Fatalf("frame not cleared and cursor not restored after the context ended: %q", w.String())
	}
	p.Stop() // the caller's deferred Stop still returns
	if n := strings.Count(w.String(), showCursor); n != 1 {
		t.Fatalf("cursor shown %d times, want 1", n)
	}
}

// TestOutputLinesAreNeverSplit writes lines in pieces through Wrap while the player redraws every
// millisecond on the same terminal, and checks that each line reaches the terminal in one piece, with
// no frame byte inside it.
func TestOutputLinesAreNeverSplit(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, fast(t, "relay"), "Sending")
	out := p.Wrap(&w)
	lines := make([]string, 0, 150)
	for i := range 150 {
		parts := []string{fmt.Sprintf("item-%03d ", i), "(result) ", "body text", "\n"}
		lines = append(lines, strings.Join(parts, ""))
		for _, part := range parts {
			if _, err := io.WriteString(out, part); err != nil {
				t.Fatal(err)
			}
			if i%10 == 0 {
				time.Sleep(time.Millisecond) // let frames land between writes
			}
		}
	}
	p.Stop()
	stream := w.String()
	if strings.Count(stream, "Sending") < 2 {
		t.Fatal("too few frames drawn for the test to mean anything")
	}
	for _, l := range lines {
		if !strings.Contains(stream, l) {
			t.Fatalf("line %q was split or lost", l)
		}
	}
	// Every frame starts at the start of a line: right after a newline, a clear, or the hidden cursor
	// at the very start. A draw begins with a carriage return that is not the one inside a clear (after
	// the cursor-up, before an erase) and not the one of a row's \r\n.
	checked := 0
	for i := range len(stream) {
		if stream[i] != '\r' || (i > 0 && stream[i-1] == 'A') || strings.HasPrefix(stream[i+1:], "\x1b[J") ||
			strings.HasPrefix(stream[i+1:], "\n") {
			continue
		}
		checked++
		before := stream[:i]
		if !strings.HasSuffix(before, "\n") && !strings.HasSuffix(before, "\x1b[J") && before != hideCursor {
			t.Fatalf("frame starts mid-line after %q", before[max(0, len(before)-30):])
		}
	}
	if checked < 2 {
		t.Fatalf("checked %d frame starts, want at least 2", checked)
	}
	_, _ = fmt.Fprint(out, "after")
	if !strings.HasSuffix(w.String(), showCursor+"after") {
		t.Fatal("output after Stop was not passed through")
	}
}

func TestPartialLineHoldsFrames(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, fast(t, "wave"), "Waiting")
	out := p.Wrap(&w)
	_, _ = fmt.Fprint(out, "Question: ")
	mark := len(w.String())
	time.Sleep(20 * time.Millisecond)
	if got := w.String()[mark:]; got != "" {
		t.Fatalf("frames drawn while a line was unfinished: %q", got)
	}
	_, _ = fmt.Fprintln(out, "answer")
	p.Stop()
	if !strings.Contains(w.String(), "Question: answer\n") {
		t.Fatal("partial line was split")
	}
}

// shortWriter writes only the first n bytes of each call and then fails, like a terminal that went
// away mid-write.
type shortWriter struct {
	fakeTerm
	n   int
	err error
}

func (s *shortWriter) Write(b []byte) (int, error) {
	n := min(s.n, len(b))
	_, _ = s.fakeTerm.Write(b[:n])
	if n < len(b) {
		return n, s.err
	}
	return n, nil
}

// TestFailedWritesNeverDrawMidLine checks that the player decides whether a line is finished from the
// bytes that were written, not from the bytes the caller offered.
func TestFailedWritesNeverDrawMidLine(t *testing.T) {
	errBoom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		first     string // written whole, to leave a line unfinished (or not)
		then      string // offered second, written only in part
		keep      int    // bytes of then that get written
		wantDraws bool   // a frame may follow the second write
	}{
		{"short write of a full line", "", "hello\n", 3, false},
		{"failed write completing a line", "Question: ", "answer\n", 0, false},
		{"short write that keeps the newline out", "Q: ", "abc\n", 3, false},
		{"complete write of a line", "", "hello\n", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var term fakeTerm
			p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
			defer p.Stop()
			if tc.first != "" {
				_, _ = fmt.Fprint(p.Wrap(&term), tc.first)
			}
			mark := len(term.String())
			sw := &shortWriter{n: tc.keep, err: errBoom}
			_, _ = io.WriteString(p.Wrap(sw), tc.then)
			p.mu.Lock()
			held := p.held
			p.mu.Unlock()
			if held == tc.wantDraws {
				t.Fatalf("line held = %v, want %v", held, !tc.wantDraws)
			}
			drawn := strings.Contains(term.String()[mark:], "Waiting")
			if drawn != tc.wantDraws {
				t.Fatalf("frame drawn = %v, want %v; terminal got %q", drawn, tc.wantDraws, term.String()[mark:])
			}
		})
	}
}

// rawColumns replays a terminal stream on a terminal that does not turn a line feed into a carriage
// return and line feed (raw mode). It counts the frame rows, from a drawing carriage return to the
// next one, and the rows that start at a column other than 0.
func rawColumns(stream string) (rows, offsets int) {
	col, inFrame, fresh := 0, false, false
	rs := []rune(stream)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case 0x1b:
			j := i + 1
			if j < len(rs) && rs[j] == '[' {
				j++
				for j < len(rs) && (rs[j] < '@' || rs[j] > '~') {
					j++
				}
				if j < len(rs) && rs[j] == 'K' && fresh && col != 0 {
					offsets++
				}
				if j < len(rs) && rs[j] == 'A' {
					inFrame = false
				}
				fresh = false
			}
			i = j
		case '\r':
			col = 0
			if i > 0 && rs[i-1] != 'A' && i+1 < len(rs) && rs[i+1] != '\n' &&
				(i+3 >= len(rs) || rs[i+1] != 0x1b || rs[i+2] != '[' || rs[i+3] != 'J') {
				inFrame, fresh = true, true
			}
		case '\n':
			if inFrame {
				rows++
				fresh = true
			}
		default:
			if inFrame && fresh && col != 0 {
				offsets++
			}
			fresh = false
			_, w := statusRune(r)
			col += w
		}
	}
	return rows, offsets
}

// TestFramesStartAtColumnZero replays the stream on a terminal in raw mode, after output lines that end
// in the middle of the screen width: every frame row still starts at column 0.
func TestFramesStartAtColumnZero(t *testing.T) {
	var w fakeTerm
	sc := still(t, "hourglass")
	p := onTerminal(nil).Start(t.Context(), &w, sc, "Waiting")
	out := p.Wrap(&w)
	_, _ = fmt.Fprintln(out, "first result (1)")
	_, _ = fmt.Fprintln(out, "Second line.")
	p.Stop()
	rows, offsets := rawColumns(w.String())
	if rows < 2*sc.Height() {
		t.Fatalf("replayed %d frame rows, want at least %d", rows, 2*sc.Height())
	}
	if offsets != 0 {
		t.Fatalf("%d frame rows start away from column 0", offsets)
	}
}

// scriptedWriter lets the first full calls through whole and then writes only keep bytes of each call
// and fails.
type scriptedWriter struct {
	fakeTerm
	mu2   sync.Mutex
	calls int
	full  int
	keep  int
}

func (s *scriptedWriter) Write(b []byte) (int, error) {
	s.mu2.Lock()
	s.calls++
	short := s.calls > s.full
	s.mu2.Unlock()
	if !short {
		return s.fakeTerm.Write(b)
	}
	n := min(s.keep, len(b))
	_, _ = s.fakeTerm.Write(b[:n])
	return n, errors.New("terminal went away")
}

// TestShortFrameWriteDisablesRedraws checks that a frame that did not reach the screen whole turns
// redraws off and is never cleared with the cursor-up move, which would eat the lines above it.
func TestShortFrameWriteDisablesRedraws(t *testing.T) {
	// Call 1 is the cursor hide; call 2 is the first frame, cut short.
	w := &scriptedWriter{full: 1, keep: 10}
	sc := still(t, "relay")
	p := onTerminal(nil).Start(t.Context(), w, sc, "Waiting")
	_, _ = fmt.Fprint(p.Wrap(w), "line\n")
	p.Stop()
	out := w.String()
	if strings.Contains(out, clearSeq(sc.Height())) {
		t.Fatalf("the incomplete frame was cleared with a cursor-up move: %q", out)
	}
	if strings.Contains(out, "Waiting") {
		t.Fatalf("a frame was drawn after the failed write: %q", out)
	}
	if !strings.Contains(out, showCursor) {
		t.Fatalf("the cursor was not shown again: %q", out)
	}
}

// TestFrameWriteFailureOnRedraw covers a failure after a good first frame.
func TestFrameWriteFailureOnRedraw(t *testing.T) {
	// hide, first frame, then a short clear: nothing more is drawn.
	w := &scriptedWriter{full: 2, keep: 3}
	sc := still(t, "relay")
	p := onTerminal(nil).Start(t.Context(), w, sc, "Waiting")
	_, _ = fmt.Fprint(p.Wrap(w), "line\n")
	before := strings.Count(w.String(), "Waiting")
	_, _ = fmt.Fprint(p.Wrap(w), "again\n")
	p.Stop()
	if got := strings.Count(w.String(), "Waiting"); got != before {
		t.Fatalf("frames drawn after a failed write: %d, want %d", got, before)
	}
	if n := strings.Count(w.String(), clearSeq(sc.Height())); n > 1 {
		t.Fatalf("clear repeated after a failed write: %d", n)
	}
}

// flakyWriter fails the numbered calls (counting from 1) and passes the rest.
type flakyWriter struct {
	*fakeTerm
	mu        sync.Mutex
	calls     int
	failCalls map[int]bool
}

func (f *flakyWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	f.calls++
	fail := f.failCalls[f.calls]
	f.mu.Unlock()
	if fail {
		return 0, errors.New("terminal went away")
	}
	return f.fakeTerm.Write(b)
}

// TestStopShowsCursorAfterFailedShowInDisable covers a frame write that fails together with the cursor
// show inside disable, on a writer that recovers before Stop: Stop must still restore the cursor.
func TestStopShowsCursorAfterFailedShowInDisable(t *testing.T) {
	var term fakeTerm
	w := &flakyWriter{fakeTerm: &term, failCalls: map[int]bool{2: true, 3: true}}
	p := onTerminal(nil).Start(t.Context(), w, still(t, "relay"), "Waiting")
	p.Stop()
	if out := term.String(); !strings.HasSuffix(out, showCursor) {
		t.Fatalf("output does not end with the show-cursor sequence: %q", out)
	}
}

// TestNarrowedTerminalStopsDrawing checks that the width is read again before each frame: once the
// terminal is narrower than a frame the player draws nothing and does not clear with geometry that no
// longer holds.
func TestNarrowedTerminalStopsDrawing(t *testing.T) {
	for _, via := range []string{"wrap", "ticker"} {
		t.Run(via, func(t *testing.T) {
			var cols atomic.Int64
			cols.Store(80)
			cfg := Config{
				LookupEnv: env(nil),
				Terminal:  func(io.Writer) (int, bool) { return int(cols.Load()), true },
			}
			var term fakeTerm
			sc := still(t, "relay")
			if via == "ticker" {
				sc = fast(t, "relay")
			}
			p := cfg.Start(t.Context(), &term, sc, "Waiting")
			mark := len(term.String())
			cols.Store(Width - 1)
			if via == "wrap" {
				_, _ = fmt.Fprint(p.Wrap(&term), "line\n")
			} else {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					p.mu.Lock()
					off := p.disabled
					p.mu.Unlock()
					if off {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			p.Stop()
			after := term.String()[mark:]
			if strings.Contains(after, clearSeq(sc.Height())) || strings.Contains(after, "Waiting") {
				t.Fatalf("drew or cleared on a narrowed terminal: %q", after)
			}
			if !strings.Contains(after, showCursor) {
				t.Fatalf("cursor not shown again: %q", after)
			}
		})
	}
}

func TestSetStatusShowsOnNextFrame(t *testing.T) {
	var w fakeTerm
	p := onTerminal(nil).Start(t.Context(), &w, still(t, "wave"), "first")
	p.SetStatus("second: detail")
	_, _ = fmt.Fprintln(p.Wrap(&w), "line")
	p.Stop()
	if !strings.Contains(w.String(), "second") {
		t.Fatalf("new status not drawn: %q", w.String())
	}
}

func TestColourModes(t *testing.T) {
	for _, tc := range []struct {
		colorterm, want, not string
	}{
		{"", sgr256, sgrTrue},
		{"truecolor", sgrTrue, sgr256},
		{"24bit", sgrTrue, sgr256},
	} {
		t.Run("COLORTERM="+tc.colorterm, func(t *testing.T) {
			var w fakeTerm
			p := onTerminal(map[string]string{"COLORTERM": tc.colorterm}).Start(t.Context(), &w, still(t, "relay"), "Waiting")
			p.Stop()
			if out := w.String(); !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
				t.Fatalf("colour codes: want %q and no %q in %q", tc.want, tc.not, out)
			}
		})
	}
}

func TestCustomStyle(t *testing.T) {
	var w fakeTerm
	cfg := onTerminal(nil)
	cfg.Style = &Style{Spinner: Colour{ANSI: 201}, Label: Colour{ANSI: 202}, Detail: Colour{ANSI: 203}}
	p := cfg.Start(t.Context(), &w, still(t, "relay"), "Label: detail")
	p.Stop()
	for _, code := range []string{"\x1b[38;5;201m", "\x1b[38;5;202mLabel", "\x1b[38;5;203m: detail"} {
		if !strings.Contains(w.String(), code) {
			t.Errorf("missing %q in %q", code, w.String())
		}
	}
}

func TestFileTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	if charDevice(f) {
		t.Error("a regular file counts as a terminal")
	}
	_ = f.Close()
	if charDevice(f) {
		t.Error("a closed file counts as a terminal")
	}
	var nilFile *os.File
	if charDevice(nilFile) {
		t.Error("a nil *os.File counts as a terminal")
	}
	if charDevice(&fakeTerm{}) {
		t.Error("a writer that is not a file counts as a terminal")
	}
	if runtime.GOOS == "windows" {
		return
	}
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no null device")
	}
	defer func() { _ = dev.Close() }()
	if !charDevice(dev) {
		t.Error("the null device does not count as a character device")
	}
}
