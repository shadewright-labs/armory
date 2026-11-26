// Package githost holds the host-neutral types of a client for one repository on a Git host.
//
// A client is bound to one repository ([RepoRef]) when it is built. It holds no credential and decides
// nothing about what a caller may do: the product supplies a [Credential], which hands out a [Token] for
// each attempt of a request.
//
// A [Token] prints, formats and marshals as a fixed placeholder with every verb and encoder. Nothing
// exported returns its value: only the module's own transport writes it into a request header, after
// checking the request against the one https origin the client is pinned to.
//
// Errors are the sentinels of this package, matched with [errors.Is], and three types for [errors.As]:
// [*Error] carries an operation label, an HTTP status and a kind, [*RetryableError] a transient failure
// with the delay the host asked for, and [*Refusal] the short class a product's check gave when it
// refused a write. Error text never holds a URL, header, body, repository, branch or token, nor the text
// of an error a product's callback returned.
//
// A write whose attempt may have reached the host and whose answer was lost matches [ErrOutcomeUnknown]
// with every later failure of the same call; a write the host accepted whose answer could not be used
// matches [ErrUnusableReceipt], and [ErrOutcomeUnknown] as well when an earlier attempt may have written
// too. Neither is retried, and neither means that nothing was written.
//
// This package holds types only. It has no Reader or Writer interface, no host client and no exported
// write check yet: those are still being designed. It does not read files, environment variables or
// secret stores, does not log, and imports only the standard library. Which repository, which branches,
// which approvals and which audit records apply are the product's decisions.
package githost
