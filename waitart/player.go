package waitart

import (
	"context"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	hideCursor = "\x1b[?25l"
	showCursor = reset + "\x1b[?25h"
)

// Config decides whether a scene is drawn and how. The zero value draws nothing, because it has no
// [Config.LookupEnv].
type Config struct {
	// Off is the caller's opt-out, such as a --no-art flag or a product's own variable. When true
	// nothing is drawn.
	Off bool
	// LookupEnv reads an environment variable, for example os.LookupEnv. The package reads NO_COLOR,
	// TERM, COLORTERM and COLUMNS through it. When nil nothing is drawn.
	LookupEnv func(name string) (value string, ok bool)
	// Terminal reports whether w is a terminal and how many columns it has (0 when unknown). It is
	// called again before each frame, so a terminal that narrows or goes away stops the drawing. When
	// nil, w counts as a terminal when it is an *os.File on a character device, with its width taken
	// from COLUMNS when that is set, and unknown otherwise.
	Terminal func(w io.Writer) (cols int, ok bool)
	// Style sets the status line colours; nil uses [DefaultStyle].
	Style *Style
}

func (c *Config) env(name string) string {
	v, _ := c.LookupEnv(name)
	return v
}

// charDevice reports whether w is an *os.File on a character device, the default terminal check. A
// character device is not always a terminal (the null device is one), but drawing on such a device
// harms nothing.
func charDevice(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// terminal returns the terminal check of c with an unknown width filled in from COLUMNS.
func (c *Config) terminal() func(io.Writer) (int, bool) {
	check := c.Terminal
	if check == nil {
		check = func(w io.Writer) (int, bool) { return 0, charDevice(w) }
	}
	envCols := 0
	if n, err := strconv.Atoi(c.env("COLUMNS")); err == nil && n > 0 {
		envCols = n
	}
	return func(w io.Writer) (int, bool) {
		cols, ok := check(w)
		if ok && cols <= 0 {
			cols = envCols
		}
		return cols, ok
	}
}

// fitsWidth reports whether a frame fits a terminal of cols columns; 0 means the width is unknown.
func fitsWidth(cols int) bool { return cols == 0 || cols >= Width }

// Enabled reports whether a scene would be drawn on w: only when LookupEnv is set, Off is false,
// NO_COLOR is empty or unset, TERM is not "dumb", and w is a terminal at least [Width] columns wide or
// of unknown width.
func (c Config) Enabled(w io.Writer) bool {
	if c.Off || c.LookupEnv == nil || w == nil {
		return false
	}
	if c.env("NO_COLOR") != "" || c.env("TERM") == "dumb" {
		return false
	}
	cols, ok := c.terminal()(w)
	// A narrower terminal would wrap the frame and break the redraw.
	return ok && fitsWidth(cols)
}

// Player plays one scene in place. A nil *Player is valid and does nothing, so callers need no check
// when the art is off.
type Player struct {
	mu        sync.Mutex
	w         io.Writer
	scene     Scene
	style     Style
	lines     int // lines a drawn frame takes
	status    string
	truecolor bool
	idx       int
	drawn     bool // a frame is on the screen
	held      bool // a partial output line is on the screen; draw nothing
	stopped   bool
	disabled  bool // a write failed or the terminal narrowed; draw nothing more
	terminal  func(io.Writer) (int, bool)

	once sync.Once
	quit chan struct{}
	done chan struct{}
}

// Start plays sc on w with the status line under it until ctx ends or [Player.Stop] is called. It
// returns nil, and draws nothing, when c is not [Config.Enabled] for w, when sc fails
// [Scene.Validate] or when ctx has already ended. A product checks its scenes in its own tests, or
// builds them into a [Set], so that an invalid scene is found before a user would see nothing.
func (c Config) Start(ctx context.Context, w io.Writer, sc Scene, status string) *Player {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.Enabled(w) || sc.Validate() != nil || ctx.Err() != nil {
		return nil
	}
	style := DefaultStyle
	if c.Style != nil {
		style = *c.Style
	}
	ct := c.env("COLORTERM")
	p := &Player{
		w:         w,
		terminal:  c.terminal(),
		scene:     sc.clone(),
		style:     style,
		lines:     sc.Height(),
		status:    status,
		truecolor: ct == "truecolor" || ct == "24bit",
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	p.mu.Lock()
	p.write(hideCursor)
	p.draw()
	p.mu.Unlock()
	go p.run(ctx)
	return p
}

func (p *Player) run(ctx context.Context) {
	defer close(p.done)
	for {
		p.mu.Lock()
		delay := p.scene.Frames[p.idx%len(p.scene.Frames)].Delay
		p.mu.Unlock()
		if delay <= 0 {
			delay = DefaultDelay
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			p.stop()
			return
		case <-p.quit:
			t.Stop()
			return
		case <-t.C:
		}
		p.mu.Lock()
		if !p.stopped {
			p.idx++
			if !p.held && p.fits() {
				p.clear()
				p.draw()
			}
		}
		p.mu.Unlock()
	}
}

// Stop clears the frame and shows the cursor again. It is safe to call more than once and on a nil
// Player, and it returns once the player has stopped drawing.
func (p *Player) Stop() {
	if p == nil {
		return
	}
	p.stop()
	<-p.done
}

func (p *Player) stop() {
	p.once.Do(func() {
		p.mu.Lock()
		p.clear()
		// Written directly: write refuses once drawing is disabled, and the cursor show inside disable
		// may have failed while the writer was down.
		_, _ = io.WriteString(p.w, showCursor)
		p.stopped = true
		p.mu.Unlock()
		close(p.quit)
	})
}

// SetStatus changes the status line; the next frame shows it. It is safe to call from any goroutine
// and on a nil Player.
func (p *Player) SetStatus(status string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.status = status
	p.mu.Unlock()
}

// Wrap returns a writer for real output that shares the screen with the player: before each write the
// frame is cleared, and once the output ends a line the frame is drawn again below it. A frame is
// never drawn while a line is unfinished, so no frame byte lands inside an output line. Writes are
// serialised with the frames. After Stop, writes pass straight to out. On a nil Player it returns out.
//
// Every writer Wrap returns shares one record of whether a line is unfinished, so all output that
// shares the screen with the player, such as standard output and standard error on the same terminal,
// should go through Wrap.
func (p *Player) Wrap(out io.Writer) io.Writer {
	if p == nil {
		return out
	}
	return &lineWriter{p: p, out: out}
}

type lineWriter struct {
	p   *Player
	out io.Writer
}

func (l *lineWriter) Write(b []byte) (int, error) {
	p := l.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || len(b) == 0 {
		return l.out.Write(b)
	}
	if p.fits() {
		p.clear()
	}
	n, err := l.out.Write(b)
	// Only the bytes that were written count: after a short or failed write the line on the screen is
	// whatever reached it.
	if n > 0 {
		p.held = b[n-1] != '\n'
	}
	if !p.held && err == nil {
		p.draw()
	}
	return n, err
}

// fits checks the terminal width again before a frame is cleared or drawn. When a frame no longer
// fits, or the writer is no longer a terminal, it turns the art off. The caller holds p.mu.
func (p *Player) fits() bool {
	if p.disabled {
		return false
	}
	if cols, ok := p.terminal(p.w); ok && fitsWidth(cols) {
		return true
	}
	p.disable()
	return false
}

// disable stops all drawing. The frame on the screen may have wrapped or been cut short, so the
// cursor-up count for clearing it can no longer be trusted: nothing is cleared. The cursor is shown
// again at once. The caller holds p.mu.
func (p *Player) disable() {
	if p.disabled {
		return
	}
	p.disabled = true
	p.drawn = false
	_, _ = io.WriteString(p.w, showCursor)
}

// clearSeq moves the cursor to the first line of a frame of n lines and erases to the end of the screen.
func clearSeq(n int) string {
	return "\x1b[" + strconv.Itoa(n) + "A\r\x1b[J"
}

// clear removes the frame from the screen. The caller holds p.mu.
func (p *Player) clear() {
	if !p.drawn || p.disabled {
		return
	}
	if !p.write(clearSeq(p.lines)) {
		return
	}
	p.drawn = false
}

// draw writes the current frame from the cursor down and leaves the cursor at the start of the line
// below it. The caller holds p.mu.
func (p *Player) draw() {
	if p.stopped || p.held || p.disabled {
		return
	}
	s := "\r" + render(&p.scene, &p.style, p.idx, p.status).ansi(p.truecolor)
	// A frame that did not reach the screen whole is never cleared: the cursor-up move would erase
	// lines above it.
	p.drawn = p.write(s)
}

// write sends s to the terminal and reports whether all of it arrived. After a short or failed write
// the art is turned off, since the failure only loses the art. The caller holds p.mu.
func (p *Player) write(s string) bool {
	if p.disabled {
		return false
	}
	n, err := io.WriteString(p.w, s)
	if err != nil || n < len(s) {
		p.disable()
		return false
	}
	return true
}
