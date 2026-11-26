package modelusage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file check that a ParseStream result depends only on the bytes, the limits and the
// reader's final error, never on how the reader splits the bytes into reads, and the lower-bound rule under
// every byte limit:
//
//   - Chunking: the same input read 1 byte at a time, 2 at a time, in random sizes, and all in one read
//     returned together with the end, gives the same Usage, the same result class (final, trailing, partial,
//     none) and the same matching sentinels.
//   - Lower bound: let P be the longest part of the input that ends after an event (a blank line) and is no
//     longer than MaxBytes, read under the same Limits. If ParseStream gives P a final usage (a nil error or
//     a *TrailingError), the input gives exactly P's Usage, with a nil error or a *TrailingError. Otherwise
//     the input's result is not final, since a final event counts only when it ends within the limit, and
//     unless its error matches ErrInconsistent, every count of its Usage is at least the same count of P's.
//     So an event that ends within MaxBytes is never lost to a byte the reader returned past the limit, and
//     one cut by the limit, even between the CR and LF of its blank line, is never final. The rule is checked
//     on inputs whose line ends are all LF, all CR LF or all CR, where a blank line is "\n\n", "\r\n\r\n"
//     or "\r\r".

// chunkReader returns data in reads whose sizes come from size, then err. With together set, the read that
// returns the last bytes returns err with them.
type chunkReader struct {
	data     []byte
	size     func() int
	err      error
	together bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, c.err
	}
	n := min(max(c.size(), 1), len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	if c.together && len(c.data) == 0 {
		return n, c.err
	}
	return n, nil
}

// chunkings are the ways the property splits an input. The random one is seeded, so a failure repeats.
func chunkings(seed uint64) map[string]func(data []byte, err error) io.Reader {
	return map[string]func([]byte, error) io.Reader{
		"1 byte": func(d []byte, err error) io.Reader {
			return &chunkReader{data: d, size: func() int { return 1 }, err: err}
		},
		"2 bytes": func(d []byte, err error) io.Reader {
			return &chunkReader{data: d, size: func() int { return 2 }, err: err}
		},
		"random": func(d []byte, err error) io.Reader {
			rng := rand.New(rand.NewPCG(seed, uint64(len(d)))) //nolint:gosec // G404: seeded, so a failure repeats
			return &chunkReader{data: d, size: func() int { return 1 + rng.IntN(17) }, err: err}
		},
		"all at once": func(d []byte, err error) io.Reader {
			return &chunkReader{data: d, size: func() int { return len(d) }, err: err, together: true}
		},
	}
}

// outcome is what the chunking property compares: the Usage, the class and the sentinels the error matches.
func outcome(t testing.TB, u Usage, err error) string {
	t.Helper()
	var matched []string
	for _, s := range []struct {
		name string
		err  error
	}{{"truncated", ErrTruncated}, {"malformed", ErrMalformed}, {"inconsistent", ErrInconsistent},
		{"range", ErrOutOfRange}, {"nousage", ErrNoUsage}, {"toolarge", ErrTooLarge}, {"line", ErrLineTooLong},
		{"cut", errChunkCut}} {
		if errors.Is(err, s.err) {
			matched = append(matched, s.name)
		}
	}
	return fmt.Sprintf("%+v %s %v", u, resultClass(t, err), matched)
}

// errChunkCut is the reader error of the property's failing readers.
var errChunkCut = errors.New("connection reset")

// blankLine returns how a blank line looks in data when all its line ends are of one kind, and false for
// mixed line ends or a leading byte order mark.
func blankLine(data []byte) (string, bool) {
	hasCR, hasLF := bytes.IndexByte(data, '\r') >= 0, bytes.IndexByte(data, '\n') >= 0
	switch {
	case bytes.HasPrefix(data, byteOrderMark):
		return "", false
	case !hasCR:
		return "\n\n", true
	case !hasLF:
		return "\r\r", true
	case bytes.Count(data, []byte("\r")) == bytes.Count(data, []byte("\r\n")) &&
		bytes.Count(data, []byte("\n")) == bytes.Count(data, []byte("\r\n")):
		return "\r\n\r\n", true
	}
	return "", false
}

