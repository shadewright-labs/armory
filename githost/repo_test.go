package githost

import (
	"strings"
	"testing"
)

func TestObjectIDValid(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("a", 40):                    true,
		strings.Repeat("0", 64):                    false,
		strings.Repeat("0", 40):                    false,
		strings.Repeat("0", 39) + "1":              true,
		"1" + strings.Repeat("0", 63):              true,
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

// TestObjectIDZero: the all-zero ID, which hosts read as "create" or "delete" a ref, is never valid.
func TestObjectIDZero(t *testing.T) {
	cases := map[string]bool{
		strings.Repeat("0", 40):       true,
		strings.Repeat("0", 64):       true,
		strings.Repeat("0", 39) + "1": false,
		strings.Repeat("0", 41):       false,
		"":                            false,
		"0":                           false,
	}
	for id, want := range cases {
		if got := ObjectID(id).IsZero(); got != want {
			t.Errorf("ObjectID(%q).IsZero() = %v, want %v", id, got, want)
		}
		if want && ObjectID(id).Valid() {
			t.Errorf("ObjectID(%q).Valid() = true for the zero ID", id)
		}
	}
}
