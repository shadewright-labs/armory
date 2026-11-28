package waitart

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWrapFlushesBufferedOutput wraps a buffered writer: a frame must never land between bytes the
// buffer has passed on and bytes it still holds.
func TestWrapFlushesBufferedOutput(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	bw := bufio.NewWriterSize(&term, 4)
	out := p.Wrap(bw)
	for _, s := range []string{"abc", "def\n", "ghi\n"} {
		if _, err := io.WriteString(out, s); err != nil {
			t.Fatal(err)
		}
	}
	p.Stop()
	_ = bw.Flush()
	got := term.String()
	for _, l := range []string{"abcdef\n", "ghi\n"} {
		if !strings.Contains(got, l) {
			t.Fatalf("line %q was split by a frame: %q", l, got)
		}
	}
	if i := strings.Index(got, "ghi\n"); !strings.Contains(got[i:], "Waiting") && strings.Count(got, "Waiting") < 3 {
		t.Fatalf("frames were not drawn after the flushed lines: %q", got)
	}
}

// TestCleanupWaitsForSplitRune leaves the output inside a two-byte rune, then stops the player or
// narrows the terminal. No byte of the player may land between the two halves of the rune.
func TestCleanupWaitsForSplitRune(t *testing.T) {
	for _, via := range []string{"stop", "narrow"} {
		t.Run(via, func(t *testing.T) {
			var cols atomic.Int64
			cols.Store(80)
			cfg := Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) { return int(cols.Load()), true }}
			var term fakeTerm
			p := cfg.Start(t.Context(), &term, still(t, "relay"), "Waiting")
			out := p.Wrap(&term)
			_, _ = out.Write([]byte("caf\xc3"))
			if via == "stop" {
				p.Stop()
			} else {
				cols.Store(Width - 1)
			}
			_, _ = out.Write([]byte("\xa9\n"))
			p.Stop()
			got := term.String()
			if !strings.Contains(got, "café\n") {
				t.Fatalf("the player wrote inside a split rune: %q", got)
			}
			if !strings.HasSuffix(got, showCursor) {
				t.Fatalf("cursor not shown once the rune was complete: %q", got)
			}
		})
	}
}

type cancelWriter struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (c cancelWriter) Write(b []byte) (int, error) {
	c.cancel()
	return c.w.Write(b)
}

// TestNoFrameAfterCancel cancels the context from inside a wrapped write that ends a line: no frame
// may follow it.
func TestNoFrameAfterCancel(t *testing.T) {
	var term fakeTerm
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := onTerminal(nil).Start(ctx, &term, still(t, "relay"), "Waiting")
	_, _ = io.WriteString(p.Wrap(cancelWriter{w: &term, cancel: cancel}), "line\n")
	<-p.done
	got := term.String()
	i := strings.Index(got, "line\n")
	if i < 0 {
		t.Fatalf("output line lost: %q", got)
	}
	if after := got[i:]; strings.Contains(after, "Waiting") {
		t.Fatalf("a frame was drawn after the context ended: %q", after)
	}
}

// shortOnCall writes only keep bytes of call number short (counting from 1) and fails it; every other
// call is written whole.
type shortOnCall struct {
	fakeTerm
	mu    sync.Mutex
	calls int
	short int
	keep  int
}

func (s *shortOnCall) Write(b []byte) (int, error) {
	s.mu.Lock()
	s.calls++
	cut := s.calls == s.short
	s.mu.Unlock()
	if !cut {
		return s.fakeTerm.Write(b)
	}
	n := min(s.keep, len(b))
	_, _ = s.fakeTerm.Write(b[:n])
	return n, io.ErrShortWrite
}

type resizeWriter struct {
	w    io.Writer
	cols *atomic.Int64
}

func (r resizeWriter) Write(b []byte) (int, error) {
	r.cols.Store(Width - 1)
	return r.w.Write(b)
}

