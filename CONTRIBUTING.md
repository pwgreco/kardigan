# Contributing

## Set up

You need Go 1.26+, Docker, kind, kubectl and kubebuilder 4.16+. The Makefile downloads everything else into `bin/`.

The [README](README.md#development) shows how to run the operator against a local Mimir, Prometheus and Grafana.

## Run the checks

```bash
make test     # unit tests and envtest
make lint
make verify   # every fast check: tidy, generated files, lint, tests, chart, govulncheck
make ci       # verify, plus an image scan and the e2e tests in a throwaway kind cluster
```

`make verify` fails if generated files are stale, so commit them first. Run `make ci` for anything that touches the image or how the operator deploys. CI runs the same targets, except the image scan.

## Test against real Mimir and Grafana

`make ci` never talks to Mimir: its e2e tests only check that the operator deploys and serves metrics. Run these before merging anything that changes the operator's behaviour, and before a release:

```bash
make test-integration   # under a minute: the clients against the stack, and a comparison with mimirtool
make test-stack         # about 9 minutes: the deployed operator against the stack
make stack-down         # stop the stack and wipe its data
```

Before a release, also run the slower checks:

```bash
make test-extended      # about 3 minutes: the Helm chart, its NetworkPolicy, and a snippet on a real scrape
make test-release       # about 3 minutes: builds the release artefacts, pushes them to a local registry, and checks them
make test-all           # all four
```

Each of these wipes the local stack first, because they add series, and Mimir counts a series as active for 20 minutes after its last sample, so leftovers would skew the next run. Don't keep anything you care about in the local stack. They leave it running afterwards. None of them run in hosted CI, because they need the whole stack.

`make test-stack` and `make test-extended` deploy to a throwaway kind cluster with its own kubeconfig, so they can't touch your current kubectl context. `make test-stack` checks budget states, report advice, spec edits, validation, cross-namespace rules, namespace targets with real series, metric and report removal, `PrometheusRule` objects, and outages of Mimir and Grafana. `make test-extended` installs from the chart instead, and applies a report's snippet to a `ServiceMonitor` through the Prometheus Operator. Both connect the stack's Mimir and Grafana to kind's Docker network. `make test-stack` stops each of them for a few minutes, which also affects anything else you have using the stack. On a failure they print the operator's log. To keep the cluster for a closer look, set `KEEP_CLUSTER=1`.

## Scan for secrets

```bash
make secrets-scan       # gitleaks over the history of every branch
```

## Change the code

- After changing API types or `+kubebuilder` markers, run `make manifests generate` and commit the result. Never edit `zz_generated.deepcopy.go`, `config/crd/bases/*`, `config/rbac/role.yaml` or `charts/kardigan/files/*` by hand.
- After changing the chart, run `make chart-lint`.
- New Go files need the licence header from `hack/boilerplate.go.txt`.
- Write table-driven tests, and wrap errors with `%w`.
- Pin new GitHub Actions to a full commit SHA, with the version in a comment.
- Kardigan stays observe-only: no code that patches `ServiceMonitor`s, `PodMonitor`s or collector config.
- Never import or copy code from Grafana Mimir or mimirtool. They're AGPL-3.0, and this project is Apache-2.0.

## Open a pull request

PR titles use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `test:`, `docs:`, `chore:`), because PRs are squash-merged and the title decides the next version and the changelog entry.

Docs use Canadian English, and stay concise.

Report vulnerabilities privately: see [SECURITY.md](SECURITY.md).
