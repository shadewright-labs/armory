package modelusage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var fuzzLimits = Limits{MaxBytes: 8 << 10, MaxLineBytes: 2 << 10, MaxTokens: 1 << 40}

// countingReader counts the bytes a parser reads.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// addSeeds adds every fixture whose name ends in suffix, under each family.
func addSeeds(f *testing.F, suffix string) {
	f.Helper()
	files, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range files {
		if !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			f.Fatal(err)
		}
		for fam := range uint8(3) {
			f.Add(fam, data)
		}
	}
}

// checkResult fails unless reading stayed within the limit and the result keeps the rules of Usage.
func checkResult(t *testing.T, cr *countingReader, u Usage, err error, fam Family) {
	t.Helper()
	if cr.n > fuzzLimits.MaxBytes+1 {
		t.Fatalf("read %d bytes past the limit of %d", cr.n, fuzzLimits.MaxBytes)
	}
	if err != nil && !errors.Is(err, ErrTruncated) && u != (Usage{}) {
		t.Fatalf("usage %+v returned with %v", u, err)
	}
	var te *TruncatedError
	if errors.As(err, &te) && te.Partial != (u != Usage{}) {
		t.Fatalf("usage %+v returned with Partial %v", u, te.Partial)
	}
	if u == (Usage{}) {
		return
	}
	if u.Family != fam {
		t.Fatalf("family %v, want %v", u.Family, fam)
	}
	if verr := u.validate(fuzzLimits.MaxTokens); verr != nil || u.UncachedInputTokens() < 0 {
		t.Fatalf("usage %+v breaks its rules: %v", u, verr)
	}
}

func FuzzParseJSON(f *testing.F) {
	addSeeds(f, ".json")
	f.Fuzz(func(t *testing.T, fam uint8, data []byte) {
		family := Family(fam%3) + Anthropic
		cr := &countingReader{r: bytes.NewReader(data)}
		u, err := ParseJSON(family, cr, fuzzLimits)
		checkResult(t, cr, u, err, family)
		if int64(len(data)) > fuzzLimits.MaxBytes && !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a reply of %d bytes gave %v, want ErrTooLarge", len(data), err)
		}
	})
}

func FuzzParseStream(f *testing.F) {
	addSeeds(f, ".sse")
	f.Fuzz(func(t *testing.T, fam uint8, data []byte) {
		family := Family(fam%3) + Anthropic
		cr := &countingReader{r: bytes.NewReader(data)}
		u, err := ParseStream(family, cr, fuzzLimits)
		checkResult(t, cr, u, err, family)
	})
}
