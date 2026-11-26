package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// TestEventFields: an Event holds no error, interface, map, slice or pointer, and the template is a
// type of the package.
func TestEventFields(t *testing.T) {
	et := reflect.TypeFor[Event]()
	for f := range et.Fields() {
		switch f.Type.Kind() {
		case reflect.Interface, reflect.Map, reflect.Slice, reflect.Pointer, reflect.Func, reflect.Chan:
			t.Errorf("Event.%s is a %s", f.Name, f.Type.Kind())
		}
	}
	if f, _ := et.FieldByName("Template"); f.Type != reflect.TypeFor[Template]() {
		t.Errorf("Event.Template is a %s, want Template", f.Type)
	}
}

// TestObserverAfterBody: the Observer is called after the answer's body is read and closed, and its
// Duration includes the body. A blocking Observer cannot cut the body short with the attempt timeout.
func TestObserverAfterBody(t *testing.T) {
	const bodyDelay = 150 * time.Millisecond
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(bodyDelay)
		okBody(w)
	})
	var events []Event
	h := newHarness(t, s, func(c *Config) {
		c.AttemptTimeout = 400 * time.Millisecond
		c.MaxAttempts = 1
		c.Observer = func(e Event) {
			events = append(events, e)
			time.Sleep(600 * time.Millisecond) // past the attempt timeout
		}
	})
	resp, err := h.c.Do(t.Context(), get())
	if err != nil || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("Do = %q, %v; want the whole body", resp.Body, err)
	}
	if len(events) != 1 || events[0].Duration < bodyDelay || events[0].Status != http.StatusOK {
		t.Fatalf("events %+v, want one whose duration covers the body", events)
	}
	if text := fmt.Sprintf("%+v", events[0]); strings.Contains(text, pathCanary) {
		t.Errorf("event %s holds the raw path", text)
	}
}

// TestObserverPastDeadline: an Observer that outlasts the caller's deadline delays the return of Do
// but starts no further attempt, and the error is the deadline.
func TestObserverPastDeadline(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusBadGateway) })
	h := newHarness(t, s, func(c *Config) {
		c.Observer = func(Event) { time.Sleep(300 * time.Millisecond) }
		c.Sleep = nil
		c.Now = nil
		c.BaseDelay = time.Millisecond
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := h.c.Do(ctx, get())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do = %v, want the deadline", err)
	}
	if n := s.hits.Load(); n != 1 {
		t.Errorf("server got %d requests, want 1", n)
	}
}

// TestObserverOnError: a request that gets no answer reports status zero, and the Observer gets no
// error value.
func TestObserverOnError(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { dropConn(w) })
	h := newHarness(t, s, func(c *Config) { c.MaxAttempts = 1 })
	if _, err := h.c.Do(t.Context(), get()); err == nil {
		t.Fatal("Do succeeded")
	}
	if len(h.events) != 1 || h.events[0].Status != 0 || h.events[0].Template != template {
		t.Fatalf("events %+v", h.events)
	}
}

const (
	pathThing    = "/repos/o/n/things/x"
	tmplContents = "/repos/{owner}/{repo}/contents/{path...}"
)

