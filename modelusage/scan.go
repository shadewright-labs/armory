package modelusage

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// The functions below walk JSON that validJSON has already accepted (a whole reply, or the data of one stream
// event), so they only find where each value ends. They never build the members of an object: a caller is
// handed each name and raw value in turn and keeps the few it reads, and every other value is skipped.

// maxStackNames is how many member names of one object members compares without a map.
const maxStackNames = 32

// members calls fn with the name and raw value of each member of the object data, in order. The name is
// decoded (escapes and invalid UTF-8 as encoding/json reads them), so it matches exactly, case included. A
// value that is not an object is ErrMalformed, and so is a name that appears twice in the object. fn returning
// an error stops the walk with that error. data must be valid JSON.
func members(data []byte, fn func(name, value []byte) error) error {
	i := skipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return ErrMalformed
	}
	var (
		stack [maxStackNames][]byte
		names = stack[:0]
		more  map[string]struct{}
	)
	i = skipSpace(data, i+1)
	if i < len(data) && data[i] == '}' {
		return nil
	}
	for i < len(data) && data[i] == '"' {
		end := skipString(data, i)
		name := data[i+1 : end-1]
		if bytes.IndexByte(name, '\\') >= 0 || !utf8.Valid(name) {
			var s string
			if json.Unmarshal(data[i:end], &s) != nil {
				return ErrMalformed
			}
			name = []byte(s)
		}
		switch {
		case more != nil:
			if _, dup := more[string(name)]; dup {
				return ErrMalformed
			}
			more[string(name)] = struct{}{}
		default:
			for _, seen := range names {
				if bytes.Equal(seen, name) {
					return ErrMalformed
				}
			}
			if len(names) < cap(names) {
				names = append(names, name)
				break
			}
			more = make(map[string]struct{}, 2*maxStackNames)
			for _, seen := range names {
				more[string(seen)] = struct{}{}
			}
			more[string(name)] = struct{}{}
		}
		i = skipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return ErrMalformed
		}
		i = skipSpace(data, i+1)
		vend := skipValue(data, i)
		if vend <= i {
			return ErrMalformed
		}
		if err := fn(name, data[i:vend]); err != nil {
			return err
		}
		i = skipSpace(data, vend)
		if i < len(data) && data[i] == '}' {
			return nil
		}
		if i >= len(data) || data[i] != ',' {
			return ErrMalformed
		}
		i = skipSpace(data, i+1)
	}
	return ErrMalformed
}

// object calls members on value, or does nothing when value is null. Any value but an object or null is
// ErrMalformed.
func object(value []byte, fn func(name, value []byte) error) error {
	if isNull(value) {
		return nil
	}
	return members(value, fn)
}

// isNull reports whether the raw value is null.
func isNull(value []byte) bool { return string(value) == "null" }

// str returns the raw value as a string, empty when it is null, without copying it when it has no escape. Any
// value but a string or null is ErrMalformed.
func str(value []byte) ([]byte, error) {
	switch {
	case isNull(value):
		return nil, nil
	case len(value) < 2 || value[0] != '"':
		return nil, ErrMalformed
	case bytes.IndexByte(value, '\\') < 0 && utf8.Valid(value):
		// No escape and valid UTF-8: the bytes between the quotes are the string itself.
		return value[1 : len(value)-1], nil
	}
	var s string
	if json.Unmarshal(value, &s) != nil {
		return nil, ErrMalformed
	}
	return []byte(s), nil
}

func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index after the string that starts with the quote at data[i], or len(data) when it
// does not end.
func skipString(data []byte, i int) int {
	for j := i + 1; j < len(data); {
		k := bytes.IndexByte(data[j:], '"')
		if k < 0 {
			break
		}
		q := j + k
		backslashes := 0
		for b := q - 1; b > i && data[b] == '\\'; b-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
	return len(data)
}

// skipValue returns the index after the value that starts at data[i].
func skipValue(data []byte, i int) int {
	if i >= len(data) {
		return i
	}
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				j = skipString(data, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return len(data)
	}
	j := i
	for j < len(data) {
		switch data[j] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return j
		}
		j++
	}
	return j
}
