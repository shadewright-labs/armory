package githost

// Host names a Git host.
type Host string

// The hosts. Each is reached at one fixed URL, pinned in the host's client.
const (
	GitHub      Host = "github"      // https://api.github.com only
	AzureDevOps Host = "azuredevops" // https://dev.azure.com only
)

// ObjectID is a full Git object ID: 40 (SHA-1) or 64 (SHA-256) lowercase hexadecimal characters.
type ObjectID string

// Valid reports whether id is a full object ID. Branch names, short IDs and uppercase IDs are never
// accepted.
func (id ObjectID) Valid() bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RepoRef names one repository. A client is bound to exactly one at construction.
type RepoRef struct {
	Host    Host
	Owner   string // GitHub owner, or Azure DevOps organization
	Project string // Azure DevOps project; empty on GitHub
	Name    string // repository name
	// ID is the host's immutable repository ID (GitHub numeric ID, Azure DevOps GUID), read once and
	// compared with every answer that carries one. An answer for another repository is ErrUntrustedAnswer.
	ID string
}
