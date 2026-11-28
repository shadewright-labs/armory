package waitart

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

// TestNestedWrapDoesNotDeadlock wraps a buffered writer that itself writes through the same player.
func TestNestedWrapDoesNotDeadlock(t *testing.T) {
	var term fakeTerm
	p := onTerminal(nil).Start(t.Context(), &term, still(t, "relay"), "Waiting")
	done := make(chan struct{})
	go func() {
		defer close(done)
		out := p.Wrap(bufio.NewWriter(p.Wrap(&term)))
		_, _ = io.WriteString(out, "line one\n")
		_, _ = io.WriteString(out, "line two\n")
		p.Stop()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nested wrapped writers deadlocked")
	}
	got := term.String()
	if !strings.Contains(got, "line one\nline two\n") || !strings.HasSuffix(got, showOnly) {
		t.Fatalf("nested output lost or cursor not shown: %q", got)
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