// endsAfter returns the offsets just after each blank line sep in data.
func endsAfter(data []byte, sep string) []int {
	var ends []int
	for i := 0; ; {
		k := bytes.Index(data[i:], []byte(sep))
		if k < 0 {
			return ends
		}
		i += k + len(sep)
		ends = append(ends, i)
	}
}

// lastEventEnd returns the length of the longest part of data that ends after a blank line sep and is at most
// n bytes long, or 0.
func lastEventEnd(data []byte, sep string, n int64) int {
	best := 0
	for _, end := range endsAfter(data, sep) {
		if int64(end) > n {
			break
		}
		best = end
	}
	return best
}

// checkChunking runs data through every chunking under lim, ending in each kind of reader end, and
// checks that the outcomes agree and keep the lower-bound rule.
func checkChunking(t *testing.T, f Family, data []byte, lim Limits, seed uint64) {
	t.Helper()
	sep, lfOnly := blankLine(data)
	var prefix Usage
	var prefixFinal bool
	if lfOnly {
		// The same Limits: the property holds for them only.
		u, err := ParseStream(f, bytes.NewReader(data[:lastEventEnd(data, sep, lim.MaxBytes)]), lim)
		prefix, prefixFinal = u, resultClass(t, err) == classFinal || resultClass(t, err) == classTrailing
	}
	// The reader ends in io.EOF, in a failure, in a failure that joins io.EOF, or in a typed nil error.
	for _, end := range []error{io.EOF, errChunkCut, errors.Join(io.EOF, errChunkCut), (*PartialError)(nil)} {
		want, wantFrom := "", ""
		for name, reader := range chunkings(seed) {
			u, err := ParseStream(f, reader(data, end), lim)
			got := outcome(t, u, err)
			if want == "" {
				want, wantFrom = got, name
			} else if got != want {
				t.Fatalf("%v, MaxBytes %d, MaxLineBytes %d, end %v, input %q:\n%s: %s\n%s: %s", f, lim.MaxBytes,
					lim.MaxLineBytes, end, data, wantFrom, want, name, got)
			}
			if !lfOnly {
				continue
			}
			if prefixFinal {
				if u != prefix || (err != nil && resultClass(t, err) != classTrailing) {
					t.Fatalf("%v, MaxBytes %d, input %q: %+v (err %v), want the final usage %+v of the prefix",
						f, lim.MaxBytes, data, u, err, prefix)
				}
				continue
			}
			if class := resultClass(t, err); class == classFinal || class == classTrailing {
				t.Fatalf("%v, MaxBytes %d, input %q: final usage %+v (err %v), but the longest event prefix within "+
					"the limit has no final event", f, lim.MaxBytes, data, u, err)
			}
			if errors.Is(err, ErrInconsistent) {
				continue
			}
			got64, low := u.countList(), prefix.countList()
			for i := range low {
				if got64[i] < low[i] {
					t.Fatalf("%v, MaxBytes %d, input %q: %+v (err %v) is less than %+v of the prefix", f,
						lim.MaxBytes, data, u, err, prefix)
				}
			}
		}
	}
}

// chunkingInput is one input of the property, with the line limit it is read under.
type chunkingInput struct {
	name   string
	family Family
	data   []byte
	line   int
}

