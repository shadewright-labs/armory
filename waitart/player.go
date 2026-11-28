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
	busy      bool   // a wrapped write is with its writer, without the lock
	overlap   bool   // another wrapped write ran while busy, so the order on the screen is unknown
	stopped   bool
	off       bool // the terminal narrowed or went away: no more frames
	dead      bool // a write failed or came up short: write nothing to the terminal again
	showAsked bool // the cursor show was asked for
	showDue   bool // the cursor show was asked for and not sent yet
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
// Start does not check this, because the package cannot read the cursor position.
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
	p.draw() // checks the width again, after the cursor hide
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
		if !p.stopped && !p.busy && ctx.Err() == nil {
			p.idx++
			if !p.held && p.fits() {
				p.clear()
				p.draw() // checks the width again, after the clear
			}
		}
		p.mu.Unlock()
	}
}

// Stop clears the frame and shows the cursor again. It is safe to call more than once and on a nil
// Player, and it returns once the player has stopped drawing.
//
// When output written through [Player.Wrap] ends inside a UTF-8 sequence, the cursor show waits for
// the wrapped write that completes it, so no byte lands inside a rune; if that write never comes, the
// cursor stays hidden. After a failed or short write the player is inert (see the package
// documentation) and Stop writes nothing.
//
// Stop waits for a terminal write of the player in progress: on a terminal whose writes block, such as
// a pipe nobody reads, Stop blocks with it, and so does [Player.SetStatus].
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
		// The width is checked first: on a narrowed terminal the cursor-up count no longer holds. While
		// a wrapped write is busy the frame is already cleared.
		if p.drawn && !p.busy && p.fits() {
			p.clear()
		}
		p.stopped = true
		p.askShow()
		p.mu.Unlock()
		close(p.quit)
	})
}

// askShow asks for the cursor show once and sends it if it can go now. The caller holds p.mu.
func (p *Player) askShow() {
	if !p.showAsked {
		p.showAsked, p.showDue = true, true
	}
	p.sendShow()
}

// sendShow writes the cursor show that is due, in one write, unless the player is inert, a wrapped
// write is busy or the output ends inside a rune; then it stays due. While a partial output line is on
// the screen the colour reset is left out, so the caller's own colour stays. A failed or short show is
// not sent again. The caller holds p.mu.
func (p *Player) sendShow() {
	if !p.showDue || p.dead || p.busy || p.midRune {
		return
	}
	p.showDue = false
	if p.held {
		p.write(showOnly)
	} else {
		p.write(showCursor)
	}
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
// never drawn while a line is unfinished, so no frame byte lands inside an output line. After Stop,
// writes pass straight to out. On a nil Player it returns out.
//
// The frame is drawn once out returns, so out must pass each write on to the terminal before it
// returns. A buffered writer with a Flush() error method, such as a [bufio.Writer], is flushed after
// each write. Any other buffering writer would let a frame land before bytes it still holds.
//
// When a write to out fails or comes up short, or its flush fails, the player goes inert for good:
// it draws, clears and sends nothing more, and later writes through Wrap pass straight to out.
//
// The player's lock is not held while out is written or flushed, so out may itself write through
// another writer of the same player, as in p.Wrap(bufio.NewWriter(p.Wrap(term))). A wrapped write that
// arrives while another is with its writer, nested like that or from another goroutine, passes
// straight through, and the player draws no frame until a later wrapped write that runs alone ends a
// line. The player does not order writes made through Wrap from several goroutines at once.
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

// pass writes b to out and flushes out when it can, and reports whether all of b went through.
func (l *lineWriter) pass(b []byte) (int, bool, error) {
	n, err := l.out.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if f, ok := l.out.(interface{ Flush() error }); ok && err == nil {
		err = f.Flush()
	}
	return n, err == nil, err
}

func (l *lineWriter) Write(b []byte) (int, error) {
	p := l.p
	if len(b) == 0 {
		return l.out.Write(b)
	}
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		n, _, err := l.pass(b)
		return n, err
	}
	nested := p.busy
	if nested {
		p.overlap = true
	} else {
		p.busy, p.overlap = true, false
		// While the output ends inside a rune no frame is on the screen, since the line is unfinished.
		if !p.stopped && !p.midRune && p.fits() {
			p.clear()
		}
	}
	p.mu.Unlock()

	n, ok, err := l.pass(b)

	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok {
		p.dead, p.drawn = true, false
	} else {
		p.track(b)
	}
	if nested {
		// The writer that was busy finishes later and decides; the line counts as unfinished.
		p.held = true
		return n, err
	}
	p.busy = false
	if p.overlap {
		p.held = true
	}
	if ok {
		p.fits() // checked after every wrapped write, line break or not
		p.draw()
	}
	p.sendShow()
	return n, err
}