// TestTemplateMatchesPath: a template must name the path's segments: its fixed segments equal the
// path's, a variable segment stands for one segment, and a final "{name...}" for the rest. Anything
// else is refused before a token is asked for.
func TestTemplateMatchesPath(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) { okBody(w) })
	h := newHarness(t, s, nil)
	cases := []struct {
		path, tmpl string
		ok         bool
	}{
		{pathThing, template, true},
		{"/repos/o/n/contents/a/b/c.txt", tmplContents, true},
		{"/repos/o/n/contents/c.txt", tmplContents, true},
		{"/rate_limit", "/rate_limit", true},
		{"/repos/o/n/things/" + pathCanary, "/repos/{owner}/{repo}/things/" + pathCanary, false},
		{pathThing, "/repos/{owner}/{repo}/things", false},
		{"/repos/o/n/things", template, false},
		{"/repos/o/n/stuff/x", template, false},
		{pathThing, "repos/{owner}/{repo}/things/{id}", false},
		{pathThing, "/repos/{owner}/{repo}/things/{Id}", false},
		{pathThing, "/repos/{owner}/{repo}/things/{}", false},
		{pathThing, "/repos/{owner}/{repo}/things/{id", false},
		{"/repos/o/n/contents/", tmplContents, false},
		{"/repos/o/n/a/b", "/repos/{owner}/{repo}/{path...}/b", false},
		{"/x/y", "/x/{a}?q", false},
		{"/x/y", "/x/{a} ", false},
		{"/x/", "/x/", false},
		// The template is matched against the path the host routes: no encoded slash or backslash, no
		// dot segment, and the path as sent, byte for byte.
		{"/repos/o/n/things/a%2Fb", template, false},
		{"/repos/o/n/things/a%2fb", template, false},
		{"/repos/o/n/things/a%5Cb", template, false},
		{"/repos/o/n/things/..", template, false},
		{"/repos/o/n/things/.", template, false},
		{"/repos/o/n/things/%2e%2e", template, false},
		{"/repos/o/n/things/%2E", template, false},
		{"/repos/o/n/things/.%2e", template, false},
		{"/repos/o/n/things/%zz", template, false},
		{"/repos/o/n/things/a b", template, false},
		{"/repos/o/../things/x", template, false},
		{"/repos/o/n/contents/a/../b", tmplContents, false},
		{"/repos/o/n/contents/a/./b", tmplContents, false},
		{"/repos/o/n/contents/a%2Fb", tmplContents, false},
		{"/repos/o/n/contents/..", tmplContents, false},
		{"/a/../b", "/a/../b", false},
		{"/a/./b", "/a/./b", false},
		{"/repos/o/n/things/a%20b", template, true},
		{"/repos/o/n/things/..a", template, true},
	}
	for _, c := range cases {
		r := get()
		r.Path, r.Template, r.Query = c.path, Template(c.tmpl), nil
		_, err := h.c.Do(t.Context(), r)
		if c.ok != (err == nil) || (!c.ok && !errors.Is(err, githost.ErrRejected)) {
			t.Errorf("Do(%q, %q) = %v, want ok %v", c.path, c.tmpl, err, c.ok)
		}
	}
	if n := h.creds.Load(); n != 6 {
		t.Errorf("credential asked %d times, want 6", n)
	}
}

