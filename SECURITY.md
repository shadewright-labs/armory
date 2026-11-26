# Security policy

## Supported versions

Armory has no release yet. Until v1.0.0, the `main` branch and the latest `v0.x` tag are supported; a fix goes into the next tag and older tags get no backports. From v1.0.0 the latest minor version of each supported major version gets fixes.

## Reporting a vulnerability

Use [GitHub's private vulnerability reporting form](https://github.com/shadewright-labs/armory/security/advisories/new). Keep vulnerability details out of public issues and pull requests, including a pull request that fixes it.

Include the affected package and version or commit, the impact and steps to reproduce with synthetic data. Keep live credentials and personal data out of the report.

Expect an acknowledgement within 7 days and a fix or decision within 90 days. Confirmed vulnerabilities receive a GitHub security advisory, a new tag and, where applicable, a CVE, so that the Go vulnerability database and `govulncheck` report it to every product that imports the affected package. Reporters can choose whether to be credited.

## Scope

This policy covers the packages and automation in this repository. The invariants in [AGENTS.md](AGENTS.md) are what Armory promises, and a way to break one is a vulnerability even without a working exploit: for example, input that makes a parser read past the limits its caller set, panic or run without bound, a redaction rule that lets a known secret form through, an audit chain that accepts an edited or truncated record, or an error message that repeats a secret from its input.

Report already public dependency advisories in an issue or a pull request that updates the dependency. A bug in a product that uses Armory goes to that product's own security policy.
