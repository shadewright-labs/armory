package modelusage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ParseJSON reads a complete JSON reply of family f from r and returns its usage. It reads at most
// lim.MaxBytes bytes and one more to tell a reply of exactly that size from a longer one.
//
// A byte order mark at the start is skipped. The reply must be one JSON object with a top-level usage block
// holding both the input and the output count of the family: input_tokens and output_tokens (Anthropic,
// Responses) or prompt_tokens and completion_tokens (chat completions). Other counts are optional and absent
// or null ones are zero.
//
// Errors: ErrInvalidLimits, a *LimitError matching ErrTooLarge, ErrMalformed, ErrNoUsage, ErrOutOfRange, or
// the reader's own error wrapped. On error the Usage is zero.
func ParseJSON(f Family, r io.Reader, lim Limits) (Usage, error) {
	if err := lim.check(f, false); err != nil {
		return Usage{}, err
	}
	br := &boundedReader{r: r, max: lim.MaxBytes}
	data, err := io.ReadAll(br)
	if br.over() {
		return Usage{}, &LimitError{Limit: lim.MaxBytes}
	}
	if err != nil {
		return Usage{}, fmt.Errorf("modelusage: reading the reply: %w", err)
	}
	data = bytes.TrimPrefix(data, byteOrderMark)
	if !json.Valid(data) {
		return Usage{}, ErrMalformed
	}
	var block []byte
	err = members(data, func(name, value []byte) error {
		if string(name) == nameUsage {
			block = value
		}
		return nil
	})
	if err != nil {
		return Usage{}, err
	}
	return usageOf(block, f, lim.MaxTokens)
}

// usageOf converts the complete raw usage block v of family f, which may be nil or null.
func usageOf(v []byte, f Family, maxTokens int64) (Usage, error) {
	if f == Anthropic {
		au, err := readAnthropicUsage(v)
		if err != nil {
			return Usage{}, err
		}
		return anthropicOf(au, maxTokens)
	}
	ou, err := readOpenAIUsage(v, f)
	if err != nil {
		return Usage{}, err
	}
	return openAIOf(ou, f, maxTokens)
}

