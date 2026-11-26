# Armory

Armory is the Go module `github.com/shadewright-labs/armory`. It holds code that more than one Shadewright Labs product needs. Today that is one package, `modelusage`, which reads the token usage a model provider reports. Redaction, hygiene checks and tamper-evident audit chains are planned.

Armory has no release yet, and its API can change until v1.0.0.

## No product dependencies

Every Shadewright Labs product may import Armory, and Armory depends on no product. No package here imports a product module or knows a product's types, policy, tokens, model clients or configuration. Each product keeps its own policy and passes what Armory needs as arguments.

Four other invariants in [AGENTS.md](https://github.com/shadewright-labs/armory/blob/main/AGENTS.md) apply to every package; weakening any of them requires an ADR in [docs/adr/](adr/README.md) first:

- Standard library first: any third-party dependency needs a reason in the pull request, an Apache-2.0 compatible licence (no GPL, AGPL or LGPL) and passes dependency review.
- No credentials and no secrets in output: Armory reads no environment secrets and no file it was not passed, does not log, and does not quote external input in error text.
- Bounded and fuzzed parsers: code that reads external input works within limits the caller sets, returns an error instead of panicking on bad input, and ships with fuzz targets and a seed corpus.
- Semantic versioning: before v1.0.0 a minor version may break the API; from v1.0.0 a breaking change uses a new major version and `/v2` module path.

## Packages

| Package | What it is for | State |
| --- | --- | --- |
| [`modelusage`](packages/modelusage.md) | Reading the token usage a model provider reports in a reply (Anthropic Messages, OpenAI chat completions and Responses), complete or streamed, within byte limits the caller sets, with cache and reasoning tokens counted once | Available |
| Redaction | Hiding known secret forms in text from outside a product, in bounded time, before the text reaches a model, storage, logs or a person | Planned, next |
| Hygiene checks | Checks a repository runs before publishing: emails, local paths and key-shaped literals in tracked files | Planned |
| Audit chain | Chained audit records, so that an edit, a removed row or a truncated tail is detected | Planned |

These are the parts of the [six shared security laws](https://shadewrightlabs.com/#security-laws) that more than one product needs.

## Where to go next

- [Use it](use.md) for the import path, Go version and pre-v1.0.0 fetching.
- [`modelusage`](packages/modelusage.md) for the API, limits and stream result classes.
- [Architecture decision records](adr/README.md) for how decisions are recorded.
- [CI](ci.md) for the checks and release model.
