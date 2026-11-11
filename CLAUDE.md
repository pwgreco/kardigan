# CLAUDE.md

Guidance for Claude (and other AI coding assistants) working in this repository. Read [`HANDOFF.md`](HANDOFF.md) first for the project's state, architecture and gotchas.

## Commands

```bash
make manifests generate   # after any change to api/ types or +kubebuilder markers (also updates the chart)
make chart-lint           # after any change to charts/kardigan
make test                 # unit tests and envtest
make lint
make verify               # every fast check CI runs; generated files must be committed first
make ci                   # verify, plus the image scan and e2e tests
make test-integration     # the clients against the local stack, compared with mimirtool
make test-stack           # the deployed operator against the local stack; run for behaviour changes
make test-extended        # the chart, its NetworkPolicy, and a snippet on a real scrape
make test-release         # the release artefacts, in a local registry
make test-all             # every test against the stack, from a wiped stack; run before a release
make run                  # run the operator locally (flags are listed in HANDOFF.md)
(cd hack/local-stack && docker compose up -d)
```

## Rules

- Never import, vendor or copy code from Grafana Mimir or mimirtool (AGPL-3.0). The project is Apache-2.0. Reading their approach to explain it is fine.
- Never edit generated files: `zz_generated.deepcopy.go`, `config/crd/bases/*`, `config/rbac/role.yaml`, `charts/kardigan/files/*`. Change the Go types or markers and regenerate.
- Keep Kardigan observe-only until an enforcement design is accepted. No code that patches ServiceMonitors, PodMonitors or collector config.
- Use only made-up data: no real company names, cluster names, account IDs, hostnames, dashboards or config, including in examples and test fixtures.
- Pin GitHub Actions to full commit SHAs with the version in a comment.
- New Go files get the Apache licence header from `hack/boilerplate.go.txt`.

## Style

- Go: `gofmt`, doc comments on exported identifiers, table-driven tests, wrap errors with `%w`, `context.Context` as the first parameter.
- Commits and PR titles: Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`).
- Docs: Canadian English, concise, no em dashes.
- When a choice between real alternatives is made, add a row to the Decisions table in `HANDOFF.md`.
- When behaviour differs from what the README or `HANDOFF.md` says, update them in the same change.
- The README's FAQ covers only what the operator does and its limitations. Design reasoning goes in the Decisions table in `HANDOFF.md`.
