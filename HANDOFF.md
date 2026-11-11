# Kardigan handoff

Everything someone (or future me) needs to pick this project up cold. Keep the **Current state** section updated after each release; the rest should change rarely.

- **Owner:** Paul Greco
- **Last updated:** 2026-11-11
- **Repo visibility:** public

## 1. What this is

Kardigan is a Kubernetes operator that gives each team a budget of active Prometheus series. It counts series in Grafana Mimir on a schedule, reports `WithinBudget`, `WarningThreshold` or `BudgetExceeded` on a `CardinalityBudget` resource, and exports metrics so alerting stays in Grafana. v0.2 adds a `CardinalityReport` that recommends which metrics and labels to drop.

## 2. Current state

| Release | Scope | Status |
| --- | --- | --- |
| v0.1.0 | `CardinalityBudget`, the Mimir client, the reconciler and metrics, budgets by namespace, the Helm chart and release automation | Published 2026-11-03 |
| v0.2.0 | `CardinalityReport`: usage from Grafana, the Mimir ruler and `PrometheusRule` objects, drop recommendations and relabel snippets | Published 2026-11-03 |
| v0.3.0 | First public release: hardening, and tests against the real stack | Next |
| Later | Enforcement design | Not started |

**Next action:** write the enforcement design.

**Tracking:** GitHub issues.

## 3. Where things are

| Path | Contents |
| --- | --- |
| `hack/local-stack/` | Docker Compose: Mimir 3.2.4, Prometheus 3.15.1, Grafana 13.3.0, four Avalanche "teams", published on `127.0.0.1` only |
| `hack/kind/` | Team namespaces with Avalanche, a Prometheus agent, and a Prometheus Operator test |
| `charts/kardigan/` | The Helm chart; `files/` holds generated copies of the CRD and manager RBAC |
| `config/kind/` | Kustomize overlay pointing the operator at the local stack |
| `.github/` | CI and release workflows, Dependabot, CODEOWNERS, issue and PR templates |
| [`CLAUDE.md`](CLAUDE.md) | Conventions for AI coding assistants working in this repo |
| `api/`, `cmd/`, `internal/`, `config/`, `test/` | The operator; scaffolded by kubebuilder |

## 4. Architecture in one paragraph

A `CardinalityBudget` names a target (a `job` or `namespace` label value) and a `maxSeries`. The reconciler calls Mimir's `GET /prometheus/api/v1/cardinality/label_values` with `selector={label="value"}` and `count_method=active`, reads `series_count` from the `labels` entry, evaluates it with `internal/budget`, writes status and two conditions (`Ready` for query health, `WithinBudget` for usage), sets `kardigan_budget_*` gauges, and requeues after `spec.interval` (default 5m). It is observe-only: it never changes how metrics are collected. The reasoning is in [Decisions](#12-decisions).

A second controller writes a `CardinalityReport` for each budget, hourly. It compares the target's metrics in Mimir (`internal/mimir`) with what dashboards and rules read (`internal/grafana`, `internal/usage`), scores the difference (`internal/recommend`) and generates a relabel snippet.

```yaml
apiVersion: kardigan.paulgreco.ca/v1alpha1
kind: CardinalityBudget        # short name: cb
spec:
  target: { label: namespace, value: team-c }   # label: job | namespace
  maxSeries: 10000
  warnAtPercent: 80            # default
  interval: 5m                 # default; minimum 30s
```

| Name | Value |
| --- | --- |
| Go module | `github.com/pwgreco/kardigan` (set once at scaffold time; changing it later means rewriting every import) |
| API group | `kardigan.paulgreco.ca`, version `v1alpha1` |
| Kinds | `CardinalityBudget` (v0.1), `CardinalityReport` (v0.2) |
| Metrics | `kardigan_budget_series`, `kardigan_budget_max_series`, `kardigan_budget_usage_ratio`, `kardigan_mimir_query_errors_total` |
| Metric labels | `namespace` and `name` (the budget), `target_label` and `target_value` (what it measures); `kardigan_mimir_query_errors_total` has only the first two |
| Operator flags | `--mimir-url`, `--mimir-tenant`, `--mimir-timeout`, `--budget-admin-namespaces`, `--grafana-url`, `--grafana-timeout`, `--report-interval`; the token comes from `GRAFANA_TOKEN` |
| Licence | Apache-2.0 |

