package modelusage

import (
	"bytes"
	"errors"
	"io"
)

// ParseStream reads a streamed reply of family f, sent as server-sent events, from r and returns its usage.
//
// Reading. It reads at most lim.MaxBytes bytes and one more, to tell a stream of exactly the limit from a
// longer one, and refuses a line, or the joined data of one event, longer than lim.MaxLineBytes. Limits apply
// to the bytes the parser consumes, in order: every line that ends within the first lim.MaxBytes bytes is
// handled before a byte past the limit is reported, so the result never depends on how much the reader
// delivers in one call. Lines end in LF, CRLF or a bare CR, and a byte order mark at the start of the stream
// is skipped. Comment lines and fields other than data are ignored. The data lines of one event, with the one
// space after the colon removed, are joined with LF and decoded as one JSON value when the blank line that ends
// the event is read. A CR LF line end is consumed whole before its line is handled, so a byte limit between
// the CR and the LF cuts the line. An event the input ends inside (any field line without the blank line that
// ends it) is never handled: before the final event it makes the stream truncated, after it a trailing
// problem.
//
// Which usage counts, per family:
//
//   - Anthropic: message_start carries the first usage block in message.usage and each message_delta a
//     cumulative one. Each count takes the latest value present in a block, as the vendor's own accumulator
//     does; a count absent or null keeps its earlier value. A count lower than its earlier value is a
//     contradiction: the counts take the new values and the parse stops with ErrInconsistent. The final event
//     is message_stop. After it, ping events and events of types the parser does not know are allowed, since
//     the provider may send ping at any point and add event types; any other event is a trailing problem.
//   - OpenAIChat: OpenAI sends one chunk with a non-null usage field (stream_options.include_usage), the last
//     before data: [DONE]; some compatible servers send cumulative usage on every chunk. The latest complete
//     block is the usage, as a whole and never mixed count by count. A block with no count lower than in the
//     block before is consistent, so growing blocks are a normal stream; a block with any lower count is a
//     contradiction: the usage becomes the new block and the parse stops with ErrInconsistent. The final
//     event is data whose whole text is [DONE]. Chat chunks do not use the type member, so it is not read. A
//     chunk with an error member ends the stream as truncated, after its usage, if any, is taken. Nothing is
//     allowed after [DONE].
//   - OpenAIResponses: the usage is response.usage of the final event, one of response.completed,
//     response.incomplete or response.failed, and nothing else. After it, one data: [DONE], which servers that
//     copy chat completions send, and events other than a final or error event are allowed.
//
// The parse stops at the first problem. The error then tells how to use the Usage:
//
//   - nil: the stream reached its final event and nothing wrong followed. The Usage is final.
//   - *TrailingError: the final event was read, then a problem (a second final event or [DONE], an event that
//     would change the counts, a malformed or unfinished event, a byte limit, a reader error, an error event).
//     The Usage is exactly the one at the final event, the same as with a nil error.
//   - *PartialError: at least one usage block with a count was accepted, then the stream ended or broke
//     before its final event. The Usage is the usage so far, under the rules above. See PartialError for what
//     it bounds.
//   - any other error, with a zero Usage: no usage block with a count was accepted. ErrInvalidLimits, a
//     *LimitError matching ErrTooLarge or ErrLineTooLong, ErrMalformed, ErrNoUsage, ErrOutOfRange,
//     ErrTruncated, or the reader's error.
//
// ParseStream returns *TrailingError and *PartialError themselves, never wrapped, and their cause never holds
// either type: a reader's error is kept as an opaque cause that errors.Is matches (the reader's error value and
// its chain: io.ErrUnexpectedEOF, a context error, a sentinel of the caller's) but errors.As does not unwrap.
// A reader error before the final event also matches ErrTruncated. Only io.EOF itself ends the input; see the
// package documentation for other reader errors and empty reads.
func ParseStream(f Family, r io.Reader, lim Limits) (Usage, error) {
	if err := lim.check(f, true); err != nil {
		return Usage{}, err
	}
	p := newStreamParser(f, r, lim)
	for p.step() {
	}
	return p.result()
}