// TestResizeChecks narrows the terminal during a wrapped write, or just before Stop: the player must
// neither draw nor clear with the old geometry.
func TestResizeChecks(t *testing.T) {
	for _, via := range []string{"during write", "before stop"} {
		t.Run(via, func(t *testing.T) {
			var cols atomic.Int64
			cols.Store(80)
			cfg := Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) { return int(cols.Load()), true }}
			var term fakeTerm
			sc := still(t, "relay")
			p := cfg.Start(t.Context(), &term, sc, "Waiting")
			mark := len(term.String())
			if via == "during write" {
				_, _ = io.WriteString(p.Wrap(resizeWriter{w: &term, cols: &cols}), "line\n")
			} else {
				cols.Store(Width - 1)
			}
			p.Stop()
			after := term.String()[mark:]
			if i := strings.Index(after, "line\n"); i >= 0 {
				after = after[i:] // the clear before the write ran at the old, valid width
			}
			if strings.Contains(after, "Waiting") || strings.Contains(after, clearSeq(sc.Height())) {
				t.Fatalf("drew or cleared on a narrowed terminal: %q", after)
			}
			if !strings.Contains(after, showCursor) {
				t.Fatalf("cursor not shown again: %q", after)
			}
		})
	}
}

// TestAmbiguousRunesCountTwice checks that East Asian ambiguous runes, which some terminals draw two
// columns wide, take two cells in the status line, the ellipsis included, so the line never wraps.
func TestAmbiguousRunesCountTwice(t *testing.T) {
	ambiguous := map[rune]bool{}
	for _, r := range "°éαЖ\u2014…€─█●" {
		ambiguous[r] = true
	}
	for _, status := range []string{strings.Repeat("°", 62), "Status: é α Ж \u2014 € ─ █ ● " + strings.Repeat("x", 80)} {
		cells := statusCells(&DefaultStyle, 0, status)
		if len(cells) > Width {
			t.Fatalf("status takes %d cells", len(cells))
		}
		for i, c := range cells {
			if ambiguous[c.r] && (i+1 >= len(cells) || cells[i+1].r != wideTail) {
				t.Fatalf("ambiguous rune %q at cell %d counted as one column", c.r, i)
			}
		}
	}
	for r := range ambiguous {
		if _, w := statusRune(r); w != 2 {
			t.Errorf("%q takes %d columns, want 2", r, w)
		}
	}
}

const (
	envLang    = "LANG"
	localeUTF8 = "en_US.UTF-8"
)

