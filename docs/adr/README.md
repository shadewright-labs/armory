# Architecture decision records

A decision that sets a package's boundary, its API contract, a security rule or a new dependency is recorded here before code depends on it. A change that weakens an invariant of [AGENTS.md](https://github.com/shadewright-labs/armory/blob/main/AGENTS.md) needs an ADR first.

- Template: [0000-template.md](0000-template.md)
- File name: `NNNN-short-title.md`, numbered in the order the pull requests open. Take the next free number on `main` and in the open pull requests.
- Status: `Proposed` while the pull request is open or the decision waits on a spike, `Accepted` once merged and agreed, `Superseded by ADR-NNNN` when a later ADR replaces it. An accepted ADR is not rewritten; a later change is a dated amendment at its end or a new ADR.
- Each ADR adds its row to the index below in the same pull request.

## Index

| ADR | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](0001-git-host-clients.md) | Package `githost`, transport and credentials for one Git host origin | Accepted | 2026-11-27 |
| [0004](0004-netguard-public-addresses.md) | Package `netguard`, which IP addresses count as public and how the table stays current | Accepted | 2026-11-27 |