// streamParser is the whole state of one ParseStream call. Reading, line splitting, event assembly and the
// handlers of each family only update it, or record a problem with report; result alone turns it into what
// ParseStream returns. No method but result returns a Usage or an error, so nothing else can decide the
// result (TestStreamHasOneExit checks this).
type streamParser struct {
	family Family
	lim    Limits
	r      io.Reader

	// Reading. buf[off:] holds the bytes read and not yet consumed; pos is the stream offset of buf[off].
	buf     []byte
	off     int
	maxBuf  int   // the largest buffer ever needed: a line at the limit, its line end and a byte order mark
	pos     int64 // bytes consumed
	read    int64 // bytes read from r, at most MaxBytes+1
	eof     bool
	readErr error // the reader's error, reported once every byte before it is consumed
	empty   int   // reads in a row that returned no byte and no error
	bomDone bool  // the start of the stream was checked for a byte order mark

	// Event assembly.
	data    []byte // the joined data lines of the current event
	pending bool   // the current event has a data line
	open    bool   // the current event has a field line of any name, so its blank line is still due

	// The accumulator.
	started   bool           // Anthropic: message_start seen
	acc       anthropicUsage // Anthropic: the latest value of each count
	last      Usage          // chat completions: the latest complete usage block
	final     Usage          // the usage at the final event
	seen      bool           // a usage block with at least one count was accepted
	done      bool           // a valid final event was read
	doneLines int            // Responses: [DONE] events after the final event
	problem   error          // the first problem, nil while there is none
	afterDone bool           // the problem came after the final event
}

// initialBuffer is the size of the read buffer a parse starts with, when its limits allow one that large.
const initialBuffer = 16 << 10

func newStreamParser(f Family, r io.Reader, lim Limits) *streamParser {
	most := lim.MaxLineBytes
	if lim.MaxBytes < int64(most) {
		most = int(lim.MaxBytes)
	}
	// most is at most math.MaxInt-5 (Limits.check), so this cannot overflow.
	maxBuf := most + 4
	return &streamParser{family: f, lim: lim, r: r, maxBuf: maxBuf, buf: make([]byte, 0, min(initialBuffer, maxBuf))}
}

// result is the only place that turns the state into what ParseStream returns.
func (p *streamParser) result() (Usage, error) {
	problem := p.problem
	if p.done && (problem == nil || p.afterDone) {
		if problem == nil {
			return p.final, nil
		}
		return p.final, &TrailingError{Err: problem}
	}
	if problem == nil {
		problem = ErrTruncated
	}
	if !p.seen {
		return Usage{}, problem
	}
	u := p.final
	switch p.family {
	case Anthropic:
		u = p.acc.usage()
	case OpenAIChat:
		u = p.last
	}
	return u, &PartialError{Err: problem}
}

// report records err as the problem of the parse, unless one is recorded already. The parse stops after it.
func (p *streamParser) report(err error) {
	if p.problem == nil {
		p.problem, p.afterDone = err, p.done
	}
}

// step handles the next line and reports whether the parse goes on.
func (p *streamParser) step() bool {
	line, ok := p.nextLine()
	if !ok {
		if p.problem == nil {
			p.end()
		}
		return false
	}
	p.line(line)
	return p.problem == nil
}

// end records what the end of the input means once every byte before it was handled.
func (p *streamParser) end() {
	switch {
	case p.readErr != nil:
		p.report(&readError{err: p.readErr, truncated: !p.done})
	case p.open:
		// The input ended inside an event, whatever its fields: it is not handled, so it cannot be the final
		// event, and after the final event it is a trailing problem.
		p.report(ErrTruncated)
	}
}

// nextLine returns the next line without its line end, or false when there is none: the input ended, or a
// limit was passed, which it reports. It hands out only lines that end within the first MaxBytes bytes (or
// at the end of the input), so a byte read past the limit never hides a line before it.
func (p *streamParser) nextLine() ([]byte, bool) {
	for {
		avail := p.buf[p.off:]
		if !p.bomDone {
			if len(avail) < len(byteOrderMark) && p.fill() {
				continue
			}
			p.bomDone = true
			if p.lim.MaxBytes >= int64(len(byteOrderMark)) && bytes.HasPrefix(avail, byteOrderMark) {
				p.consume(len(byteOrderMark))
				continue
			}
		}
		window := avail
		if left := p.lim.MaxBytes - p.pos; int64(len(window)) > left {
			window = window[:left]
		}
		scan := window[:min(len(window), p.lim.MaxLineBytes+1)]
		if i := bytes.IndexAny(scan, "\r\n"); i >= 0 {
			// A line end is consumed whole before the line is handed out: LF, CR LF, or a CR whose next byte
			// is known not to be LF.
			end := i + 1
			if scan[i] == '\r' {
				switch {
				case i+1 < len(window):
					if window[i+1] == '\n' {
						end++
					}
				case i+1 < len(avail):
					// The next byte lies past the byte limit. If it is the LF of this CR LF, the line end is
					// cut by the limit, so the line is not complete within it.
					if avail[i+1] == '\n' {
						p.report(&LimitError{Limit: p.lim.MaxBytes})
						return nil, false
					}
				case p.fill():
					continue
				}
			}
			p.consume(end)
			return scan[:i], true
		}
		switch {
		case len(window) > p.lim.MaxLineBytes:
			p.report(&LimitError{Line: true, Limit: int64(p.lim.MaxLineBytes)})
			return nil, false
		case len(window) < len(avail):
			// The window ends at the byte limit, and a byte past it was read.
			p.report(&LimitError{Limit: p.lim.MaxBytes})
			return nil, false
		case p.fill():
			continue
		case len(window) > 0:
			// The last line of the input, without a line end.
			p.consume(len(window))
			return window, true
		}
		return nil, false
	}
}