## 5. Local environment

| Service | URL | Notes |
| --- | --- | --- |
| Grafana | <http://localhost:9000> | Anonymous admin; Mimir data source provisioned |
| Mimir | <http://localhost:9009> | Tenant `demo` (send `X-Scope-OrgID: demo`); container `kardigan-mimir` |
| Prometheus | <http://localhost:9090> | Scrapes the teams and, when it runs locally, the operator on `host.docker.internal:8080` |
| Operator metrics | <http://localhost:8080/metrics> | Only with `--metrics-bind-address=:8080 --metrics-secure=false` |

| Team | Approximate active series | Sample `maxSeries` | Expected state |
| --- | --- | --- | --- |
| team-a | 500 | 1,000 | `WithinBudget` |
| team-b | 2,000 | 2,200 | `WarningThreshold` |
| team-c | 15,750 | 10,000 | `BudgetExceeded` |
| team-d | 1,000 | 2,000 | `WithinBudget`, with label and histogram advice in its report |

```bash
make stack-up     # start, and wait for every team's data and the demo dashboard
make stack-down   # stop and wipe
kind create cluster --name kardigan                # cluster for the CRD
```

## 6. Verified versus unverified

**Verified when the starter was built (October 2026):**

- `hack/local-stack/mimir.yaml` keys all exist in Mimir 3.2.4's config descriptor; the compose file passes `docker compose config`.
- Mimir response field names (`series_count_total`, `labels`, `label_name`, `cardinality`, `label_value`, `series_count`) and API defaults (`limit` 20, max 500; default `count_method` is `inmemory`) were read from Mimir 3.2.4 source.
- `kubebuilder init` v4.16 leaves every starter file unchanged and adds its own beside them.

**Verified on 2026-11-02:**

- The local stack runs end to end on Mimir 3.2.4. Observed active series are 505, 2,005 and 15,755, which PromQL `count()` confirms; each is the Avalanche count plus 5 from Prometheus.
- `series_count_total` ignores the selector and always holds the tenant's total. The earlier source reading got this wrong.
- The budget controller, run locally against the stack and kind: expected states for all three teams, no status-write loop, immediate spec edits, `Ready=False` during a Mimir outage with recovery inside a minute, gauges removed on delete, and the `kardigan` scrape target up. Grafana accepts the dashboard JSON and its query returns all three budgets through the data source; the rendered panel has not been checked by eye.

**Verified on 2026-11-03:**

- The operator deployed in kind: namespace budgets show the expected states, status writes succeed with the reduced RBAC, a cross-namespace budget gets `CrossNamespaceTarget`, an admin-namespace budget succeeds, and on the live metrics endpoint target labels appear, an edited target replaces its series and a deleted budget's series go.
- A CRD CEL rule can't read `self.metadata.namespace`; the API server refuses to compile it.
- On a fresh kind cluster: `install.yaml` installs and works once given a Mimir URL. The chart, packaged as the release workflow does and installed from an OCI registry, installs, upgrades, serves secure and plain metrics, refuses to install without `mimir.url`, keeps the CRD and budgets on uninstall, and reinstalls over them.

