package transport

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/shadewright-labs/armory/githost"
)

// Template names the path of a request, such as "/repos/{owner}/{repo}/pulls/{n}", for the Observer and
// the write check. Host packages declare each template as a constant.
//
// Do refuses a request whose template does not name its path: a fixed segment (lowercase ASCII letters,
// digits, ".", "_" and "-", except "." and "..") must equal the path's segment, "{name}" (lowercase
// ASCII letters and "_") stands for one non-empty segment, and a final "{name...}" for one or more. So
// the template holds no value of the request beyond its fixed segments, and no raw path reaches the
// Observer or the write check through it.
type Template string

// names reports whether t names path.
func (t Template) names(path string) bool {
	ts := strings.Split(string(t), "/")
	ps := strings.Split(path, "/")
	if len(ts) < 2 || ts[0] != "" || ps[0] != "" {
		return false
	}
	for i := 1; i < len(ts); i++ {
		if i >= len(ps) {
			return false
		}
		switch segmentKind(ts[i]) {
		case fixedSegment:
			if ps[i] != ts[i] {
				return false
			}
		case oneSegment:
			if ps[i] == "" {
				return false
			}
		case restSegments:
			if i != len(ts)-1 {
				return false
			}
			return !slices.Contains(ps[i:], "")
		default:
			return false
		}
	}
	return len(ps) == len(ts)
}

// cleanSegments reports whether every segment of an escaped path, once decoded, is what a router sees
// as one segment: no slash or backslash, encoded or not, no "." or ".." segment, and a valid escape.
func cleanSegments(path string) bool {
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil || dec == "." || dec == ".." || strings.ContainsAny(dec, "/\\") {
			return false
		}
	}
	return true
}

const (
	badSegment = iota
	fixedSegment
	oneSegment
	restSegments
)

func segmentKind(s string) int {
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && len(s) > 2 {
		name, kind := s[1:len(s)-1], oneSegment
		if n, ok := strings.CutSuffix(name, "..."); ok {
			name, kind = n, restSegments
		}
		if name == "" || !allIn(name, "abcdefghijklmnopqrstuvwxyz_") {
			return badSegment
		}
		return kind
	}
	if s == "" || s == "." || s == ".." || !allIn(s, "abcdefghijklmnopqrstuvwxyz0123456789._-") {
		return badSegment
	}
	return fixedSegment
}

func allIn(s, set string) bool {
	for i := range len(s) {
		if strings.IndexByte(set, s[i]) < 0 {
			return false
		}
	}
	return true
}

const (
	// pageParam is the query parameter of a page number.
	pageParam = "page"
	// maxPage bounds the page number a paged answer may lead to.
	maxPage = 1000
)

// NextPage reads the rel="next" link of an answer to r and returns the request for the next page: r
// with its "page" query parameter one higher (a request without one is page 1), on the pinned origin.
// Only the page number is read from the link; its URL is never followed or kept. ok is false on the
// last page.
//
// An unbalanced bracket or quote in a Link header, a link off the pinned origin, a second rel="next"
// link, a missing, repeated or malformed page number (only 1 to maxPage, in decimal without sign or
// leading zero), or a page that is not the current one plus one (a skip, a repeat or a step back, so no
// sequence of pages can cycle) is githost.ErrUntrustedAnswer. A request whose operation label is not a
// valid label, or whose own page number is repeated or malformed, is githost.ErrRejected; an invalid
// operation label is replaced with a fixed one.
func (c *Client) NextPage(r Request, h http.Header) (next Request, ok bool, err error) {
	if !validOp(r.Op) {
		return Request{}, false, &githost.Error{Op: opDo, Kind: githost.ErrRejected}
	}
	current := 1
	if vs, has := r.Query[pageParam]; has {
		n, valid := 0, len(vs) == 1
		if valid {
			n, valid = parsePage(vs[0])
		}
		if !valid {
			return Request{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
		}
		current = n
	}
	untrusted := &githost.Error{Op: r.Op, Kind: githost.ErrUntrustedAnswer}
	page, found := "", false
	for _, link := range h.Values("Link") {
		// Commas and semicolons split the field only outside a <URI> and a quoted string.
		links, balanced := splitOutside(link, ',')
		if !balanced {
			return Request{}, false, untrusted
		}
		for _, part := range links {
			fields, _ := splitOutside(part, ';')
			if len(fields) < 2 || !isNext(fields[1:]) {
				continue
			}
			if found {
				return Request{}, false, untrusted
			}
			found = true
			target := strings.TrimSpace(fields[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				return Request{}, false, untrusted
			}
			u, perr := url.Parse(target[1 : len(target)-1])
			if perr != nil || !onOrigin(u, c.host) {
				return Request{}, false, untrusted
			}
			q, qerr := url.ParseQuery(u.RawQuery)
			if qerr != nil || len(q[pageParam]) != 1 {
				return Request{}, false, untrusted
			}
			page = q[pageParam][0]
		}
	}
	if !found {
		return Request{}, false, nil
	}
	n, valid := parsePage(page)
	if !valid || n != current+1 {
		return Request{}, false, untrusted
	}
	next = r
	next.Query = make(url.Values, len(r.Query)+1)
	for k, vs := range r.Query {
		next.Query[k] = append([]string(nil), vs...)
	}
	next.Query.Set(pageParam, strconv.Itoa(n))
	return next, true, nil
}

// parsePage reads a page number from 1 to maxPage, in decimal without sign or leading zero.
func parsePage(s string) (int, bool) {
	if s == "" || len(s) > len(strconv.Itoa(maxPage)) || s[0] < '1' || s[0] > '9' ||
		!allIn(s, "0123456789") {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > maxPage {
		return 0, false
	}
	return n, true
}

// isNext reports whether one of a link's parameters is rel with "next" among its values.
func isNext(params []string) bool {
	for _, p := range params {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if !strings.EqualFold(strings.TrimSpace(k), "rel") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		for rel := range strings.FieldsSeq(v) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}

// splitOutside splits s at each sep that is neither inside a <URI> nor inside a quoted string (RFC 8288,
// with RFC 9110 quoted strings and their backslash escapes). balanced is false when s leaves a bracket
// or a quote open.
func splitOutside(s string, sep byte) (parts []string, balanced bool) {
	inAngle, inQuote, escaped, start := false, false, false, 0
	for i := range len(s) {
		c := s[i]
		switch {
		case inQuote:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inQuote = false
			}
		case inAngle:
			inAngle = c != '>'
		case c == '"':
			inQuote = true
		case c == '<':
			inAngle = true
		case c == sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if inAngle || inQuote {
		return nil, false
	}
	return append(parts, s[start:]), true
}
