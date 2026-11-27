package transport

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/shadewright-labs/armory/githost"
)

const (
	cursorA = "Y3Vy"
	cursorB = "abc"
)

// TestNextPageCursor: a next link that carries a cursor is never followed as given. Only its "after"
// value is taken, checked and set on the original request, which stays on the pinned origin with its
// own path, template and other parameters.
func TestNextPageCursor(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	link := func(query string) string { return `<https://api.example.test/elsewhere/7?` + query + `>; rel="next"` }
	cases := []struct {
		name       string
		current    string // the cursor of the request; empty means none
		links      []string
		wantCursor string // empty means the last page
		wantErr    error
	}{
		{name: "first cursor", links: []string{link("after=Y3Vyc29yOnYyOpK0")}, wantCursor: "Y3Vyc29yOnYyOpK0"},
		{name: "next cursor", current: cursorA, links: []string{link("per_page=100&after=Y3Vz")}, wantCursor: "Y3Vz"},
		{name: "escaped cursor", links: []string{link("after=Y3Vy%3D%3D")}, wantCursor: "Y3Vy=="},
		{name: "other parameters of the link are dropped", links: []string{
			link("after=abc&q=" + pathCanary + "&per_page=1")}, wantCursor: cursorB},
		{name: "last page", current: cursorA, links: []string{`<https://api.example.test/x?before=Y3Vx>; rel="prev"`}},
		{name: "same cursor again", current: cursorA, links: []string{link("after=" + cursorA)},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "page and cursor", links: []string{link("after=Y3Vy&page=2")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "page after a cursor", current: cursorA, links: []string{link("page=2")},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor twice", links: []string{link("after=a&after=b")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "empty cursor", links: []string{link("after=")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor too long", links: []string{link("after=" + strings.Repeat("a", maxCursor+1))},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor with a NUL", links: []string{link("after=a%00b")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "base64 cursor", links: []string{link("after=a%2Bb%2Fc%3D")}, wantCursor: "a+b/c="},
		{name: "cursor with an ampersand", links: []string{link("after=a%26page%3D9")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor with a space", links: []string{link("after=a+b")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor off the origin", links: []string{`<https://evil.example.test/x?after=abc>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor over http", links: []string{`<http://api.example.test/x?after=abc>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "bad cursor on the request", current: "a b", links: []string{link("after=" + cursorB)},
			wantErr: githost.ErrRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := get()
			if tc.current != "" {
				r.Query.Set(cursorParam, tc.current)
			}
			orig := r.Query.Encode()
			hdr := http.Header{}
			for _, l := range tc.links {
				hdr.Add("Link", l)
			}
			next, ok, err := c.NextPage(r, hdr)
			if r.Query.Encode() != orig {
				t.Errorf("the original request changed: %s", r.Query.Encode())
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || ok || !reflect.DeepEqual(next, Request{}) {
					t.Fatalf("NextPage = %+v, %v, %v; want %v", next, ok, err, tc.wantErr)
				}
				assertScrubbed(t, nil, err)
				return
			}
			if err != nil || ok != (tc.wantCursor != "") {
				t.Fatalf("NextPage = %v, %v; want cursor %q", ok, err, tc.wantCursor)
			}
			if !ok {
				return
			}
			want := url.Values{"q": {pathCanary}, cursorParam: {tc.wantCursor}}
			if !reflect.DeepEqual(next.Query, want) || next.Path != r.Path || next.Template != r.Template ||
				next.Method != r.Method || next.Op != r.Op {
				t.Errorf("NextPage = %+v, want the request with query %v", next, want)
			}
			u, berr := c.buildURL(next)
			if berr != nil || u.Host != apiHost || u.Path != r.Path {
				t.Errorf("next request URL = %v, %v; want the original path on the pinned origin", u, berr)
			}
		})
	}
}

// TestNextPageCursorBounded: a server that keeps handing out new cursors cannot lead a caller through
// more than maxPage pages.
func TestNextPageCursorBounded(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	r := get()
	pages := 1
	for i := 0; ; i++ {
		hdr := http.Header{"Link": {`<https://api.example.test/x?after=c` + string(rune('a'+i%2)) + `x` +
			strings.Repeat("y", i%7) + `>; rel="next"`}}
		next, ok, err := c.NextPage(r, hdr)
		if err != nil {
			if !errors.Is(err, githost.ErrUntrustedAnswer) || pages != maxPage {
				t.Fatalf("NextPage after %d pages = %v; want ErrUntrustedAnswer after %d", pages, err, maxPage)
			}
			return
		}
		if !ok {
			t.Fatal("NextPage stopped without a reason")
		}
		r = next
		pages++
		if pages > maxPage+1 {
			t.Fatalf("NextPage led past %d pages", maxPage)
		}
	}
}