// ParseStream reads a streamed reply of family f, sent as server-sent events, from r and returns its usage.
// It reads at most lim.MaxBytes bytes in all (and one more, to tell the limit apart from a longer stream) and
// refuses a line, or the joined data of one event, longer than lim.MaxLineBytes. Lines end in LF, CRLF or a
// bare CR, and a byte order mark at the start of the stream is skipped, as the server-sent events format
// allows. Comment lines and fields other than data are ignored; the data lines of one event are joined with
// LF and decoded as one JSON value. Events of unknown types are ignored.
//
// After the final event, a second final event, or an event that would change the counts, is ErrMalformed.
// Other events after it are ignored, including an error event, since the counts are already final: the
// Anthropic stream may send ping events at any point, and the Responses API reference does not say that
// the final event is the last one sent. Chat completions are stricter: their reference ends the stream with
// [DONE], so any event after it is ErrMalformed.
//
// Which usage counts, per family:
//
//   - Anthropic: message_start carries the first usage block in message.usage, and each message_delta a
//     usage block whose counts are cumulative. A count present in a later block replaces the earlier one,
//     and a count absent or null keeps it. The final event is message_stop. A second message_start is
//     ErrMalformed. Without input_tokens and output_tokens at the end, the result is ErrNoUsage.
//   - OpenAIChat: usage arrives in a chunk with a non-null usage field (with stream_options.include_usage,
//     the last chunk before [DONE]). When more than one chunk carries usage, the last one wins, since each
//     reports the whole request so far. The final event is data: [DONE]. A stream that ends with [DONE] and
//     no usage is ErrNoUsage.
//   - OpenAIResponses: usage is read only from the response.usage of the final event, which is one of
//     response.completed, response.incomplete or response.failed; usage in earlier events is ignored. A
//     second final event is ErrMalformed, because the counts of two responses cannot be told apart. A [DONE]
//     line after the final event, which servers that copy chat completions send, is ignored.
//
// The Usage is final only when the error is nil. A stream that ends before its final event, whose reader
// fails after at least one usage block, or that carries an error event (Anthropic and Responses type error,
// or a chat chunk with an error field) returns a *TruncatedError, which matches ErrTruncated and wraps the
// reader's error if there was one. With it comes the usage read so far, the counts of the last complete
// usage block, which can be lower than what the provider bills: TruncatedError.Partial tells whether there
// is any. An event cut off by the end of the stream counts as missing, not malformed. A reader error after
// the final event does not change the result, since the counts are already final.
//
// Other errors: ErrInvalidLimits, a *LimitError matching ErrTooLarge or ErrLineTooLong, ErrMalformed,
// ErrNoUsage, ErrOutOfRange, or the reader's own error wrapped when it fails before any usage block. With
// these the Usage is zero.
func ParseStream(f Family, r io.Reader, lim Limits) (Usage, error) {
	if err := lim.check(f, true); err != nil {
		return Usage{}, err
	}
	br := &boundedReader{r: r, max: lim.MaxBytes}
	sc := bufio.NewScanner(br)
	// The buffer holds a line and its line ending, which scanLines needs to see to return the line, and the
	// byte order mark that may lead the first line.
	sc.Buffer(make([]byte, 0, min(4096, lim.MaxLineBytes+5)), lim.MaxLineBytes+5)
	sc.Split(scanLines)
	st := &streamState{family: f, maxTokens: lim.MaxTokens}
	var data []byte
	pending, first := false, true
	for sc.Scan() {
		if br.over() {
			return Usage{}, &LimitError{Limit: lim.MaxBytes}
		}
		line := sc.Bytes()
		if first {
			line, first = bytes.TrimPrefix(line, byteOrderMark), false
		}
		if len(line) > lim.MaxLineBytes {
			return Usage{}, &LimitError{Line: true, Limit: int64(lim.MaxLineBytes)}
		}
		if len(line) == 0 {
			if pending {
				if err := st.event(data); err != nil {
					return st.fail(err)
				}
				data, pending = data[:0], false
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if string(name) != "data" {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if pending {
			data = append(data, '\n')
		}
		data = append(data, value...)
		pending = true
		// The joined data of one event is held to the line limit too, so that many data lines cannot make one
		// event as large as the whole reply.
		if len(data) > lim.MaxLineBytes {
			return Usage{}, &LimitError{Line: true, Limit: int64(lim.MaxLineBytes)}
		}
	}
	if br.over() {
		return Usage{}, &LimitError{Limit: lim.MaxBytes}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Usage{}, &LimitError{Line: true, Limit: int64(lim.MaxLineBytes)}
		}
		switch {
		case st.done:
			// The final event was read, so the counts are final: a cut after it loses nothing.
			return st.finish()
		case st.seen:
			return st.truncated(err)
		}
		return Usage{}, fmt.Errorf("modelusage: reading the stream: %w", err)
	}
	if pending {
		// The stream ended without the blank line that closes an event. A complete event still counts; a cut
		// one is the sign of a cut stream, and after the final event it changes nothing.
		wasDone := st.done
		if err := st.event(data); err != nil {
			switch {
			case errors.Is(err, ErrMalformed) && wasDone:
			case errors.Is(err, ErrMalformed):
				return st.fail(ErrTruncated)
			default:
				return st.fail(err)
			}
		}
	}
	return st.finish()
}

// byteOrderMark is the UTF-8 byte order mark, skipped at the start of a reply or a stream.
var byteOrderMark = []byte("\xEF\xBB\xBF")

// boundedReader reads at most max+1 bytes from r, so that over tells a reply longer than max.
type boundedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n > b.max {
		return 0, io.EOF
	}
	// max is below MaxInt64 (Limits.check), so left is at least one and cannot overflow.
	if left := b.max - b.n + 1; int64(len(p)) > left {
		p = p[:left]
	}
	k, err := b.r.Read(p)
	b.n += int64(k)
	return k, err
}

func (b *boundedReader) over() bool { return b.n > b.max }

// streamState follows the events of one stream.
type streamState struct {
	family    Family
	maxTokens int64
	started   bool           // Anthropic: message_start seen
	acc       anthropicUsage // Anthropic: the counts so far, each replaced by a later present one
	usage     Usage          // OpenAI: the last complete usage block
	seen      bool           // a usage block was read
	done      bool           // the final event was read
}

// event handles the data of one event. After the final event, an event that would change the counts or end
// the reply a second time is ErrMalformed, and any other is ignored, except in chat completions, whose
// [DONE] ends the stream.
func (s *streamState) event(data []byte) error {
	switch s.family {
	case Anthropic:
		return s.anthropicEvent(data)
	case OpenAIChat:
		return s.chatEvent(data)
	default:
		return s.responsesEvent(data)
	}
}

// Names the parser matches in more than one place.
const (
	nameUsage = "usage"
	typeError = "error"
)

// eventFields are the members of an event that the parser reads, raw, nil when absent.
type eventFields struct {
	typ      []byte // type, decoded
	message  []byte // Anthropic message_start: message
	usage    []byte // Anthropic message_delta, chat chunk: usage
	response []byte // Responses final events: response
	err      []byte // chat chunk: error
}

// decodeEvent decodes the data of one event, which must be one JSON object, and returns the members the parser
// reads. Every other member is skipped without being decoded.
func decodeEvent(data []byte) (eventFields, error) {
	var ev eventFields
	if !json.Valid(data) {
		return ev, ErrMalformed
	}
	err := members(data, func(name, value []byte) error {
		switch string(name) {
		case "type":
			t, err := str(value)
			ev.typ = t
			return err
		case "message":
			ev.message = value
		case nameUsage:
			ev.usage = value
		case "response":
			ev.response = value
		case typeError:
			ev.err = value
		}
		return nil
	})
	return ev, err
}

// usageMember returns the raw usage member of the raw object v, nil when v or the member is absent or null. A
// v that is not an object or null is ErrMalformed.
func usageMember(v []byte) ([]byte, error) {
	var block []byte
	err := object(v, func(name, value []byte) error {
		if string(name) == nameUsage {
			block = value
		}
		return nil
	})
	return block, err
}

func (s *streamState) anthropicEvent(data []byte) error {
	ev, err := decodeEvent(data)
	if err != nil {
		return err
	}
	switch string(ev.typ) {
	case "message_start":
		if ev.message == nil || isNull(ev.message) {
			return ErrMalformed
		}
		block, err := usageMember(ev.message)
		if err != nil {
			return err
		}
		if s.started {
			return ErrMalformed
		}
		s.started = true
		return s.merge(block)
	case "message_delta":
		if !s.started || s.done {
			return ErrMalformed
		}
		return s.merge(ev.usage)
	case "message_stop":
		if !s.started || s.done {
			return ErrMalformed
		}
		s.done = true
	case typeError:
		if s.done {
			return nil
		}
		return ErrTruncated
	}
	return nil
}

// merge replaces each count of the Anthropic total that the raw usage block v carries.
func (s *streamState) merge(v []byte) error {
	au, err := readAnthropicUsage(v)
	if err != nil || au == nil {
		return err
	}
	from := au.counts()
	if err := firstErr(from...); err != nil {
		return err
	}
	for i, to := range s.acc.counts() {
		if from[i].set {
			*to = *from[i]
		}
	}
	s.seen = true
	return nil
}

func (s *streamState) chatEvent(data []byte) error {
	if s.done {
		return ErrMalformed
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		s.done = true
		return nil
	}
	ev, err := decodeEvent(data)
	if err != nil {
		return err
	}
	if ev.err != nil && !isNull(ev.err) {
		return ErrTruncated
	}
	if ev.usage == nil || isNull(ev.usage) {
		return nil
	}
	u, err := usageOf(ev.usage, OpenAIChat, s.maxTokens)
	if err != nil {
		return err
	}
	s.usage, s.seen = u, true
	return nil
}

func (s *streamState) responsesEvent(data []byte) error {
	if s.done && bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return nil
	}
	ev, err := decodeEvent(data)
	if err != nil {
		return err
	}
	switch string(ev.typ) {
	case "response.completed", "response.incomplete", "response.failed":
		if s.done {
			return ErrMalformed
		}
		if ev.response == nil || isNull(ev.response) {
			return ErrMalformed
		}
		block, err := usageMember(ev.response)
		if err != nil {
			return err
		}
		u, err := usageOf(block, OpenAIResponses, s.maxTokens)
		if err != nil {
			return err
		}
		s.usage, s.seen, s.done = u, true, true
	case typeError:
		if s.done {
			return nil
		}
		return ErrTruncated
	}
	return nil
}