// TestNextPage: the next page is the original request with its page number one higher, on the pinned
// origin. Any other continuation is refused.
func TestNextPage(t *testing.T) {
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	link := func(page string) string {
		return `<https://api.example.test/repositories/7/x?page=` + page + `>; rel="next"`
	}
	cases := []struct {
		name     string
		current  string // the page of the request; empty means none
		links    []string
		wantPage string // empty means the last page
		wantErr  error
	}{
		{name: "no link"},
		{name: "last page", current: "4", links: []string{`<https://api.example.test/x?page=3>; rel="prev"`}},
		{name: "first to second", links: []string{link("2") + `, <https://api.example.test/x?page=9>; rel="last"`},
			wantPage: "2"},
		{name: "third to fourth", current: "3", links: []string{link("4")}, wantPage: "4"},
		{name: "unquoted rel in a second header", current: "1",
			links: []string{`<https://api.example.test/x?page=1>; rel="prev"`,
				`<https://api.example.test/x?page=2>; rel=next`},
			wantPage: "2"},
		{name: "two rels", links: []string{`<https://api.example.test/x?page=2>; rel="first next"`}, wantPage: "2"},
		{name: "skips a page", current: "1", links: []string{link("3")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "same page", current: "3", links: []string{link("3")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "back to the first", current: "3", links: []string{link("1")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "two next links", links: []string{link("2"), link("2")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "two next links, one header", links: []string{link("2") + ", " + link("3")},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "page twice", links: []string{`<https://api.example.test/x?page=2&page=3>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "no page", links: []string{`<https://api.example.test/x?per_page=3>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "cursor", links: []string{`<https://api.example.test/x?after=Y3Vy>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "zero", links: []string{link("0")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "negative", links: []string{link("-2")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "plus sign", links: []string{link("+2")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "leading zero", links: []string{link("02")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "not a number", links: []string{link("2a")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "empty", links: []string{link("")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "past the bound", current: fmt.Sprint(maxPage), links: []string{link(fmt.Sprint(maxPage + 1))},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "overflow", links: []string{link("99999999999999999999999")}, wantErr: githost.ErrUntrustedAnswer},
		{name: "other host", links: []string{`<https://evil.example.test/x?page=2>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "plain http", links: []string{`<http://api.example.test/x?page=2>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "userinfo", links: []string{"<https://" + apiHost + at + `evil.test/x?page=2>; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "no brackets", links: []string{`https://api.example.test/x?page=2; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "comma in the URL", links: []string{`<https://api.example.test/a,b?page=2>; rel="next"`},
			wantPage: "2"},
		{name: "comma in a quoted title", links: []string{`<https://api.example.test/x?page=2>; rel="next"; title="a, b"`},
			wantPage: "2"},
		{name: "rel inside a quoted title", links: []string{
			`<https://api.example.test/x?page=1>; rel="prev"; title="x; rel=next, <https://api.example.test/x?page=2>"`},
		},
		{name: "escaped quote in a title", links: []string{
			`<https://api.example.test/x?page=1>; title="a\", rel=next"; rel="prev"`, link("2")},
			wantPage: "2"},
		{name: "unclosed bracket", links: []string{`<https://api.example.test/x?page=2; rel="next"`},
			wantErr: githost.ErrUntrustedAnswer},
		{name: "bad current page", current: "x", links: []string{link("2")}, wantErr: githost.ErrRejected},
		{name: "current page zero", current: "0", wantErr: githost.ErrRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := get()
			if tc.current != "" {
				r.Query.Set(pageParam, tc.current)
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
				return
			}
			if err != nil || ok != (tc.wantPage != "") {
				t.Fatalf("NextPage = %v, %v; want page %q", ok, err, tc.wantPage)
			}
			if !ok {
				return
			}
			want := get()
			want.Query.Set(pageParam, tc.wantPage)
			if !reflect.DeepEqual(next, want) {
				t.Errorf("NextPage = %+v, want %+v", next, want)
			}
		})
	}
}

// FuzzNextPage: no Link header panics, and a next page is always the current one plus one, with the
// rest of the request unchanged.
func FuzzNextPage(f *testing.F) {
	f.Add("", `<https://api.example.test/x?page=2>; rel="next"`)
	f.Add("3", `<https://api.example.test/x?page=4>; rel="next", <https://api.example.test/x?page=9>; rel="last"`)
	f.Add("1", `<https://api.example.test/x?page=2&page=2>; rel=next`)
	f.Add("1", `<https://api.example.test/x?page=%32>; rel="prev next"`)
	c, err := New(Config{Host: apiHost, Credential: githost.CredentialFunc(
		func(context.Context, githost.Scope) (githost.Token, error) { return githost.Token{}, nil })})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, current, link string) {
		r := get()
		if current != "" {
			r.Query.Set(pageParam, current)
		}
		next, ok, err := c.NextPage(r, http.Header{"Link": {link}})
		if !ok {
			if !reflect.DeepEqual(next, Request{}) {
				t.Fatalf("NextPage = %+v without ok", next)
			}
			return
		}
		if err != nil {
			t.Fatalf("NextPage = ok with %v", err)
		}
		cur := 1
		if current != "" {
			if _, serr := fmt.Sscan(current, &cur); serr != nil {
				t.Fatalf("a bad current page %q gave a next page", current)
			}
		}
		if got := next.Query.Get(pageParam); got != fmt.Sprint(cur+1) || cur+1 > maxPage {
			t.Fatalf("NextPage from %q = page %q", current, got)
		}
		next.Query.Set(pageParam, current)
		if current == "" {
			next.Query.Del(pageParam)
		}
		if !reflect.DeepEqual(next, r) {
			t.Fatalf("NextPage changed the request: %+v", next)
		}
	})
}
