package waitart

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	hideCursor = "\x1b[?25l"
	showOnly   = "\x1b[?25h"
	showCursor = reset + showOnly
)

// Config decides whether a scene is drawn and how. The zero value draws nothing, because it has no
// [Config.LookupEnv].
type Config struct {
	// Off is the caller's opt-out, such as a --no-art flag or a product's own variable. When true
	// nothing is drawn.
	Off bool
	// LookupEnv reads an environment variable, for example os.LookupEnv. The package reads NO_COLOR,
	// TERM, COLORTERM, COLUMNS, LC_ALL, LC_CTYPE and LANG through it. When nil nothing is drawn.
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

// utf8Locale reports whether the locale uses UTF-8: the first of LC_ALL, LC_CTYPE and LANG that is set
// and not empty names UTF-8, or none is set.
func (c *Config) utf8Locale() bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(c.env(name)); v != "" {
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return true
}

// Enabled reports whether a scene would be drawn on w: only when LookupEnv is set, Off is false,
// NO_COLOR is empty or unset, TERM is not "dumb", the locale is UTF-8 or not set, and w is a terminal
// at least [Width] columns wide or of unknown width.
//
// The locale is the first of LC_ALL, LC_CTYPE and LANG that is set and not empty. A locale that does
// not name UTF-8, such as C, POSIX or en_US.ISO-8859-1, turns the art off: a terminal that reads 8-bit
// controls would take bytes inside the UTF-8 encoding of a rune such as U+00DB for a control sequence.
// With no locale set the terminal is taken to be UTF-8.
func (c Config) Enabled(w io.Writer) bool {
	if c.Off || c.LookupEnv == nil || w == nil {
		return false
	}
	if c.env("NO_COLOR") != "" || c.env("TERM") == "dumb" || !c.utf8Locale() {
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
	drawn     bool   // a frame is on the screen
	held      bool   // a partial output line is on the screen; draw nothing
	tail      []byte // the last bytes of output, to tell whether it ends inside a rune
	midRune   bool   // the output ends inside a UTF-8 sequence; write nothing to the terminal
	stopped   bool
	disabled  bool   // a write failed or the terminal narrowed; draw nothing more
	shown     bool   // the cursor show was asked for
	owed      string // bytes of the cursor show that have not reached the terminal yet
	terminal  func(io.Writer) (int, bool)
	ctx       context.Context

	once sync.Once
	quit chan struct{}
	done chan struct{}
}

// Start plays sc on w with the status line under it until ctx ends or [Player.Stop] is called. It
// returns nil, and draws nothing, when c is not [Config.Enabled] for w, when sc fails
// [Scene.Validate] or when ctx has already ended. A product checks its scenes in its own tests, or
// builds them into a [Set], so that an invalid scene is found before a user would see nothing.
//
// The first frame is drawn from the start of the line the cursor is on, so call Start once the output
// so far ends a line: text printed before Start without a line break, such as a prompt, is drawn over.
// The package cannot read the cursor position to tell.
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
		ctx:       ctx,
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
		if !p.stopped && ctx.Err() == nil {
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
// Player, and it returns once the player has stopped drawing. When the terminal took only part of the
// cursor show, a later Stop or write through [Player.Wrap] sends the rest. When output written through
// Wrap ends inside a UTF-8 sequence, the cursor show waits for the write that completes it, so no byte
// lands inside a rune.
//
// Stop waits for a terminal write in progress: on a terminal whose writes block, such as a pipe nobody
// reads, Stop blocks with it, and so do [Player.SetStatus] and writes through Wrap.
func (p *Player) Stop() {
	if p == nil {
		return
	}
	p.stop()
	<-p.done
	p.mu.Lock()
	p.payCursor()
	p.mu.Unlock()
}

func (p *Player) stop() {
	p.once.Do(func() {
		p.mu.Lock()
		// The width is checked first: on a narrowed terminal the cursor-up count no longer holds.
		if p.drawn && p.fits() {
			p.clear()
		}
		p.restoreCursor()
		p.stopped = true
		p.mu.Unlock()
		close(p.quit)
	})
}

// restoreCursor asks for the cursor show and sends it unless the output ends inside a rune. It is
// written directly, not through write, which refuses once drawing is disabled. The caller holds p.mu.
func (p *Player) restoreCursor() {
	if !p.shown {
		p.shown = true
		p.owed = showCursor
	}
	p.payCursor()
}

// payCursor sends the part of the cursor show that has not reached the terminal, when the output is at
// a rune boundary. While a partial output line is on the screen the colour reset is left out, so the
// caller's own colour stays. The caller holds p.mu.
func (p *Player) payCursor() {
	if p.owed == "" || p.midRune {
		return
	}
	if p.owed == showCursor && p.held {
		p.owed = showOnly
	}
	n, _ := io.WriteString(p.w, p.owed)
	p.owed = p.owed[min(max(n, 0), len(p.owed)):]
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
// The frame is drawn once out returns, so out must pass each write on to the terminal before it
// returns. A buffered writer with a Flush() error method, such as a [bufio.Writer], is flushed after
// each write, and when the flush fails no frame is drawn until a later line ends. Any other buffering
// writer would let a frame land before bytes it still holds.
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
	if len(b) == 0 {
		return l.out.Write(b)
	}
	if p.stopped {
		n, err := l.out.Write(b)
		p.track(b[:min(max(n, 0), len(b))])
		p.payCursor()
		return n, err
	}
	// The width check may disable the art and show the cursor, so it waits while the output ends
	// inside a rune; nothing is drawn then anyway, since the line is unfinished.
	if !p.midRune && p.fits() {
		p.clear()
	}
	n, err := l.out.Write(b)
	// Only the bytes that were written count: after a short or failed write the line on the screen is
	// whatever reached it.
	p.track(b[:min(max(n, 0), len(b))])
	if f, ok := l.out.(interface{ Flush() error }); ok && n > 0 {
		if ferr := f.Flush(); ferr != nil {
			// Some of the bytes may still be held: treat the line as unfinished.
			p.held = true
			if err == nil {
				err = ferr
			}
		}
	}
	if !p.held && err == nil && p.fits() {
		p.draw()
	}
	p.payCursor()
	return n, err
}

// track records the output bytes that reached the terminal: whether they leave a line unfinished, and
// whether they end inside a UTF-8 sequence. The caller holds p.mu.
func (p *Player) track(b []byte) {
	if len(b) == 0 {
		return
	}
	p.held = b[len(b)-1] != '\n'
	const keep = utf8.UTFMax - 1
	if len(b) >= keep {
		p.tail = append(p.tail[:0], b[len(b)-keep:]...)
	} else {
		p.tail = append(p.tail, b...)
		if len(p.tail) > keep {
			p.tail = append(p.tail[:0], p.tail[len(p.tail)-keep:]...)
		}
	}
	p.midRune = openRune(p.tail)
}

// openRune reports whether b ends inside a UTF-8 sequence: a lead byte whose continuation bytes have
// not all arrived. Only the last three bytes matter.
func openRune(b []byte) bool {
	for i := len(b) - 1; i >= 0 && i >= len(b)-(utf8.UTFMax-1); i-- {
		c := b[i]
		switch {
		case c < 0x80:
			return false
		case c >= 0xf0:
			return len(b)-i < 4
		case c >= 0xe0:
			return len(b)-i < 3
		case c >= 0xc0:
			return len(b)-i < 2
		}
		// A continuation byte: look further back for its lead byte.
	}
	return false
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
	p.restoreCursor()
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
	if p.stopped || p.held || p.disabled || p.ctx.Err() != nil {
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
