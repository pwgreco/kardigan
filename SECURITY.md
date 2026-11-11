# Security policy

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub: on the repo's **Security** tab, choose **Report a vulnerability**.

Include what you found, how to reproduce it, and which version you tested. This is a personal project, so responses are best effort, but reports are taken seriously.

## Supported versions

Kardigan is alpha, so only the latest release gets fixes.

## What to expect from a release

- Images are distroless and run as non-root, with a read-only root filesystem and all capabilities dropped.
- Images carry an SBOM and build provenance, and the licences of their dependencies at `/licenses`. From v0.3.0, releases are also signed with keyless [cosign](https://github.com/sigstore/cosign).
- Actions, base images and tools are pinned, and code and dependencies are scanned with govulncheck.

The operator is observe-only. Its ClusterRole can read budgets and write their status, manage reports, and list `PrometheusRule` objects. It can't read Secrets, and can't change how metrics are collected.

One limit to be aware of in shared clusters: a budget with a `job` target can read that job's series count and metric names from any namespace, because a job label has no owning namespace. Use RBAC to limit who can create budgets if that matters.
