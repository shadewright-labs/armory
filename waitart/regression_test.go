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

// TestStopRetriesShortCursorShow lets the terminal take only the colour reset of the cursor show
// during Stop: a second Stop must send the rest.
func TestStopRetriesShortCursorShow(t *testing.T) {
	// Calls: 1 hide, 2 first frame, 3 clear, 4 cursor show cut after the reset.
	w := &shortOnCall{short: 4, keep: len(reset)}
	p := onTerminal(nil).Start(t.Context(), w, still(t, "relay"), "Waiting")
	p.Stop()
	p.Stop()
	if got := w.String(); !strings.HasSuffix(got, showCursor) {
		t.Fatalf("the cursor show was not completed: %q", got[max(0, len(got)-30):])
	}
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
	for _, r := range "°éαЖ—…€─█●" {
		ambiguous[r] = true
	}
	for _, status := range []string{strings.Repeat("°", 62), "Status: é α Ж — € ─ █ ● " + strings.Repeat("x", 80)} {
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