// TestNonUTF8LocaleDrawsNothing checks that a locale that is not UTF-8 turns the art off, since an
// 8-bit terminal would read the continuation bytes of a rune such as U+00DB as C1 controls.
func TestNonUTF8LocaleDrawsNothing(t *testing.T) {
	for _, tc := range []struct {
		vars map[string]string
		want bool
	}{
		{nil, true},
		{map[string]string{envLang: localeUTF8}, true},
		{map[string]string{envLang: "pl_PL.utf8"}, true},
		{map[string]string{envLang: "en_US.ISO-8859-1"}, false},
		{map[string]string{envLang: "C"}, false},
		{map[string]string{envLang: localeUTF8, "LC_CTYPE": "POSIX"}, false},
		{map[string]string{envLang: "C", "LC_ALL": "C.UTF-8"}, true},
		{map[string]string{"LC_ALL": "de_DE@euro", envLang: localeUTF8}, false},
	} {
		t.Run(fmt.Sprint(tc.vars), func(t *testing.T) {
			if got := onTerminal(tc.vars).Enabled(&fakeTerm{}); got != tc.want {
				t.Fatalf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLookupReturnsCopies changes a scene from Lookup and from ForKind: the set must not change.
func TestLookupReturnsCopies(t *testing.T) {
	s, err := NewSet(Builtin().Scenes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Lookup("relay")
	want := a.Frames[0].Delay
	a.Frames[0].Delay = want + 1
	a.Palette['w'] = Colour{ANSI: 1}
	b := s.ForKind("any", nil)
	b.Frames[0].Rows[0] = Row{}
	list := s.Scenes()
	list[1].Frames[0].Delay = want + 2
	got, _ := s.Lookup("relay")
	if got.Frames[0].Delay != want || got.Palette['w'].ANSI == 1 {
		t.Fatal("changing a scene from Lookup or Scenes changed the set")
	}
	if first := s.ForKind("any", nil); first.Frames[0].Rows[0] == (Row{}) {
		t.Fatal("changing a scene from ForKind changed the set")
	}
	if !errors.Is(got.Validate(), nil) {
		t.Fatal("scene from the set no longer valid")
	}
}

// cutTerm is a terminal that writes only keep bytes of the first call that contains on and fails that
// call, then works again. Everything that reaches it is recorded.
type cutTerm struct {
	fakeTerm
	mu2  sync.Mutex
	on   string
	keep int
}

func (c *cutTerm) Write(b []byte) (int, error) {
	c.mu2.Lock()
	cut := c.on != "" && strings.Contains(string(b), c.on)
	if cut {
		c.on = ""
	}
	c.mu2.Unlock()
	if !cut {
		return c.fakeTerm.Write(b)
	}
	n := min(c.keep, len(b))
	_, _ = c.fakeTerm.Write(b[:n])
	return n, io.ErrShortWrite
}

// TestFailedFlushMidRuneGoesInert lets a buffered writer accept a line and then fail its flush halfway
// through a rune. The player must write nothing more, not even on Stop, though the terminal recovers.
func TestFailedFlushMidRuneGoesInert(t *testing.T) {
	term := cutTerm{on: "café", keep: len("caf\xc3")}
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	out := p.Wrap(bufio.NewWriterSize(&term, 64))
	if _, err := io.WriteString(out, "café\n"); err == nil {
		t.Fatal("the failed flush was not reported")
	}
	mark := len(term.String())
	p.Stop()
	if after := term.String()[mark:]; after != "" {
		t.Fatalf("the player wrote after a failed flush: %q", after)
	}
	if !strings.HasSuffix(term.String(), "caf\xc3") {
		t.Fatalf("the cut output is not last: %q", term.String())
	}
}

// TestShortCursorShowIsNotRetried cuts the cursor show inside its escape sequence. The player must not
// send the rest later, where it would follow (and corrupt) real output.
func TestShortCursorShowIsNotRetried(t *testing.T) {
	// Calls: 1 hide, 2 first frame, 3 clear, 4 cursor show, cut inside "\x1b[?25h".
	cut := reset + "\x1b[?25"
	w := &shortOnCall{short: 4, keep: len(cut)}
	p := onTerminal(nil).Start(t.Context(), w, still(t, "relay"), "Waiting")
	p.Stop()
	p.Stop()
	_, _ = io.WriteString(p.Wrap(w), "Done\n")
	if got := w.String(); !strings.HasSuffix(got, cut+"Done\n") {
		t.Fatalf("the player wrote after a short cursor show: %q", got[max(0, len(got)-30):])
	}
}

// TestStopWithBufferedSplitRune stops the player while buffered output ends inside a rune, then
// completes the rune through the same buffered wrapper: the cursor show must follow the whole rune.
func TestStopWithBufferedSplitRune(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	out := p.Wrap(bufio.NewWriterSize(&term, 64))
	_, _ = out.Write([]byte("caf\xc3"))
	p.Stop()
	_, _ = out.Write([]byte("\xa9\n"))
	got := term.String()
	if !strings.Contains(got, "café\n") || !strings.HasSuffix(got, showOnly) {
		t.Fatalf("the cursor show did not wait for the buffered rune: %q", got)
	}
}

// TestInvalidByteDoesNotHoldCursor ends the output with a byte that starts no UTF-8 sequence: the
// cursor show must not wait for continuation bytes that never come.
func TestInvalidByteDoesNotHoldCursor(t *testing.T) {
	for _, tail := range []string{"\xff", "\xf8", "\xc0", "\xe0\x80"} {
		var term fakeTerm
		p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
		_, _ = io.WriteString(p.Wrap(&term), "abc"+tail)
		p.Stop()
		if got := term.String(); !strings.HasSuffix(got, tail+showOnly) {
			t.Fatalf("tail %q: cursor not shown at once: %q", tail, got[max(0, len(got)-20):])
		}
	}
}

// within fails the test when f does not return within five seconds, the sign of a deadlock.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: deadlocked", what)
	}
}

// TestNestedWrapIsRefused wraps a writer that the same player's Wrap made. Every write through it must
// fail with ErrNestedWrap, reach no writer and leave the player as it was: a normal Stop afterwards
// still clears the frame and shows the cursor.
func TestNestedWrapIsRefused(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	mark := len(term.String())
	within(t, "nested wrap", func() {
		out := p.Wrap(p.Wrap(&term))
		for _, part := range []string{"line one\n", "\xe2", "\x82", ""} {
			if n, err := io.WriteString(out, part); n != 0 || !errors.Is(err, ErrNestedWrap) {
				t.Errorf("write %q: got %d, %v, want 0, ErrNestedWrap", part, n, err)
			}
		}
		if got := term.String()[mark:]; got != "" {
			t.Errorf("a refused write reached the terminal: %q", got)
		}
		p.Stop()
	})
	want := clearSeq(scene(t, "relay").Height()) + showCursor
	if got := term.String()[mark:]; got != want {
		t.Fatalf("Stop after refused writes wrote %q, want %q", got, want)
	}
}

// gateWriter blocks the first write that contains hold until gate is closed, and records the most
// writes it was ever in at once.
type gateWriter struct {
	fakeTerm
	hold    string
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
	in      atomic.Int64
	most    atomic.Int64
}

func (g *gateWriter) Write(b []byte) (int, error) {
	n := g.in.Add(1)
	defer g.in.Add(-1)
	for {
		m := g.most.Load()
		if n <= m || g.most.CompareAndSwap(m, n) {
			break
		}
	}
	if strings.Contains(string(b), g.hold) {
		g.once.Do(func() { close(g.entered) })
		<-g.gate
	}
	return g.fakeTerm.Write(b)
}

// TestWrappedWritesNeverOverlap blocks one wrapped write inside its writer while another goroutine
// writes through a second wrapped writer, a third calls Stop and frames are due. Nothing may reach the
// terminal until the blocked write returns: no caller bytes, no frame, no clear, no cursor show.
func TestWrappedWritesNeverOverlap(t *testing.T) {
	g := &gateWriter{hold: "first", entered: make(chan struct{}), gate: make(chan struct{})}
	p := onTerminal(nil).Start(t.Context(), g, fast(t, "relay"), "Waiting")
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.WriteString(p.Wrap(g), "first\n") })
	<-g.entered
	mark := len(g.String())
	wg.Go(func() { _, _ = io.WriteString(p.Wrap(g), "second\n") })
	wg.Go(p.Stop)
	time.Sleep(50 * time.Millisecond)
	during := g.String()[mark:]
	close(g.gate)
	within(t, "writes after the gate", wg.Wait)
	if during != "" {
		t.Fatalf("bytes reached the terminal while a wrapped write was in progress: %q", during)
	}
	if m := g.most.Load(); m != 1 {
		t.Fatalf("%d writes were in the writer at once, want 1", m)
	}
}

// TestConcurrentWritesThroughSharedBuffer writes lines from several goroutines through one wrapped
// buffered writer, which is not safe for concurrent use on its own. Each write must reach the terminal
// whole and once, with frames drawn in between.
func TestConcurrentWritesThroughSharedBuffer(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, fast(t, "relay"), "Waiting")
	out := p.Wrap(bufio.NewWriterSize(&term, 16))
	const workers, lines = 8, 40
	var wg sync.WaitGroup
	for g := range workers {
		wg.Go(func() {
			for i := range lines {
				_, _ = fmt.Fprintf(out, "line %02d-%03d\n", g, i)
			}
		})
	}
	within(t, "concurrent writes", wg.Wait)
	p.Stop()
	got := term.String()
	for g := range workers {
		for i := range lines {
			if n := strings.Count(got, fmt.Sprintf("line %02d-%03d\n", g, i)); n != 1 {
				t.Fatalf("line %02d-%03d reached the terminal %d times", g, i, n)
			}
		}
	}
}

// failEmpty fails every zero-length write and passes the rest.
type failEmpty struct{ w io.Writer }

func (f failEmpty) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errors.New("terminal went away")
	}
	return f.w.Write(b)
}

