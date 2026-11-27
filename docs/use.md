# Use Armory

## Import path

The module path is `github.com/shadewright-labs/armory`. Import a package directly:

```go
import "github.com/shadewright-labs/armory/modelusage"
```

## Go version

`go.mod` specifies `go 1.27.4`. Armory currently uses only the Go standard library.

## Fetching before a tagged release

Armory has no tagged version yet. Fetch the latest commit on `main`:

```bash
go get github.com/shadewright-labs/armory@main
```

To pin a specific revision, pass a commit hash:

```bash
go get github.com/shadewright-labs/armory@<commit-sha>
```

## Stability

Armory has no release yet, and its API can change until v1.0.0. Before v1.0.0, a minor version may break the API. From v1.0.0 onward, Armory follows semantic versioning: a breaking change requires a new major version and module path (`/v2`), and removed identifiers are deprecated with a `// Deprecated:` comment first.

## Go reference

Generated package documentation at [pkg.go.dev/github.com/shadewright-labs/armory](https://pkg.go.dev/github.com/shadewright-labs/armory) appears once the first version is tagged. Until then, read the package documentation locally with `go doc`:

```bash
go doc -all github.com/shadewright-labs/armory/modelusage
```
