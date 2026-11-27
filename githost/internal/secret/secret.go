// Package secret is the one path by which a token's value leaves a githost.Token. It is internal, so
// only packages under githost can import it, and only the transport uses it, after the request's scheme
// and host are checked against the pinned origin.
package secret

// Reveal returns the scheme and value of tok, which must be a githost.Token; ok is false for anything
// else and for a token with no value. Package githost sets it when it is initialized.
var Reveal func(tok any) (scheme int, value string, ok bool)
