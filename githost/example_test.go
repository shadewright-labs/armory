package githost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadewright-labs/armory/githost"
)

// A Token prints, formats and marshals as a placeholder, so it is safe in a log line, an error or a
// JSON document. The value here is a fake built from two parts.
func ExampleNewToken() {
	tok := githost.NewToken(githost.Bearer, "fake-"+"token-value", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	fmt.Println(tok)
	fmt.Printf("%#v %q\n", tok, tok)
	data, _ := json.Marshal(struct{ Token githost.Token }{tok})
	fmt.Println(string(data))
	fmt.Println(tok.Expires().Format(time.RFC3339), tok.IsZero())
	// Output:
	// [redacted token]
	// [redacted token] [redacted token]
	// {"Token":"[redacted token]"}
	// 2026-01-02T03:04:05Z false
}

// A product supplies tokens through a Credential. CredentialFunc adapts a function; a client calls it
// before each attempt with the repository and whether the request writes.
func ExampleCredentialFunc() {
	var cred githost.Credential = githost.CredentialFunc(func(_ context.Context, s githost.Scope) (githost.Token, error) {
		if s.Write {
			return githost.Token{}, githost.ErrForbidden // this product's credential reads only
		}
		return githost.NewToken(githost.Bearer, "fake-"+"read-token", time.Time{}), nil
	})
	repo := githost.RepoRef{Host: githost.GitHub, Owner: "octo", Name: "hello", ID: "42"}
	tok, err := cred.Token(context.Background(), githost.Scope{Repo: repo})
	fmt.Println(tok.IsZero(), err)
	_, err = cred.Token(context.Background(), githost.Scope{Repo: repo, Write: true})
	fmt.Println(errors.Is(err, githost.ErrForbidden))
	// Output:
	// false <nil>
	// true
}

// An Error carries an operation label, a status and a kind; match the kind with errors.Is.
func ExampleError() {
	err := fmt.Errorf("read branch: %w", &githost.Error{Op: "BranchHead", Status: 404, Kind: githost.ErrNotFound})
	fmt.Println(err)
	fmt.Println(errors.Is(err, githost.ErrNotFound), errors.Is(err, githost.ErrConflict))
	// Output:
	// read branch: githost: BranchHead: status 404: not found
	// true false
}

// A RetryableError says the same request may succeed later, and how long to wait when the host said.
func ExampleRetryableError() {
	err := error(&githost.RetryableError{
		RetryAfter: 30 * time.Second,
		Err:        &githost.Error{Op: "ListPulls", Status: 503},
	})
	var re *githost.RetryableError
	if errors.Is(err, githost.ErrRetryable) && errors.As(err, &re) {
		fmt.Println("retry after", re.RetryAfter)
	}
	fmt.Println(err)
	// Output:
	// retry after 30s
	// retryable (after 30s): githost: ListPulls: status 503
}

// A product's check before a write refuses with a Refusal; only a valid class survives into the
// client's error.
func ExampleRefusal() {
	for _, r := range []*githost.Refusal{{Class: "paused"}, {Class: "Paused for the night"}} {
		fmt.Println(r.Valid(), r)
	}
	// Output:
	// true refused: paused
	// false refused
}

// ObjectID.Valid accepts a 40 or 64 character lowercase hexadecimal ID and refuses the all-zero ID,
// which hosts read as creating or deleting a ref.
func ExampleObjectID_Valid() {
	for _, id := range []githost.ObjectID{
		"4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"0000000000000000000000000000000000000000",
		"4B825DC642CB6EB9A060E54BF8D69288FBEE4904",
	} {
		fmt.Println(id.Valid())
	}
	// Output:
	// true
	// false
	// false
}