// consume marks the next n bytes of the buffer as handled.
func (p *streamParser) consume(n int) {
	p.off += n
	p.pos += int64(n)
}

// errBadRead stands for a reader that returned a byte count outside the buffer it was given.
var errBadRead = errors.New("modelusage: the reader returned an invalid count")

// maxEmptyReads is how many reads in a row may return no byte and no error before the reader counts as stuck.
const maxEmptyReads = 100

// fill reads more bytes into the buffer. It reports false when the input can give no more: it ended, failed,
// or the byte past the limit was read.
func (p *streamParser) fill() bool {
	if p.eof || p.readErr != nil || p.read > p.lim.MaxBytes {
		return false
	}
	if len(p.buf) == cap(p.buf) {
		switch {
		case p.off > 0:
			n := copy(p.buf, p.buf[p.off:])
			p.buf, p.off = p.buf[:n], 0
		case cap(p.buf) < p.maxBuf:
			grown := make([]byte, len(p.buf), min(2*cap(p.buf), p.maxBuf))
			copy(grown, p.buf)
			p.buf = grown
		default:
			// Not reached: a full buffer of maxBuf bytes holds a line past the line limit or the byte past
			// the byte limit, which nextLine reports before it asks for more.
			p.readErr = errBadRead
			return false
		}
	}
	space := p.buf[len(p.buf):cap(p.buf)]
	if left := p.lim.MaxBytes + 1 - p.read; int64(len(space)) > left {
		space = space[:left]
	}
	n, err := p.r.Read(space)
	if n < 0 || n > len(space) {
		n, err = 0, errBadRead
	}
	p.buf = p.buf[:len(p.buf)+n]
	p.read += int64(n)
	switch {
	case err == io.EOF:
		// Only io.EOF itself ends the input; one wrapped or joined with another error is a failure.
		p.eof = true
	case err != nil:
		p.readErr = err
	case n == 0:
		if p.empty++; p.empty >= maxEmptyReads {
			p.readErr = io.ErrNoProgress
		}
	default:
		p.empty = 0
	}
	return true
}

