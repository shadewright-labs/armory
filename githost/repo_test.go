package githost

import (
	"strings"
	"testing"
)

func TestObjectIDValid(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("a", 40):                    true,
		strings.Repeat("0", 64):                    true,
		"0123456789abcdef0123456789abcdef01234567": true,
		strings.Repeat("a", 39):                    false,
		strings.Repeat("a", 41):                    false,
		strings.Repeat("a", 63):                    false,
		strings.Repeat("A", 40):                    false,
		strings.Repeat("g", 40):                    false,
		"":                                         false,
		"main":                                     false,
		"abc1234":                                  false,
		strings.Repeat("a", 39) + " ":              false,
		strings.Repeat("a", 39) + "\x00":           false,
	}
	for id, want := range cases {
		if got := ObjectID(id).Valid(); got != want {
			t.Errorf("ObjectID(%q).Valid() = %v, want %v", id, got, want)
		}
	}
}