// fail returns err with a zero Usage, or a *TruncatedError with the usage read so far when err is
// ErrTruncated.
func (s *streamState) fail(err error) (Usage, error) {
	if !errors.Is(err, ErrTruncated) {
		return Usage{}, err
	}
	return s.truncated(nil)
}

// truncated returns the usage read so far with a *TruncatedError that wraps readErr, which may be nil.
func (s *streamState) truncated(readErr error) (Usage, error) {
	u := s.partial()
	return u, &TruncatedError{Partial: u != (Usage{}), Err: readErr}
}

// partial is the usage read so far, or zero when none was read or it breaks a rule of Usage.
func (s *streamState) partial() Usage {
	if !s.seen {
		return Usage{}
	}
	if s.family != Anthropic {
		return s.usage
	}
	u := s.acc.usage()
	if u.validate(s.maxTokens) != nil {
		return Usage{}
	}
	return u
}

// finish returns the usage of a stream that ended.
func (s *streamState) finish() (Usage, error) {
	if !s.done {
		return s.fail(ErrTruncated)
	}
	if s.family == Anthropic {
		return anthropicOf(&s.acc, s.maxTokens)
	}
	if !s.seen {
		return Usage{}, ErrNoUsage
	}
	return s.usage, nil
}

// scanLines is a bufio.SplitFunc for server-sent events: a line ends in CRLF, LF or a bare CR, and the line
// ending is not part of the line.
func scanLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		switch {
		case data[i] == '\n':
			return i + 1, data[:i], nil
		case i+1 < len(data) && data[i+1] == '\n':
			return i + 2, data[:i], nil
		case i+1 < len(data) || atEOF:
			return i + 1, data[:i], nil
		}
		// A CR at the end of what was read so far: the next byte tells CRLF from a bare CR.
		return 0, nil, nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