// line handles one line: a blank line ends the current event, a data line adds to it, and any other is ignored.
func (p *streamParser) line(line []byte) {
	if len(line) == 0 {
		if p.pending {
			p.event(p.data)
			p.data, p.pending = p.data[:0], false
		}
		p.open = false
		return
	}
	if line[0] == ':' {
		return
	}
	p.open = true
	name, value, _ := bytes.Cut(line, []byte(":"))
	if string(name) != "data" {
		return
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if p.pending {
		p.data = append(p.data, '\n')
	}
	p.data = append(p.data, value...)
	p.pending = true
	// The joined data of one event is held to the line limit too, so that many data lines cannot make one
	// event as large as the whole reply.
	if len(p.data) > p.lim.MaxLineBytes {
		p.report(&LimitError{Line: true, Limit: int64(p.lim.MaxLineBytes)})
	}
}

// event handles the data of one complete event.
func (p *streamParser) event(data []byte) {
	switch p.family {
	case Anthropic:
		p.anthropicEvent(data)
	case OpenAIChat:
		p.chatEvent(data)
	default:
		p.responsesEvent(data)
	}
}

// doneMarker is the data of the event that ends a chat completions stream, exactly.
const doneMarker = "[DONE]"

// knownAnthropicEvent reports whether typ is an Anthropic event type other than ping.
func knownAnthropicEvent(typ string) bool {
	switch typ {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta",
		"content_block_stop", typeError:
		return true
	}
	return false
}

func (p *streamParser) anthropicEvent(data []byte) {
	ev, err := decodeEvent(data, true)
	if err != nil {
		p.report(err)
		return
	}
	typ := string(ev.typ)
	if p.done {
		if knownAnthropicEvent(typ) {
			p.report(ErrMalformed)
		}
		return
	}
	switch typ {
	case "message_start":
		if ev.message == nil || isNull(ev.message) {
			p.report(ErrMalformed)
			return
		}
		block, err := usageMember(ev.message)
		switch {
		case err != nil:
			p.report(err)
		case p.started:
			p.report(ErrMalformed)
		default:
			p.started = true
			p.mergeAnthropic(block)
		}
	case "message_delta":
		if !p.started {
			p.report(ErrMalformed)
			return
		}
		p.mergeAnthropic(ev.usage)
	case "message_stop":
		if !p.started {
			p.report(ErrMalformed)
			return
		}
		u, err := anthropicOf(&p.acc, p.lim.MaxTokens)
		if err != nil {
			p.report(err)
			return
		}
		p.final, p.done = u, true
	case typeError:
		p.report(ErrTruncated)
	}
}

// mergeAnthropic takes each count the raw usage block v carries as the latest value of that count. A block
// with a refused count, or one that would take a count or total past the limit, is not taken. A block that
// lowers a count is taken and then reported as ErrInconsistent.
func (p *streamParser) mergeAnthropic(v []byte) {
	au, err := readAnthropicUsage(v)
	if err != nil {
		p.report(err)
		return
	}
	if au == nil {
		return
	}
	from := au.counts()
	if err := firstErr(from...); err != nil {
		p.report(err)
		return
	}
	next := p.acc
	present, lower := false, false
	for i, to := range next.counts() {
		if !from[i].set {
			continue
		}
		present = true
		lower = lower || (to.set && from[i].n < to.n)
		*to = *from[i]
	}
	if !present {
		return
	}
	if err := next.usage().checkRange(p.lim.MaxTokens); err != nil {
		p.report(err)
		return
	}
	p.acc, p.seen = next, true
	if lower {
		p.report(ErrInconsistent)
	}
}

func (p *streamParser) chatEvent(data []byte) {
	if p.done {
		p.report(ErrMalformed)
		return
	}
	if string(data) == doneMarker {
		if !p.seen {
			p.report(ErrNoUsage)
			return
		}
		p.final, p.done = p.last, true
		return
	}
	ev, err := decodeEvent(data, false)
	if err != nil {
		p.report(err)
		return
	}
	if ev.usage != nil && !isNull(ev.usage) {
		u, err := usageOf(ev.usage, OpenAIChat, p.lim.MaxTokens)
		if err != nil {
			p.report(err)
			return
		}
		lower := p.seen && anyLower(u, p.last)
		p.last, p.seen = u, true
		if lower {
			p.report(ErrInconsistent)
			return
		}
	}
	if ev.err != nil && !isNull(ev.err) {
		p.report(ErrTruncated)
	}
}

func (p *streamParser) responsesEvent(data []byte) {
	if p.done && string(data) == doneMarker {
		if p.doneLines++; p.doneLines > 1 {
			p.report(ErrMalformed)
		}
		return
	}
	ev, err := decodeEvent(data, true)
	if err != nil {
		p.report(err)
		return
	}
	switch string(ev.typ) {
	case "response.completed", "response.incomplete", "response.failed":
		if p.done || ev.response == nil || isNull(ev.response) {
			p.report(ErrMalformed)
			return
		}
		block, err := usageMember(ev.response)
		if err != nil {
			p.report(err)
			return
		}
		u, err := usageOf(block, OpenAIResponses, p.lim.MaxTokens)
		if err != nil {
			p.report(err)
			return
		}
		p.final, p.seen, p.done = u, true, true
	case typeError:
		// Before the final event an error event ends the reply early; after it, it has no place.
		if p.done {
			p.report(ErrMalformed)
			return
		}
		p.report(ErrTruncated)
	}
}

// anyLower reports whether any count of next is lower than the same count of prev.
func anyLower(next, prev Usage) bool {
	a := [...]int64{next.InputTokens, next.OutputTokens, next.CacheCreationInputTokens,
		next.CacheCreation5mInputTokens, next.CacheCreation1hInputTokens, next.CacheReadInputTokens,
		next.CachedPromptTokens, next.CacheWritePromptTokens, next.CacheWrite30mPromptTokens,
		next.CacheWrite12hPromptTokens, next.ReasoningTokens}
	b := [...]int64{prev.InputTokens, prev.OutputTokens, prev.CacheCreationInputTokens,
		prev.CacheCreation5mInputTokens, prev.CacheCreation1hInputTokens, prev.CacheReadInputTokens,
		prev.CachedPromptTokens, prev.CacheWritePromptTokens, prev.CacheWrite30mPromptTokens,
		prev.CacheWrite12hPromptTokens, prev.ReasoningTokens}
	for i := range a {
		if a[i] < b[i] {
			return true
		}
	}
	return false
}
