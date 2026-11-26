package modelusage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validCases are inputs on which validJSON must agree with json.Valid.
var validCases = []string{
	``, ` `, `{}`, `[]`, `{"a":1}`, `{"a":[1,2,{"b":null}]}`, ` {"a" : true , "b":false} `, `"x"`, `0`, `-0`,
	`-0.5e+10`, `1E5`, `01`, `1.`, `.5`, `-`, `1e`, `+1`, `tru`, `nul`, `[1,]`, `{"a":1,}`, `{"a"}`, `{"a":}`,
	`{1:2}`, `[1 2]`, `{"a":1}{}`, `"é\n\"\\\/\b\f\r\t"`, `"\u12"`, `"\x"`, "\"a\x01\"", "\"\xff\xfe\"",
	`[[[[]]]]`, `[[[[]]]`, `{"a":{"b":{"c":{}}}}`, `"unterminated`, `{"a":1]`, `[1}`, `nulls`, "\t\n\r[1]\n",
}

func TestValidJSONMatchesEncodingJSON(t *testing.T) {
	for _, c := range validCases {
		if got, want := validJSON([]byte(c)), json.Valid([]byte(c)); got != want {
			t.Errorf("validJSON(%q) = %v, json.Valid = %v", c, got, want)
		}
	}
	// Deeper than encoding/json allows: still valid, and an unclosed one is still refused.
	deep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	if !validJSON([]byte(deep)) || validJSON([]byte(deep[:len(deep)-1])) {
		t.Error("validJSON does not handle deep nesting")
	}
}

func FuzzValidJSON(f *testing.F) {
	for _, c := range validCases {
		f.Add([]byte(c))
	}
	files, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range files {
		if strings.HasSuffix(e.Name(), ".json") {
			data, err := os.ReadFile(filepath.Join("testdata", e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// encoding/json refuses nesting past 10,000 levels; below that the two must agree.
		if bytes.Count(data, []byte("["))+bytes.Count(data, []byte("{")) >= 10000 {
			return
		}
		if got, want := validJSON(data), json.Valid(data); got != want {
			t.Fatalf("validJSON(%q) = %v, json.Valid = %v", data, got, want)
		}
	})
}
