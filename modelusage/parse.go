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
// The reply must be one JSON object with a top-level usage block holding both the input and the output
// count of the family: input_tokens and output_tokens (Anthropic, Responses) or prompt_tokens and
// completion_tokens (chat completions). Other counts are optional and absent or null ones are zero.
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
	if f == Anthropic {
		var reply struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if err := decodeObject(data, &reply); err != nil {
			return Usage{}, err
		}
		return anthropicOf(reply.Usage, lim.MaxTokens)
	}
	var reply struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := decodeObject(data, &reply); err != nil {
		return Usage{}, err
	}
	return openAIOf(reply.Usage, f, lim.MaxTokens)
}

// ParseStream reads a streamed reply of family f, sent as server-sent events, from r and returns its usage.
// It reads at most lim.MaxBytes bytes in all (and one more, to tell the limit apart from a longer stream) and
// refuses a line longer than lim.MaxLineBytes. Lines end in LF or CRLF. Comment lines and fields other than
// data are ignored; the data lines of one event are joined and decoded as one JSON value. Events of unknown
// types are ignored. An event after the final one is ErrMalformed.
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
//     second final event is ErrMalformed, because the counts of two responses cannot be told apart.
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
	// The buffer holds a line and its line ending, which ScanLines needs to see to return the line.
	sc.Buffer(make([]byte, 0, min(4096, lim.MaxLineBytes+2)), lim.MaxLineBytes+2)
	st := &streamState{family: f, maxTokens: lim.MaxTokens}
	var data []byte
	pending := false
	for sc.Scan() {
		if br.over() {
			return Usage{}, &LimitError{Limit: lim.MaxBytes}
		}
		line := sc.Bytes()
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
	if pending && !st.done {
		// The stream ended without the blank line that closes an event. A complete event still counts; a cut
		// one is the sign of a cut stream.
		if err := st.event(data); err != nil {
			if errors.Is(err, ErrMalformed) {
				return st.fail(ErrTruncated)
			}
			return st.fail(err)
		}
	} else if pending {
		return Usage{}, ErrMalformed
	}
	return st.finish()
}

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

// event handles the data of one event.
func (s *streamState) event(data []byte) error {
	if s.done {
		return ErrMalformed
	}
	switch s.family {
	case Anthropic:
		return s.anthropicEvent(data)
	case OpenAIChat:
		return s.chatEvent(data)
	default:
		return s.responsesEvent(data)
	}
}

func (s *streamState) anthropicEvent(data []byte) error {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if err := decodeObject(data, &ev); err != nil {
		return err
	}
	switch ev.Type {
	case "message_start":
		if s.started || ev.Message == nil {
			return ErrMalformed
		}
		s.started = true
		return s.merge(ev.Message.Usage)
	case "message_delta":
		if !s.started {
			return ErrMalformed
		}
		return s.merge(ev.Usage)
	case "message_stop":
		if !s.started {
			return ErrMalformed
		}
		s.done = true
	case "error":
		return ErrTruncated
	}
	return nil
}

// merge replaces each count of the Anthropic total that the block carries.
func (s *streamState) merge(au *anthropicUsage) error {
	if au == nil {
		return nil
	}
	if err := firstErr(au.InputTokens, au.OutputTokens, au.CacheCreationInputTokens, au.CacheReadInputTokens); err != nil {
		return err
	}
	for _, p := range []struct{ from, to *count }{
		{&au.InputTokens, &s.acc.InputTokens},
		{&au.OutputTokens, &s.acc.OutputTokens},
		{&au.CacheCreationInputTokens, &s.acc.CacheCreationInputTokens},
		{&au.CacheReadInputTokens, &s.acc.CacheReadInputTokens},
	} {
		if p.from.set {
			*p.to = *p.from
		}
	}
	s.seen = true
	return nil
}

func (s *streamState) chatEvent(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		s.done = true
		return nil
	}
	var chunk struct {
		Usage *openAIUsage    `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := decodeObject(data, &chunk); err != nil {
		return err
	}
	if len(chunk.Error) > 0 && !bytes.Equal(chunk.Error, []byte("null")) {
		return ErrTruncated
	}
	if chunk.Usage == nil {
		return nil
	}
	u, err := openAIOf(chunk.Usage, OpenAIChat, s.maxTokens)
	if err != nil {
		return err
	}
	s.usage, s.seen = u, true
	return nil
}

func (s *streamState) responsesEvent(data []byte) error {
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if err := decodeObject(data, &ev); err != nil {
		return err
	}
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		if ev.Response == nil {
			return ErrMalformed
		}
		u, err := openAIOf(ev.Response.Usage, OpenAIResponses, s.maxTokens)
		if err != nil {
			return err
		}
		s.usage, s.seen, s.done = u, true, true
	case "error":
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
	u := Usage{
		Family:                   Anthropic,
		InputTokens:              s.acc.InputTokens.n,
		OutputTokens:             s.acc.OutputTokens.n,
		CacheCreationInputTokens: s.acc.CacheCreationInputTokens.n,
		CacheReadInputTokens:     s.acc.CacheReadInputTokens.n,
	}
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