// track records output bytes that reached the terminal: whether they leave a line unfinished, and
// whether they end inside a UTF-8 sequence. The caller holds p.mu.
func (p *Player) track(b []byte) {
	if len(b) == 0 {
		return
	}
	p.held = b[len(b)-1] != '\n'
	const keep = utf8.UTFMax - 1
	p.tail = append(p.tail, b[max(0, len(b)-keep):]...)
	p.tail = p.tail[max(0, len(p.tail)-keep):]
	p.midRune = openRune(p.tail)
}

// openRune reports whether b ends inside a UTF-8 sequence: the last byte that can start a rune is a
// valid lead byte whose continuation bytes have not all arrived. A byte that starts no valid sequence,
// such as 0xff, leaves nothing open.
func openRune(b []byte) bool {
	for i := len(b) - 1; i >= 0 && i >= len(b)-(utf8.UTFMax-1); i-- {
		if utf8.RuneStart(b[i]) {
			return !utf8.FullRune(b[i:])
		}
	}
	return false
}

// fits checks the terminal width again before a frame is cleared or drawn and after each write that
// could change it. When a frame no longer fits, or the writer is no longer a terminal, it turns the
// frames off and asks for the cursor show. The caller holds p.mu.
func (p *Player) fits() bool {
	if p.off || p.dead || p.stopped {
		return false
	}
	if cols, ok := p.terminal(p.w); ok && fitsWidth(cols) {
		return true
	}
	// The frame on the screen may have wrapped or been cut short, so the cursor-up count for clearing
	// it can no longer be trusted: nothing is cleared.
	p.off, p.drawn = true, false
	p.askShow()
	return false
}

// clearSeq moves the cursor to the first line of a frame of n lines and erases to the end of the screen.
func clearSeq(n int) string {
	return "\x1b[" + strconv.Itoa(n) + "A\r\x1b[J"
}

// clear removes the frame from the screen. The caller holds p.mu.
func (p *Player) clear() {
	if !p.drawn {
		return
	}
	p.drawn = false
	p.write(clearSeq(p.lines))
}

// draw writes the current frame from the cursor down and leaves the cursor at the start of the line
// below it. The width and the context are checked once more after the frame is rendered, just before
// it is written; a write already under way cannot be called back. The caller holds p.mu.
func (p *Player) draw() {
	if p.stopped || p.held || p.busy || p.off || p.dead || p.ctx.Err() != nil {
		return
	}
	s := "\r" + render(&p.scene, &p.style, p.idx, p.status).ansi(p.truecolor)
	if !p.fits() || p.ctx.Err() != nil {
		return
	}
	// A frame that did not reach the screen whole is never cleared: the player is inert after it.
	p.drawn = p.write(s)
}

// write sends s to the terminal and reports whether all of it arrived. After a failed or short write
// the player goes inert for good: it writes nothing to the terminal again, not even the cursor show,
// and retries nothing. The caller holds p.mu.
func (p *Player) write(s string) bool {
	if p.dead {
		return false
	}
	n, err := io.WriteString(p.w, s)
	if err != nil || n < len(s) {
		p.dead, p.drawn = true, false
		return false
	}
	return true
}
