package githost

import "context"

// Scope is what a token is asked for: one repository, and whether the request writes.
type Scope struct {
	Repo  RepoRef
	Write bool
}

// Credential returns a token for one attempt of a request. A client asks for a token before each
// attempt, retries included, and does not keep it, so a token refreshed between attempts is the one
// sent; caching and refresh are the credential's business. An error on a retry after an attempt of a
// write that may have reached the host also matches [ErrOutcomeUnknown]. A delay a Credential asks for
// in a [RetryableError] is capped at one hour, like one the host asks for. When a client replaces an
// error of a Credential it keeps only its class (canceled, deadline, retryable, not found, forbidden,
// unauthorized), never its text or identity.
type Credential interface {
	Token(ctx context.Context, s Scope) (Token, error)
}

// CredentialFunc adapts a function to [Credential].
type CredentialFunc func(ctx context.Context, s Scope) (Token, error)

// Token calls f.
func (f CredentialFunc) Token(ctx context.Context, s Scope) (Token, error) { return f(ctx, s) }

// SignatureAlgorithm names the algorithm a [Signer] uses. It is a named type, so a product that already
// has its own named type can alias this one.
type SignatureAlgorithm string

// RS256 is RSASSA-PKCS1-v1_5 with SHA-256, as GitHub App JWTs require.
const RS256 SignatureAlgorithm = "RS256"

// Signer signs a digest with a key that never leaves its key store, for example the RS256 signature of a
// GitHub App JWT.
type Signer interface {
	// Algorithm reports the signature algorithm of the key.
	Algorithm() SignatureAlgorithm
	// Sign signs a digest the caller has already computed (SHA-256 for RS256).
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}
