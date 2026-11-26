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
// digits, ".", "_" and "-") must equal the path's segment, "{name}" stands for one non-empty segment,
// and a final "{name...}" for one or more. So the template holds no value of the request beyond its
// fixed segments, and no raw path reaches the Observer or the write check through it.
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
	if s == "" || !allIn(s, "abcdefghijklmnopqrstuvwxyz0123456789._-") {
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
// A link off the pinned origin, a second rel="next" link, a missing, repeated or malformed page number
// (only 1 to maxPage, in decimal without sign or leading zero), or a page that is not the current one
// plus one (a skip, a repeat or a step back, so no sequence of pages can cycle) is
// githost.ErrUntrustedAnswer. A request whose own page number is malformed is githost.ErrRejected.
func (c *Client) NextPage(r Request, h http.Header) (next Request, ok bool, err error) {
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
		for part := range strings.SplitSeq(link, ",") {
			target, params, hasParams := strings.Cut(strings.TrimSpace(part), ";")
			if !hasParams || !isNext(params) {
				continue
			}
			if found {
				return Request{}, false, untrusted
			}
			found = true
			target = strings.TrimSpace(target)
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

func isNext(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(strings.TrimSpace(k), "rel") {
			for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(v), `"`)) {
				if strings.EqualFold(rel, "next") {
					return true
				}
			}
		}
	}
	return false
}
