package waitart_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/shadewright-labs/armory/waitart"
)

// A command plays a scene on standard error while it waits, and sends its own output through Wrap so
// the frame never splits a line. The context from signal.NotifyContext ends on Ctrl-C, which stops the
// frames, and the deferred Stop waits until the frame is cleared and the cursor shown again. When the art is off (not a
// terminal, NO_COLOR, TERM=dumb, the caller's opt-out) Start returns nil and the same code still works.
func Example() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	noArt := false // from the command's --no-art flag
	cfg := waitart.Config{LookupEnv: os.LookupEnv, Off: noArt}
	sc := waitart.Builtin().ForKind("build", rand.NewPCG(uint64(time.Now().UnixNano()), 0))
	p := cfg.Start(ctx, os.Stderr, sc, "Building: step 1 of 3")
	defer p.Stop()

	out := p.Wrap(os.Stdout)
	_, _ = fmt.Fprintln(out, "step 1 done")
	p.SetStatus("Building: step 2 of 3")
	_, _ = fmt.Fprintln(out, "step 2 done")
	// Output:
	// step 1 done
	// step 2 done
}

// Enabled says whether a scene would be drawn. The package reads the environment only through
// LookupEnv; Terminal stands in for a real terminal check here.
func ExampleConfig_Enabled() {
	lookup := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	term := func(io.Writer) (int, bool) { return 120, true }
	for _, c := range []struct {
		name string
		cfg  waitart.Config
	}{
		{"terminal", waitart.Config{LookupEnv: lookup(nil), Terminal: term}},
		{"no LookupEnv", waitart.Config{Terminal: term}},
		{"NO_COLOR=1", waitart.Config{LookupEnv: lookup(map[string]string{"NO_COLOR": "1"}), Terminal: term}},
		{"TERM=dumb", waitart.Config{LookupEnv: lookup(map[string]string{"TERM": "dumb"}), Terminal: term}},
		{"LANG=C", waitart.Config{LookupEnv: lookup(map[string]string{"LANG": "C"}), Terminal: term}},
		{"opt-out", waitart.Config{LookupEnv: lookup(nil), Terminal: term, Off: true}},
		{"not a terminal", waitart.Config{LookupEnv: lookup(nil)}},
	} {
		fmt.Printf("%s: %v\n", c.name, c.cfg.Enabled(&bytes.Buffer{}))
	}
	// Output:
	// terminal: true
	// no LookupEnv: false
	// NO_COLOR=1: false
	// TERM=dumb: false
	// LANG=C: false
	// opt-out: false
	// not a terminal: false
}

// A Player draws the scene and clears it before each line of real output. Here the terminal is a
// buffer that a Terminal function vouches for.
func ExampleConfig_Start() {
	var screen bytes.Buffer
	cfg := waitart.Config{
		LookupEnv: func(string) (string, bool) { return "", false },
		Terminal:  func(io.Writer) (int, bool) { return 80, true },
	}
	sc, _ := waitart.Builtin().Lookup("relay")
	p := cfg.Start(context.Background(), &screen, sc, "Sending: 3 files")
	_, _ = fmt.Fprintln(p.Wrap(&screen), "file 1 sent")
	p.Stop()
	fmt.Println(p != nil, strings.Contains(screen.String(), "file 1 sent\n"), strings.Contains(screen.String(), "Sending"))
	// Output:
	// true true true
}

// A Player returned for a disabled Config is nil, and every method of a nil Player is safe: Wrap
// returns the writer it was given.
func ExamplePlayer_Wrap() {
	var p *waitart.Player
	_, _ = fmt.Fprintln(p.Wrap(os.Stdout), "plain output")
	p.SetStatus("ignored")
	p.Stop()
	// Output:
	// plain output
}

// A product keeps its scenes in its own repository and passes them in a Set with its kinds of wait.
// A seeded source makes the choice the same on every run.
func ExampleNewSet() {
	palette := map[byte]waitart.Colour{'o': {R: 0xf0, G: 0xb8, B: 0x6a, ANSI: 215}}
	dot := func(x int) waitart.Frame {
		return waitart.Frame{Rows: []waitart.Row{{Text: strings.Repeat(" ", x) + "●", Mask: strings.Repeat(" ", x) + "o"}}}
	}
	scenes := []waitart.Scene{
		{Name: "dot", Palette: palette, Frames: []waitart.Frame{dot(0), dot(1), dot(2)}},
		{Name: "bounce", Palette: palette, Frames: []waitart.Frame{dot(0), dot(2), dot(0)}},
	}
	set, err := waitart.NewSet(scenes, map[waitart.Kind][]string{"approval": {"bounce"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(set.ForKind("approval", rand.NewPCG(1, 1)).Name)
	fmt.Println(set.ForKind("other", nil).Name)
	// Output:
	// bounce
	// dot
}

// Validate refuses a scene that would corrupt the terminal, such as one with an escape sequence or a
// two-column rune in its art, without quoting the scene in the error.
func ExampleScene_Validate() {
	sc := waitart.Scene{
		Name:   "bad",
		Frames: []waitart.Frame{{Rows: []waitart.Row{{Text: "\x1b[2J", Mask: "    "}}}},
	}
	err := sc.Validate()
	fmt.Println(errors.Is(err, waitart.ErrInvalidScene))
	fmt.Println(err)
	// Output:
	// true
	// waitart: invalid scene: frame 0 row 0: text holds a rune that is not one printable column
}

// Builtin holds three neutral scenes for a product without its own.
func ExampleBuiltin() {
	for _, sc := range waitart.Builtin().Scenes() {
		fmt.Printf("%s: %d frames, %d lines\n", sc.Name, len(sc.Frames), sc.Height())
	}
	// Output:
	// hourglass: 11 frames, 7 lines
	// relay: 11 frames, 4 lines
	// wave: 10 frames, 3 lines
}