// TestFailedEmptyWriteGoesInert fails a zero-length wrapped write: the player must go inert like after
// any other failed write, so Stop writes nothing.
func TestFailedEmptyWriteGoesInert(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	if _, err := p.Wrap(failEmpty{&term}).Write(nil); err == nil {
		t.Fatal("the failed empty write was not reported")
	}
	mark := len(term.String())
	p.Stop()
	if after := term.String()[mark:]; after != "" {
		t.Fatalf("the player wrote after a failed empty write: %q", after)
	}
}

// narrowOn narrows the terminal when it is given a write that contains seq.
type narrowOn struct {
	fakeTerm
	cols *atomic.Int64
	seq  string
}

func (n *narrowOn) Write(b []byte) (int, error) {
	if strings.Contains(string(b), n.seq) {
		n.cols.Store(Width - 1)
	}
	return n.fakeTerm.Write(b)
}

// TestWidthCheckedAfterPlayerWrites narrows the terminal while it takes the cursor hide at start, the
// clear before a redraw, or a wrapped write with no line break: no frame may follow, and the cursor
// comes back at once.
func TestWidthCheckedAfterPlayerWrites(t *testing.T) {
	const viaHide, viaClear, viaPartial = "hide", "clear", "partial line"
	for _, via := range []string{viaHide, viaClear, viaPartial} {
		t.Run(via, func(t *testing.T) {
			var cols atomic.Int64
			cols.Store(80)
			cfg := Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) { return int(cols.Load()), true }}
			sc := still(t, "relay")
			w := &narrowOn{cols: &cols, seq: "\x00never"}
			switch via {
			case viaHide:
				w.seq = hideCursor
			case viaClear:
				w.seq = clearSeq(sc.Height())
				sc = fast(t, "relay")
			case viaPartial:
				w.seq = "progress"
			}
			p := cfg.Start(t.Context(), w, sc, "Waiting")
			defer p.Stop()
			switch via {
			case viaClear:
				deadline := time.Now().Add(5 * time.Second)
				for cols.Load() >= Width && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				time.Sleep(20 * time.Millisecond) // let further ticks pass
			case viaPartial:
				_, _ = io.WriteString(p.Wrap(w), "progress")
			}
			got := w.String()
			after := got[strings.Index(got, w.seq)+len(w.seq):]
			if strings.Contains(after, "Waiting") {
				t.Fatalf("a frame was drawn after the terminal narrowed: %q", after)
			}
			if !strings.Contains(after, showOnly) {
				t.Fatalf("cursor not shown once the terminal narrowed: %q", after)
			}
		})
	}
}

