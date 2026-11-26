package modelusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMembers(t *testing.T) {
	many := func(dupAt int) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range 2 * maxStackNames {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"k%d":%d`, i, i)
		}
		if dupAt >= 0 {
			fmt.Fprintf(&b, `,"k%d":0`, dupAt)
		}
		b.WriteString(`,"usage":1}`)
		return b.String()
	}
	tests := []struct {
		name    string
		data    string
		want    string // the raw value of the member usage
		wantErr error
	}{
		{name: "plain", data: `{"a":1,"usage":{"x":[1,"}"]}}`, want: `{"x":[1,"}"]}`},
		{name: "escaped name matches", data: `{"usage":2}`, want: `2`},
		{name: "escaped quote in a skipped string", data: `{"a":"x\"}\\","usage":3}`, want: `3`},
		{name: "backslashes before a quote", data: `{"a":"\\\\","usage":4}`, want: `4`},
		{name: "case differs", data: `{"Usage":5}`},
		{name: "whitespace", data: " {\n \"usage\" :\t6 , \"b\" : [ ] } ", want: `6`},
		{name: "empty", data: `{}`},
		{name: "duplicate", data: `{"usage":1,"usage":2}`, wantErr: ErrMalformed},
		{name: "duplicate through an escape", data: `{"usage":1,"usage":2}`, wantErr: ErrMalformed},
		{name: "duplicate through invalid UTF-8", data: "{\"a\xff\":1,\"a\xfe\":2}", wantErr: ErrMalformed},
		{name: "duplicate in a nested object is not checked", data: `{"a":{"b":1,"b":2},"usage":7}`, want: `7`},
		{name: "many names", data: many(-1), want: `1`},
		{name: "duplicate among many names, early", data: many(3), wantErr: ErrMalformed},
		{name: "duplicate among many names, late", data: many(2*maxStackNames - 1), wantErr: ErrMalformed},
		{name: "not an object", data: `[1]`, wantErr: ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			err := members([]byte(tt.data), func(name, value []byte) error {
				if string(name) == "usage" {
					got = string(value)
				}
				return nil
			})
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("usage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestElements(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    []string
		wantErr error
	}{
		{name: "null array", data: `null`},
		{name: "empty array", data: `[]`},
		{name: "whitespace empty", data: " [ \n\t ] "},
		{name: "one object", data: `[{"type":"compaction","input_tokens":8}]`,
			want: []string{`{"type":"compaction","input_tokens":8}`}},
		{name: "mixed values", data: `[{"a":[1,2]}, "x]y", 3, null]`,
			want: []string{`{"a":[1,2]}`, `"x]y"`, `3`, `null`}},
		{name: "not an array", data: `{"a":1}`, wantErr: ErrMalformed},
		{name: "number", data: `42`, wantErr: ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			err := elements([]byte(tt.data), func(elem []byte) error {
				got = append(got, string(elem))
				return nil
			})
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("elements = %q, want %q", got, tt.want)
			}
		})
	}
}

// referenceMembers decodes the object data with encoding/json token by token, as the parser did before it
// skipped values, and returns its members in order with ErrMalformed for a repeated name.
func referenceMembers(data []byte) ([][2]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, ErrMalformed
	}
	var out [][2]string
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		name, _ := tok.(string)
		if seen[name] {
			return nil, ErrMalformed
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, ErrMalformed
		}
		out = append(out, [2]string{name, string(raw)})
	}
	return out, nil
}

// FuzzMembers checks members against encoding/json on every valid JSON object: the same names, the same
// values once compacted, and the same refusal of a repeated name.
func FuzzMembers(f *testing.F) {
	for _, s := range []string{`{"a":1,"b":[{"c":"}"}],"ab":null}`, `{"x":"\\\"","y":-1.5e3}`, `{}`,
		`{"a":1,"a":2}`, "{\"a\xff\":1,\"a\xfe\":2}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if !json.Valid(data) {
			return
		}
		want, wantErr := referenceMembers(data)
		var got [][2]string
		err := members(data, func(name, value []byte) error {
			var b bytes.Buffer
			if cerr := json.Compact(&b, value); cerr != nil {
				t.Fatalf("value %q is not one JSON value: %v", value, cerr)
			}
			got = append(got, [2]string{string(name), b.String()})
			return nil
		})
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if err != nil {
			return
		}
		for i := range want {
			var b bytes.Buffer
			_ = json.Compact(&b, []byte(want[i][1]))
			want[i][1] = b.String()
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("members = %q, want %q", got, want)
		}
	})
}
