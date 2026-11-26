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

// Valid reports whether id is a full object ID of an object. Branch names, short IDs, uppercase IDs and
// the all-zero ID are never accepted: hosts read the all-zero ID as "no object", which creates or
// deletes a ref.
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
	return !id.IsZero()
}

// IsZero reports whether id is the all-zero ID of either length, which names no object.
func (id ObjectID) IsZero() bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for i := range len(id) {
		if id[i] != '0' {
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
	// ID is the host's immutable repository ID (GitHub numeric ID, Azure DevOps GUID). It is
	// configuration: the product supplies it with the names, and a client never learns it from the host.
	// It is carried for the typed calls, which will compare it with the host's answers; nothing in this
	// module checks it yet. The transport only passes the RepoRef to the Credential in a [Scope].
	ID string
}