// TestCancelJustBeforeFrameWrite ends the context after the frame is rendered, during the last width
// check before the write: the frame must not be written.
func TestCancelJustBeforeFrameWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int64
	cfg := Config{LookupEnv: env(nil), Terminal: func(io.Writer) (int, bool) {
		// Call 1 is Enabled; call 2 is the check just before the first frame is written.
		if calls.Add(1) == 2 {
			cancel()
		}
		return 80, true
	}}
	var term fakeTerm
	p := cfg.Start(ctx, &term, still(t, "relay"), "Waiting")
	p.Stop()
	if got := term.String(); strings.Contains(got, "Waiting") {
		t.Fatalf("a frame was written after the context ended: %q", got)
	}
}

// panicOn panics on the first write that contains seq, and records every other write.
type panicOn struct {
	fakeTerm
	seq  string
	once sync.Once
}

func (w *panicOn) Write(b []byte) (int, error) {
	hit := false
	if strings.Contains(string(b), w.seq) {
		w.once.Do(func() { hit = true })
	}
	if hit {
		panic("terminal writer failed")
	}
	return w.fakeTerm.Write(b)
}

// recovered runs f and reports whether it panicked.
func recovered(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
}

// TestWriterPanicReleasesLock makes the terminal writer panic on the cursor show during Stop, or a
// writer passed to Wrap panic, and recovers at the call site. The lock must be free afterwards, so a
// wrapped write and a Stop from another goroutine return, and the player must be inert: it writes
// nothing more of its own.
func TestWriterPanicReleasesLock(t *testing.T) {
	for _, via := range []string{"cursor show", "wrapped write"} {
		t.Run(via, func(t *testing.T) {
			w := &panicOn{seq: showOnly}
			if via == "wrapped write" {
				w.seq = "boom"
			}
			p := onTerminal(nil).Start(t.Context(), w, still(t, "relay"), "Waiting")
			if via == "cursor show" {
				if !recovered(p.Stop) {
					t.Fatal("the cursor show did not panic")
				}
			} else if !recovered(func() { _, _ = io.WriteString(p.Wrap(w), "boom\n") }) {
				t.Fatal("the wrapped write did not panic")
			}
			mark := len(w.String())
			within(t, "calls after a panic", func() {
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = io.WriteString(p.Wrap(w), "after\n")
					p.Stop()
				}()
				<-done
			})
			if got := w.String()[mark:]; got != "after\n" {
				t.Fatalf("after the panic the terminal got %q, want only the caller's bytes", got)
			}
		})
	}
}