- The report controller, in kind against the local stack: a report per budget, `DropMetric`, `FixLabel` and `Keep` on real data, usage read from Grafana (panels, a collapsed row and a template variable), the Mimir ruler and a `PrometheusRule`, which moved a metric from `DropMetric` to `Keep`. A report comes back when deleted, and goes with its budget. A cross-namespace budget gets none. In a Grafana outage all reports went `Ready=False` within about 67 seconds and kept their recommendations, then recovered on the 5-minute retry. Memory is about 14 MiB after analysing six budgets.
- The chart on a fresh kind cluster, with both CRDs, a service account token, the viewer role, no Grafana configured (`NoUsageData`), and an uninstall that keeps the CRDs and budgets.
- Every metric of every job agrees with mimirtool 3.2.4 on used or unused and on series counts, apart from the five that Prometheus generates: 5 used and 50 unused, across 5 jobs (`TestAgainstMimirtool`).
- `DropLabel`, `FixLabel` and `TrimHistogram` on real data, from team-d. Team-d's snippet on a real scrape: samples went from 1,000 to 480 as the report claimed, series stayed distinct, and the budget fell to exactly 485 once Mimir's 20 minutes passed.
- The same snippet pasted into a `ServiceMonitor` on Prometheus Operator v0.94.2: the CRD accepted it, `replacement: ""` reached the generated config, and samples went from 1,000 to 480. A `PrometheusRule` through the real CRD moved a metric to `Keep`.

