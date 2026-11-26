package modelusage

import (
	"bytes"
	"encoding/json"
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
// reads. Every other member is skipped without being decoded. withType reads the type member, which must then
// be a string or null; chat completions do not use it, so their chunks leave it unread whatever it holds.
func decodeEvent(data []byte, withType bool) (eventFields, error) {
	var ev eventFields
	if !json.Valid(data) {
		return ev, ErrMalformed
	}
	err := members(data, func(name, value []byte) error {
		switch string(name) {
		case "type":
			if !withType {
				return nil
			}
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