// TestStopAfterEmptyWriteClearsFrame makes a zero-length wrapped write while a frame is on the screen:
// a normal Stop afterwards must still clear the frame and show the cursor.
func TestStopAfterEmptyWriteClearsFrame(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	if !strings.Contains(term.String(), "Waiting") {
		t.Fatal("no frame on the screen")
	}
	mark := len(term.String())
	if _, err := p.Wrap(&term).Write(nil); err != nil {
		t.Fatalf("empty write: %v", err)
	}
	p.Stop()
	want := clearSeq(scene(t, "relay").Height()) + showCursor
	if got := term.String()[mark:]; got != want {
		t.Fatalf("Stop after an empty write wrote %q, want %q", got, want)
	}
}

// TestDeepNestedWrapIsRefused wraps a refused writer again, as in p.Wrap(p.Wrap(p.Wrap(term))). The
// outer Wrap must refuse it too: the write fails with ErrNestedWrap, reaches no writer and leaves the
// frame on the screen, so Stop writes exactly the clear and the cursor show.
func TestDeepNestedWrapIsRefused(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	mark := len(term.String())
	within(t, "deep nested wrap", func() {
		out := p.Wrap(p.Wrap(p.Wrap(&term)))
		if n, err := io.WriteString(out, "line\n"); n != 0 || !errors.Is(err, ErrNestedWrap) {
			t.Errorf("write: got %d, %v, want 0, ErrNestedWrap", n, err)
		}
		p.Stop()
	})
	want := clearSeq(scene(t, "relay").Height()) + showCursor
	if got := term.String()[mark:]; got != want {
		t.Fatalf("Stop after a deep nested write wrote %q, want %q", got, want)
	}
}

// TestGoexitInTerminalReleasesLock makes the Terminal callback call runtime.Goexit during a frame
// tick. The frame loop's goroutine ends, and Stop must still return: the tick gives up the lock.
func TestGoexitInTerminalReleasesLock(t *testing.T) {
	var armed atomic.Bool
	fired := make(chan struct{})
	cfg := Config{
		LookupEnv: env(nil),
		Terminal: func(io.Writer) (int, bool) {
			if armed.CompareAndSwap(true, false) {
				close(fired)
				runtime.Goexit()
			}
			return 80, true
		},
	}
	var term fakeTerm
	p := cfg.Start(t.Context(), &term, fast(t, "relay"), "Waiting")
	armed.Store(true)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame tick called Terminal")
	}
	within(t, "Stop after Goexit in Terminal", p.Stop)
}
