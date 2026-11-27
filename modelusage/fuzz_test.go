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
	var (
		pe *PartialError
		te *TrailingError
	)
	partial := errors.As(err, &pe)
	trailing := errors.As(err, &te)
	carries := partial || trailing
	if partial && trailing {
		t.Fatalf("error %v is both partial and trailing", err)
	}
	if err != nil && !carries && u != (Usage{}) {
		t.Fatalf("usage %+v returned with %v", u, err)
	}
	if carries && u == (Usage{}) {
		t.Fatalf("no usage returned with %v", err)
	}
	if u == (Usage{}) {
		return
	}
	if u.Family != fam {
		t.Fatalf("family %v, want %v", u.Family, fam)
	}
	for _, n := range u.countList() {
		if n < 0 || n > fuzzLimits.MaxTokens {
			t.Fatalf("count %d out of range in %+v", n, u)
		}
	}
	for _, sum := range [...]int64{u.TotalInputTokens(), u.TotalOutputTokens(), u.CacheReadTokens(),
		u.CacheWriteTokens(), u.TotalTokens()} {
		if sum < 0 || sum > fuzzLimits.MaxTokens {
			t.Fatalf("sum %d out of range in %+v", sum, u)
		}
	}
	checkUsageInvariants(t, u)
	if partial {
		if rerr := u.checkRange(fuzzLimits.MaxTokens); rerr != nil {
			t.Fatalf("partial usage %+v is out of range: %v", u, rerr)
		}
		return
	}
	if verr := u.validate(fuzzLimits.MaxTokens); verr != nil || u.UncachedInputTokens() < 0 {
		t.Fatalf("usage %+v breaks its rules: %v", u, verr)
	}
}

// checkUsageInvariants verifies that u's totals never fall below the top-level counts and family-only fields
// stay zero on other families.
func checkUsageInvariants(t *testing.T, u Usage) {
	t.Helper()
	parts := u.TotalInputTokens() + u.TotalOutputTokens()
	if u.TotalTokens() != max(parts, u.ReportedTotalTokens) ||
		u.TotalInputTokens() < u.InputTokens || u.TotalOutputTokens() < u.OutputTokens ||
		u.TotalTokens() < u.InputTokens+u.OutputTokens {
		t.Fatalf("inconsistent totals in %+v", u)
	}
	if u.Family == Anthropic {
		if u.ReportedTotalTokens != 0 ||
			u.CacheReadTokens() < u.CacheReadInputTokens ||
			u.CacheWriteTokens() < u.CacheCreationInputTokens ||
			u.TotalInputTokens() < u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens {
			t.Fatalf("inconsistent anthropic totals in %+v", u)
		}
		return
	}
	if u.CompactionInputTokens != 0 || u.CompactionOutputTokens != 0 ||
		u.AdvisorInputTokens != 0 || u.AdvisorOutputTokens != 0 ||
		u.UnclassifiedIterationInputTokens != 0 || u.UnclassifiedIterationOutputTokens != 0 ||
		(u.ReportedTotalTokens > 0 && u.ReportedTotalTokens < parts) {
		t.Fatalf("inconsistent openai totals in %+v", u)
	}
}

// wrapJSONAsStream wraps a single valid JSON object (stripped of BOM and newlines) as an SSE stream for fam.
func wrapJSONAsStream(fam Family, trimmed []byte) []byte {
	single := make([]byte, len(trimmed))
	for i, c := range trimmed {
		if c == '\r' || c == '\n' {
			c = ' '
		}
		single[i] = c
	}
	var b bytes.Buffer
	switch fam {
	case Anthropic:
		b.WriteString(`data: {"type":"message_start","message":`)
		b.Write(single)
		b.WriteString("}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	case OpenAIChat:
		b.WriteString("data: ")
		b.Write(single)
		b.WriteString("\n\ndata: [DONE]\n\n")
	default:
		b.WriteString(`data: {"type":"response.completed","response":`)
		b.Write(single)
		b.WriteString("}\n\n")
	}
	return b.Bytes()
}

// checkJSONStreamAgreement checks that ParseJSON and ParseStream agree when the same JSON object is wrapped
// in the family's stream framing.
func checkJSONStreamAgreement(t *testing.T, fam Family, data []byte, u Usage, err error) {
	t.Helper()
	if int64(len(data)) > fuzzLimits.MaxBytes {
		return
	}
	trimmed := bytes.TrimPrefix(data, byteOrderMark)
	if !validJSON(trimmed) {
		return
	}
	i := skipSpace(trimmed, 0)
	if i >= len(trimmed) || trimmed[i] != '{' {
		return
	}
	stream := wrapJSONAsStream(fam, trimmed)
	lim := Limits{
		MaxBytes:     int64(len(stream)) + 64,
		MaxLineBytes: len(stream) + 64,
		MaxTokens:    fuzzLimits.MaxTokens,
	}
	su, serr := ParseStream(fam, bytes.NewReader(stream), lim)
	if err != nil {
		if serr == nil {
			t.Fatalf("ParseJSON failed with %v, but ParseStream succeeded with %+v on %q", err, su, data)
		}
		return
	}
	if fam == OpenAIChat {
		if ev, decErr := decodeEvent(trimmed, false); decErr == nil && ev.err != nil && !isNull(ev.err) {
			var pe *PartialError
			if su != u || !errors.As(serr, &pe) || !errors.Is(serr, ErrTruncated) {
				t.Fatalf("chat reply with error member: stream gave (%+v, %v), want (%+v, *PartialError(ErrTruncated))",
					su, serr, u)
			}
			return
		}
	}
	if serr != nil || su != u {
		t.Fatalf("ParseJSON gave (%+v, nil), ParseStream gave (%+v, %v) on %q", u, su, serr, data)
	}
}

func FuzzParseJSON(f *testing.F) {
	addSeeds(f, ".json")
	f.Fuzz(func(t *testing.T, fam uint8, data []byte) {
		family := Family(fam%3) + Anthropic
		cr := &countingReader{r: bytes.NewReader(data)}
		u, err := ParseJSON(family, cr, fuzzLimits)
		checkResult(t, cr, u, err, family)
		if err != nil && u != (Usage{}) {
			t.Fatalf("ParseJSON returned non-zero usage %+v with error %v", u, err)
		}
		if int64(len(data)) > fuzzLimits.MaxBytes && !errors.Is(err, ErrTooLarge) {
			t.Fatalf("a reply of %d bytes gave %v, want ErrTooLarge", len(data), err)
		}
		checkJSONStreamAgreement(t, family, data, u, err)
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
