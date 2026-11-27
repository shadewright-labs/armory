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
	// cursorParam is the query parameter of a page cursor.
	cursorParam = "after"
	// maxPage bounds the page number a paged answer may lead to, and the number of pages a chain of
	// cursors may lead through.
	maxPage = 1000
	// maxCursor bounds the length of a cursor in bytes, once decoded.
	maxCursor = 512
)

// NextPage reads the rel="next" link of an answer to r and returns the request for the next page, rebuilt
// from r on the pinned origin. The link's URL is never followed or kept: only its page parameter is read
// from it, and its path, host and other parameters are dropped. ok is false on the last page.
//
// A link carries either a page number or a cursor. With a page number, the next request is r with its
// "page" query parameter one higher (a request without one is page 1). With a cursor, the next request
// is r with its "after" query parameter set to the link's: 1 to 512 bytes of ASCII letters, digits and
// "+", "/", "=", "_", "-", ".", "~" and ":" once decoded. A chain of cursors leads through at most
// maxPage pages, counted on the requests NextPage returns.
//
// An unbalanced bracket or quote in a Link header, a link off the pinned origin, a second rel="next"
// link, a link with both a page and a cursor or with neither, a missing, repeated or malformed page
// number (only 1 to maxPage, in decimal without sign or leading zero), a page that is not the current
// one plus one (a skip, a repeat or a step back, so no sequence of pages can cycle), a repeated or
// malformed cursor, the cursor r already carries, a change from pages to cursors or back, or a cursor
// past maxPage pages is githost.ErrUntrustedAnswer. A request whose operation label is not a valid
// label, whose own page number or cursor is repeated or malformed, or that carries both, is
// githost.ErrRejected; an invalid operation label is replaced with a fixed one.
func (c *Client) NextPage(r Request, h http.Header) (next Request, ok bool, err error) {
	if !validOp(r.Op) {
		return Request{}, false, &githost.Error{Op: opDo, Kind: githost.ErrRejected}
	}
	current, cursor, hasCursor, valid := position(r.Query)
	if !valid {
		return Request{}, false, &githost.Error{Op: r.Op, Kind: githost.ErrRejected}
	}
	untrusted := &githost.Error{Op: r.Op, Kind: githost.ErrUntrustedAnswer}
	target, found, trusted := c.nextLink(h)
	if !trusted {
		return Request{}, false, untrusted
	}
	if !found {
		return Request{}, false, nil
	}
	pages, cursors := target[pageParam], target[cursorParam]
	next = r.frozen()
	switch {
	case len(pages) == 1 && len(cursors) == 0 && !hasCursor:
		n, valid := parsePage(pages[0])
		if !valid || n != current+1 {
			return Request{}, false, untrusted
		}
		next.Query = orNew(next.Query)
		next.Query.Set(pageParam, strconv.Itoa(n))
	case len(cursors) == 1 && len(pages) == 0 && !r.Query.Has(pageParam):
		// The first page is the request that carries no cursor, so the request after r is page
		// r.cursorPage+2.
		if !validCursor(cursors[0]) || (hasCursor && cursors[0] == cursor) || r.cursorPage+2 > maxPage {
			return Request{}, false, untrusted
		}
		next.Query = orNew(next.Query)
		next.Query.Set(cursorParam, cursors[0])
		next.cursorPage = r.cursorPage + 1
	default:
		return Request{}, false, untrusted
	}
	return next, true, nil
}

// position reads the page number (1 when absent) and the cursor of a request's query. valid is false when
// either is repeated or malformed, or the query carries both.
func position(q url.Values) (page int, cursor string, hasCursor, valid bool) {
	page = 1
	if vs, has := q[pageParam]; has {
		if len(vs) != 1 {
			return 0, "", false, false
		}
		if page, valid = parsePage(vs[0]); !valid {
			return 0, "", false, false
		}
	}
	if vs, has := q[cursorParam]; has {
		if len(vs) != 1 || !validCursor(vs[0]) || q.Has(pageParam) {
			return 0, "", false, false
		}
		cursor, hasCursor = vs[0], true
	}
	return page, cursor, hasCursor, true
}

// nextLink returns the query of the one rel="next" link in h. found is false when there is none, and
// trusted is false for an unbalanced bracket or quote, a second next link, a target that is not in
// brackets, a link off the pinned origin or a query that does not parse.
func (c *Client) nextLink(h http.Header) (query url.Values, found, trusted bool) {
	for _, link := range h.Values("Link") {
		// Commas and semicolons split the field only outside a <URI> and a quoted string.
		links, balanced := splitOutside(link, ',')
		if !balanced {
			return nil, false, false
		}
		for _, part := range links {
			fields, _ := splitOutside(part, ';')
			if len(fields) < 2 || !isNext(fields[1:]) {
				continue
			}
			if found {
				return nil, false, false
			}
			found = true
			ref := strings.TrimSpace(fields[0])
			if !strings.HasPrefix(ref, "<") || !strings.HasSuffix(ref, ">") {
				return nil, false, false
			}
			u, perr := url.Parse(ref[1 : len(ref)-1])
			if perr != nil || !onOrigin(u, c.host) {
				return nil, false, false
			}
			q, qerr := url.ParseQuery(u.RawQuery)
			if qerr != nil {
				return nil, false, false
			}
			query = q
		}
	}
	return query, found, true
}

// orNew returns q, or an empty url.Values when q is nil.
func orNew(q url.Values) url.Values {
	if q == nil {
		return url.Values{}
	}
	return q
}

// validCursor reports whether s is a cursor NextPage takes: 1 to maxCursor bytes of ASCII letters,
// digits and "+/=_-.~:", the characters of base64 and the cursors hosts send.
func validCursor(s string) bool {
	return s != "" && len(s) <= maxCursor &&
		allIn(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/=_-.~:")
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