// chunkingInputs gathers every stream fixture, every inline and regression stream, and a few line end and byte
// order mark variants.
func chunkingInputs(t testing.TB) []chunkingInput {
	t.Helper()
	var in []chunkingInput
	files, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		if !strings.HasSuffix(e.Name(), ".sse") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		f := OpenAIResponses
		switch {
		case strings.HasPrefix(e.Name(), "anthropic"):
			f = Anthropic
		case strings.HasPrefix(e.Name(), "chat"):
			f = OpenAIChat
		}
		in = append(in, chunkingInput{e.Name(), f, data, testLimits.MaxLineBytes},
			chunkingInput{e.Name() + " crlf", f, bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n")),
				testLimits.MaxLineBytes},
			chunkingInput{e.Name() + " cr", f, bytes.ReplaceAll(data, []byte("\n"), []byte("\r")),
				testLimits.MaxLineBytes})
	}
	for _, c := range streamInlineCases() {
		line := testLimits.MaxLineBytes
		if c.lim.MaxLineBytes > 0 {
			line = c.lim.MaxLineBytes
		}
		in = append(in, chunkingInput{c.name, c.family, []byte(c.body), line})
	}
	for _, c := range regressionCases {
		in = append(in, chunkingInput{c.name, c.family, []byte(c.input), testLimits.MaxLineBytes})
	}
	base := rcAnthropicStart + rcAnthropicStop
	for _, v := range []string{strings.ReplaceAll(base, "\n", "\r\n"), strings.ReplaceAll(base, "\n", "\r"),
		"\xEF\xBB\xBF" + base, "\xEF\xBB\xBF" + strings.ReplaceAll(base, "\n", "\r") + "x"} {
		in = append(in, chunkingInput{"variant " + fmt.Sprintf("%q", v[:12]), Anthropic, []byte(v),
			testLimits.MaxLineBytes})
	}
	return in
}

// byteLimits returns the MaxBytes values the property tries for an input of n bytes: every one from 1 to n+2
// for an input up to the size of the largest fixture with CR LF line ends; for a longer one, the ends, both
// sides of each event end and a seeded sample.
func byteLimits(data []byte, seed uint64) []int64 {
	n := int64(len(data))
	if n <= 4096 {
		out := make([]int64, 0, n+2)
		for i := int64(1); i <= n+2; i++ {
			out = append(out, i)
		}
		return out
	}
	var out []int64
	for i := int64(1); i <= 8; i++ {
		out = append(out, i, n+3-i)
	}
	for _, end := range eventEnds(data) {
		for d := int64(-2); d <= 1; d++ {
			if e := int64(end) + d; e >= 1 && e <= n+2 {
				out = append(out, e)
			}
		}
	}
	rng := rand.New(rand.NewPCG(seed, uint64(n))) //nolint:gosec // G404: seeded, so a failure repeats
	for range 40 {
		out = append(out, 1+rng.Int64N(n+2))
	}
	return out
}

func TestResultDoesNotDependOnChunking(t *testing.T) {
	const seed = 20261126
	for _, in := range chunkingInputs(t) {
		t.Run(in.family.String()+"/"+in.name, func(t *testing.T) {
			t.Parallel()
			limits := byteLimits(in.data, seed)
			if testing.Short() || raceEnabled {
				limits = limits[:min(len(limits), 48)]
			}
			for _, mb := range limits {
				lim := Limits{MaxBytes: mb, MaxLineBytes: in.line, MaxTokens: testLimits.MaxTokens}
				checkChunking(t, in.family, in.data, lim, seed)
			}
		})
	}
}

func FuzzChunking(f *testing.F) {
	for _, in := range chunkingInputs(f) {
		fam := familyIndex(in.family)
		f.Add(fam, in.data, len(in.data), in.line, uint64(1))
		f.Add(fam, in.data, len(in.data)/2, 64, uint64(2))
	}
	f.Fuzz(func(t *testing.T, fam uint8, data []byte, maxBytes, maxLine int, seed uint64) {
		family := Family(fam%3) + Anthropic
		lim := Limits{
			MaxBytes:     int64(abs(maxBytes%(len(data)+3))) + 1,
			MaxLineBytes: abs(maxLine%(4<<10)) + 1,
			MaxTokens:    fuzzLimits.MaxTokens,
		}
		checkChunking(t, family, data, lim, seed)
	})
}

// familyIndex is the fuzz byte that FuzzChunking maps back to f.
func familyIndex(f Family) uint8 {
	switch f {
	case Anthropic:
		return 0
	case OpenAIChat:
		return 1
	}
	return 2
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