**Verified automatically:** `make test-all` repeats most of the above against the real stack on every run. See [`CONTRIBUTING.md`](CONTRIBUTING.md#test-against-real-mimir-and-grafana).

- `make test-integration`: client counts, and agreement with mimirtool.
- `make test-stack`: budget states and counts, report advice, spec edits, validation, cross-namespace rules, namespace targets with real series, metric and report removal, `PrometheusRule` objects, and Mimir and Grafana outages with recovery.
- `make test-extended`: a chart install and uninstall, the NetworkPolicy allowing and blocking real connections, and a report's snippet through the Prometheus Operator, where samples fall by exactly what the report claims.
- `make test-release`: a two-platform image with an SBOM and provenance in a local registry, non-root, with licences, plus `install.yaml` and the chart.

**Not verified:**

- A rejected Grafana token on a real Grafana. The local one allows anonymous admin, so it accepts bad tokens; only a unit test covers the failure. For the same reason, a Viewer token hasn't been shown to be enough on its own.
- `FixLabel` and `TrimHistogram` savings haven't been checked by making those changes at the source, and mimirtool doesn't give that advice. They're derived from `count()` queries and bucket counts.
- The reworked release workflow (the `verify` gate, the new step order, and the `sign` job, which only runs once the repo is public): it first runs on the first release from the public repo. `make lint-actions` checks only its syntax, and `make test-release` checks what it should produce, not the workflow itself. The same goes for every other workflow's new triggers. Watch the first release, and verify its signature with `cosign verify`.
- The Grafana panel has not been checked by eye. Its query returns data through the data source, but a headless browser hung on it.
- Pulling the published chart and image from GHCR.

When reality differs from this document, trust reality and fix it in the same PR.

## 7. Gotchas worth knowing before they bite

**Mimir**
- The cardinality endpoints return HTTP 400 unless `limits.cardinality_analysis_enabled: true`. On an existing Mimir, that's the first thing to check.
- Missing `X-Scope-OrgID` returns HTTP 401 when multitenancy is on.
- Always pass `count_method=active`. The default (`inmemory`) also counts quiet series still in the ingester head.
- `series_count_total` is the tenant's total whatever the selector. Read `labels[].series_count`; `"labels": []` means nothing matched.
- Too many parallel queries get HTTP 429 ("too many outstanding requests"), even on the local stack. `mimirtool analyze prometheus` skips those metrics with only a warning and writes a short result, so run it with `--concurrency=2` and check for `skipped` lines. Reports query Mimir one at a time for this reason.
- The Prometheus API lives under `/prometheus` (`http://localhost:9009/prometheus`). Tools that expect a bare Prometheus URL get 404 without it.

**Controller**
- Without `GenerationChangedPredicate`, each status write triggers another reconcile. A fast Mimir hides this, because a rewrite of `lastChecked` within the same second is a no-op; once queries take over a second, the operator queries Mimir nonstop (measured at 26 reconciles in 30 seconds, against 3 with the predicate).
- A Mimir error should set `Ready=False` and requeue after a minute, not return an error (which triggers exponential backoff and noisy logs).
- Delete a budget's gauge series when the budget is deleted, or stale values report forever. Match on `namespace` and `name` with `DeletePartialMatch`, since the target may be unknown by then. A label name typo there deletes nothing and raises no error, so use the constants.
- Don't clear gauges on every reconcile. A scrape between the delete and the set sees a gap, which resets an alert's `for` timer. Clear them only when the spec changed.
- `make deploy` rewrites `config/manager/kustomization.yaml`; don't commit that.
- After a rollout, budgets don't update for up to about 20 seconds while the new pod waits for the leader lease.

**Legal**
- Mimir and mimirtool are AGPL-3.0. Run the mimirtool binary for comparison; never import or copy its code.

**GitHub**
- `LICENSE` keeps the `[yyyy] [name of copyright owner]` placeholder on purpose: it's part of Apache-2.0's appendix. The copyright line belongs in file headers and `NOTICE`.
- A release created with the default `GITHUB_TOKEN` doesn't trigger other workflows, so the image build lives in the same workflow as release-please.
- release-please needs Settings, Actions, **Allow GitHub Actions to create and approve pull requests**.
- GHCR image names must be lowercase.
- A squash merge rewrites history, so a PR stacked on another conflicts once the lower one merges. Branch from `main` after each merge.
- `release-please` opens its release PR as soon as the automation reaches `main`. Merging that PR is what publishes.
- A label that passes the uniqueness check saves no active series, and one that would save series fails it. Colliding series fail silently: on Prometheus 3.15.1, 20 series became 1 with the target `up` and its duplicate, out-of-order and out-of-bounds counters at 0. Labels are checked together, because two can be redundant only with each other. Mimir's `label_names` ignores `count_method=active` and keeps listing labels on series that have gone, so reports count values with a query. Mimir also counts a series as active for 20 minutes after its last sample, while queries find nothing, so a zero count says nothing. `labeldrop` also applies to the whole scrape. Reports use `DropLabel`, `FixLabel` and a metric-scoped `replace` because of this.
- A report on a `job` target shows that job's metric names, and any namespace can create one, because a job label has no owning namespace. Budgets only showed a count. Namespace targets are protected by `--budget-admin-namespaces`.
- With no earlier release, release-please starts at `1.0.0` unless `initial-version` is set; the `0.0.0` manifest and `bump-minor-pre-major` don't change that.
- release-please puts the component in the tag (`kardigan-v0.1.0`) unless `include-component-in-tag` is false. The release workflow needs a plain `v0.1.0`, since it strips the `v` to get the chart version.

**Local stack and kind**
- The `kardigan` Prometheus target is DOWN until the operator runs locally. That's expected.
- Pods in kind reach Mimir only after `docker network connect kind kardigan-mimir`.
- Image tags in compose and kind manifests are pinned; Dependabot proposes bumps monthly.

## 8. Open questions

| Question | Where it gets decided |
| --- | --- |
| Should the Grafana dashboard JSON be provisioned automatically? | A small follow-up |
| Mine Mimir query-frontend logs for ad hoc query usage (raises recommendation confidence)? | Possible v0.3 |
| Enforcement: advisory only, collector-level drops, or per-team Mimir tenants? | The enforcement design |

## 9. Conventions

- One feature, one branch, one PR, even working alone (see [Decisions](#12-decisions)). Squash-merge with a Conventional Commit title (`feat:`, `fix:`, `test:`, `docs:`, `chore:`); release-please depends on it.
- Run `make manifests generate` after changing API types or RBAC markers; `make verify-generated` fails if generated files are stale.
- Never edit generated files (`zz_generated.deepcopy.go`, `config/crd/bases/`, `config/rbac/role.yaml`).
- Table-driven tests; the reconciler is tested with envtest and a fake `SeriesCounter`.
- Record any choice between real alternatives in [Decisions](#12-decisions).
- Use only made-up data: no real company names, cluster names, account IDs, hostnames, dashboards or config, including in examples and test fixtures.

## 10. Reference

- Tooling: Go 1.26+, kubebuilder v4.16+, kind, kubectl, Docker, `gh`; Helm v4 (`make helm` installs it to `bin/`)
- [Kubebuilder Book](https://book.kubebuilder.io) · [controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime) · [Mimir HTTP API](https://grafana.com/docs/mimir/latest/references/http-api/) · [PromQL parser](https://pkg.go.dev/github.com/prometheus/prometheus/promql/parser) · [Avalanche](https://github.com/prometheus-community/avalanche)

## 11. Roadmap

v0.1.0 shipped budgets and v0.2.0 shipped reports. Next is an enforcement design: whether Kardigan should act on what it finds, and how (advisory only, collector-level drops, or per-team Mimir tenants). Until it's accepted, Kardigan stays observe-only.

## 12. Decisions

| Decision | Why | Alternatives considered |
| --- | --- | --- |
| Name **Kardigan**, kind `CardinalityBudget` (`cb`) | Cardinality plus guardian; the kind limits active series and uses the term teams know | Kardinality, `MetricBudget`, `SeriesBudget` |
| Personal GitHub account | One maintainer; an org adds settings to manage for no gain | A `kardigan` org |
| Apache-2.0; never import Mimir or mimirtool code | AGPL would force itself onto Kardigan | AGPL-3.0 |
| Observe-only through v0.2 | Dropping series can silently break someone's alert | Patching ServiceMonitors |
| Poll with `RequeueAfter` and `GenerationChangedPredicate` | Mimir has no events; status writes would otherwise loop | A ticker goroutine |
| Cardinality API with `count_method=active` | One cheap call per budget | PromQL `count()`, `inmemory` |
| Two conditions: `Ready` and `WithinBudget` | "Mimir is down" and "team is over budget" have different owners | One combined condition |
| Export metrics; alerting stays in Grafana or the Mimir ruler | Reuses existing alert routing | Kardigan sends notifications itself |
| Go 1.26+, kubebuilder v4.16, `v1alpha1` | Current tooling; `v1alpha1` signals the API may change | |
| Domain `kardigan.paulgreco.ca`, API group `kardigan.paulgreco.ca` | Owned domain avoids API group collisions; the `kardigan.` prefix leaves room for other projects under the same domain; no `--group` prefix because all kinds are one API area | `kardigan.io` (not owned), `<user>.github.io`, bare `paulgreco.ca`, `observability.kardigan.paulgreco.ca` |
| Import alias `kardiganv1alpha1` for the API package | Readable; kubebuilder's default for an empty group is `kardiganpaulgrecocav1alpha1` | Keep the default, which avoids the duplicate import `create api` otherwise adds |
| `toolchain` directive in `go.mod` pins the patch release CI builds with | `setup-go` with `go-version-file` installs exactly what `go.mod` names, so `go 1.26.0` meant building on an unpatched standard library; `toolchain` raises the build version without raising the minimum for importers | Raise the `go` line; `go-version: stable` in each workflow |
| Dependabot, not Renovate, for dependency updates | Built into GitHub, already configured for all four ecosystems, updates SHA-pinned actions and tracks the `toolchain` line | Renovate (more flexible grouping and automerge, but needs an app install or self-hosting) |
| `SeriesCounter` lives in `internal/mimir`, beside `Client` | Keeps one place that describes talking to Mimir; there is one implementation plus a test fake | In the controller package, the usual "define interfaces where they're used" |
| Read `labels[].series_count`, not `series_count_total` | The total ignores the selector, so every budget would report the whole tenant (observed on Mimir 3.2.4) | `labels[].cardinality[].series_count`, which is the same number one level deeper; PromQL `count()` |
| One PR per feature, not per issue (2026-11-02) | Each feature is a reviewable unit and later work builds on earlier work, so per-issue PRs would be a long stack to merge in order | One PR per issue; one PR for everything |
| 30s minimum on `spec.interval`, enforced by a CEL rule in the CRD | Each check is one Mimir query per budget, and the API server rejects a bad value at `kubectl apply`, where the author sees it | Clamp in the reconciler (silently ignores what was asked for); a webhook (more to run); no floor |
| Mimir errors set `Ready=False` and retry after a fixed minute; `Reconcile` doesn't return them | Returning one means exponential backoff from 5ms up to about 16 minutes: a burst of queries against a struggling Mimir, then slow recovery | Return the error as well as setting status |
| Budgets may only target their own namespace, except in namespaces listed in `--budget-admin-namespaces` | Supports teams managing their own budgets and SRE managing all of them from one place; a controller check needs nothing extra installed and explains itself in status | Always allow (relies entirely on RBAC); no exemption (blocks central management); `ValidatingAdmissionPolicy` (only applies if installed); CRD CEL (can't read `metadata.namespace`) |
| `target_label` and `target_value` on the `kardigan_budget_*` gauges | A centrally managed budget's `namespace` label is the SRE namespace, so alerts need another label to route on; labels get harder to change once dashboards use them | Encode the team in the budget's name |
| Operator RBAC is read-only on budgets plus `update` on status | It never creates, edits or deletes budgets, and has no finalizers; this is what the scaffold's markers granted beyond its needs | Keep the scaffold's default verbs |
| Hand-written Helm chart in `charts/kardigan`, reading generated CRD and RBAC copies that `make manifests` keeps in sync | Small and readable, with no drift on generated parts because CI's stale-file check covers the copies | Kubebuilder's `helm/v2-alpha` plugin (alpha, more generated code to own); converting `install.yaml` |
| CRD in chart `templates/`, gated by `crds.install`, with `helm.sh/resource-policy: keep` | Helm never upgrades `crds/`, and this `v1alpha1` API will change; `keep` stops an uninstall deleting every budget | Helm's `crds/` directory |
| Publish the chart to GHCR as OCI, and attach it to the release | One registry for image and chart, no chart repo to host | A GitHub Pages chart repo |
| Chart version is set by the release workflow, not committed | One source of truth for the version; `Chart.yaml` holds placeholders | release-please updating `Chart.yaml` |
| Mimir address in a `config/kind` overlay, not `config/manager` | `install.yaml` is built from the base, and would ship the local stack's address and tenant | Edit the base (the original approach) |
| No `ServiceMonitor` in the chart for v0.1 | It needs a Prometheus Operator to test against, and its token setup deserves real testing; the metrics Service works with any scrape config | Ship one untested |
| Deployed builds run with `--zap-devel=false` | Development logging prints a stack trace for every failed query, so an outage floods the log | Change the default in `cmd/main.go`, which `make run` also uses |
| No Grafana screenshot in the README for v0.1.0 (2026-11-03) | Not needed yet; the README's `kubectl get cb -A` output shows what Kardigan does | Add one before release |
| Cross-compile the arm64 image instead of emulating it (2026-11-03) | The scaffold's Dockerfile ran the compiler under QEMU, which made the v0.1.0 image step take 27 minutes; Go cross-compiles natively, and the final stage has no `RUN` | Keep QEMU; build amd64 only |
| First release is 0.1.0, set with `initial-version` (2026-11-03) | Applies once and never needs removing; the API is `v1alpha1`, so 1.0.0 would promise stability it doesn't have | `Release-As` commit footer, or the deprecated `release-as` config, which keeps forcing the version until removed |
| Ship dependency licences in the image at `/licenses`, collected by `go-licenses` at build time (2026-11-03) | The image is what distributes the compiled dependencies, and a build-time scan can't go stale. It also fails the build on an unidentified licence | Commit the licences to the repo; publish them only as a release asset |
| Add `FixLabel`, and report `DropLabel` with `seriesSaved: 0` (2026-11-03) | Measured on the local stack: a label that passes a uniqueness check saves nothing, and one that would save series fails it. Claiming savings would mislead | Show `labeldrop` with a saving it can't deliver |
| Remove labels with a metric-scoped `replace`, not `labeldrop` (2026-11-03) | `labeldrop` can't name a metric, so it would remove the label from metrics nothing checked, which can make series collide | `labeldrop`, only if the check passes on every metric in the scrape, which costs far more queries |
| A source that can't be read stops the analysis (2026-11-03) | A metric in a dashboard Kardigan couldn't read isn't unused. The last good recommendations stay, with `Ready=False` | Carry on with partial data at low confidence |
| List `PrometheusRule` objects uncached, not watched (2026-11-03) | A watch would need the CRD to exist for the operator to start, and rules only matter hourly | Unstructured informer with a CRD check at startup |
| Write our own usage extraction instead of running mimirtool (2026-11-03) | AGPL rules out its code and it has no library; running the binary would still mean bundling it and working around its gaps | Ship the mimirtool binary in the image and parse its JSON; drop reports and leave this to mimirtool |
| Reserve 5 of the 25 report entries for `Keep` (2026-11-03) | `Keep` saves nothing, so it would always be cut, and teams need it to see why a metric stayed | Strict top 25 by savings |
| Remove `scripts/` (2026-11-03) | It seeded the issues once and isn't needed again | Keep it |
| `make verify` and `make ci` are the source of truth; workflows wrap Makefile targets (2026-11-03) | The same checks run with or without Actions, so an outage doesn't stop development | Keep the logic in workflow steps |
| Limit the README's FAQ to behaviour and limitations (2026-11-03) | Readers evaluating or running Kardigan don't need the reasoning, which is in this table | Keep architecture and contributing answers there |
| Keep upstream error text out of status conditions (2026-11-03) | Anyone who can read a budget could otherwise see Mimir's address and up to 512 bytes of what it answered; the log still has it all. Costs some convenience when debugging with `kubectl` | Copy `err.Error()` into the message, as before |
| Fail on oversized responses: 32 MiB from Mimir, 16 MiB from Grafana (2026-11-03) | Protects a pod with a 128 MiB limit. Failing keeps the rule that a source it can't read stops the analysis | Truncate; stream the JSON; a configurable limit |
| Remove `AGENTS.md`, `.devcontainer/`, webhook flags and cert-manager from e2e (2026-11-03) | All unused scaffold. `AGENTS.md` described targets that don't exist, and the devcontainer ran privileged with unpinned tools | Keep them as scaffold convention |
| Dependabot uses `fix` for Go modules and Docker (2026-11-03) | release-please doesn't release for `chore`, so a security update would merge and never ship. It means more release PRs, which only publish when merged | Keep `chore` and cut releases by hand |
| Sign images with keyless cosign, in a job that only runs when the repo is public (2026-11-04) | Keyless signing needs no key to manage, but writes the repo's name and workflow to a public log, which a private repo shouldn't do | Sign regardless; manage a signing key; no signing |
| `job` targets stay unrestricted (2026-11-03) | A job label has no owning namespace, so there's nothing to check it against. Documented in the FAQ; RBAC limits who can create budgets. Worth revisiting for a multi-tenant cluster | Only admin namespaces may target jobs; a flag to choose |
| Test against the real stack with `make test-integration` and `make test-stack`, outside `make ci` and hosted CI (2026-11-03) | e2e never talks to Mimir, so `make ci` couldn't show budgets or reports work. These need the whole stack and take minutes, so they suit a merge or release, not every push | Add them to `make ci`; extend e2e with a fake Mimir, which wouldn't catch a wrong assumption about the real one |
| Split tests against the stack: `test-stack` for behaviour, `test-extended` and `test-release` before a release (2026-11-04) | The snippet, chart and artefact checks are slower to set up and matter less per merge, so a behaviour change only pays for `test-stack` | One suite for everything |
| Publish from a fresh repo with one squashed commit; first public release is 0.3.0 (2026-11-11) | The private history held process churn that don't belong in a public record; a clean start also leaves no dead commit or PR links | Publish the history as is; rewrite it with `git filter-repo`, which changes every commit ID and leaves the old commits reachable through PR refs |
