# Kardigan Helm chart

Installs the Kardigan operator and the `CardinalityBudget` and `CardinalityReport` CRDs.

```bash
helm install kardigan oci://ghcr.io/pwgreco/charts/kardigan \
  --namespace kardigan-system --create-namespace \
  --set mimir.url=http://mimir-gateway.mimir.svc:8080 \
  --set mimir.tenant=my-tenant
```

## Values

| Value | Default | Description |
| --- | --- | --- |
| `mimir.url` | none, required | Base URL of Mimir's HTTP API, without `/prometheus` |
| `mimir.tenant` | `""` | Tenant sent as `X-Scope-OrgID`. Leave empty when multitenancy is off |
| `mimir.timeout` | `10s` | Timeout for each request to Mimir |
| `grafana.url` | `""` | Base URL of Grafana. Without it, reports only check rules, and their recommendations have low confidence |
| `grafana.tokenSecret.name` | `""` | Secret holding a read-only service account token that can see every dashboard |
| `grafana.tokenSecret.key` | `token` | Key in that Secret |
| `grafana.timeout` | `30s` | Timeout for each request to Grafana |
| `reportInterval` | `1h` | How often to regenerate each report |
| `budgetAdminNamespaces` | `[]` | Namespaces whose budgets may target any namespace |
| `metrics.enabled` | `true` | Serve metrics and create a Service for them |
| `metrics.secure` | `true` | Serve over HTTPS with token authentication |
| `metrics.port` | `8443` | Metrics port |
| `networkPolicy.enabled` | `false` | Allow ingress only on the metrics port, from `networkPolicy.metricsFrom` |
| `networkPolicy.metricsFrom` | `[]` | NetworkPolicy peers allowed to scrape metrics |
| `crds.install` | `true` | Install and upgrade the CRDs with the chart |
| `image.repository` | `ghcr.io/pwgreco/kardigan` | Operator image |
| `image.tag` | the chart's `appVersion` | Operator image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Secrets for pulling the image |
| `serviceAccount.create` | `true` | Create a service account |
| `serviceAccount.name` | the release's full name | Service account to use; required when `create` is false |
| `serviceAccount.annotations` | `{}` | Annotations for the service account |
| `replicaCount` | `1` | Only the leader acts; more replicas give faster failover |
| `extraArgs` | `[]` | Extra command-line flags for the operator |
| `resources` | see `values.yaml` | Pod resources |
| `podAnnotations`, `podLabels` | `{}` | Extra pod metadata |
| `nodeSelector`, `tolerations`, `affinity` | empty | Pod scheduling |
| `nameOverride`, `fullnameOverride` | `""` | Resource naming |

## Scraping metrics

The metrics are how alerting works, so point your Prometheus at the `<release>-metrics` Service.

With `metrics.secure=true`, bind the scraper's service account to the `<release>-metrics-reader` ClusterRole and scrape over HTTPS with its token. The certificate is self-signed, so the scraper must skip verification. With `metrics.secure=false`, scrape plain HTTP, and limit who can reach the port. To limit it with a NetworkPolicy, set `networkPolicy.enabled=true` and list your Prometheus under `networkPolicy.metricsFrom`, if your network plugin enforces them.

The chart doesn't create a `ServiceMonitor` yet, so add one or a scrape config yourself.

## Reading dashboards and rules

Reports compare a team's metrics with what dashboards and rules read:

```bash
kubectl create secret generic grafana-token -n kardigan-system --from-literal=token=<token>
helm upgrade kardigan oci://ghcr.io/pwgreco/charts/kardigan --reuse-values \
  --namespace kardigan-system \
  --set grafana.url=http://grafana.monitoring.svc \
  --set grafana.tokenSecret.name=grafana-token
```

Rules in Mimir's ruler and `PrometheusRule` objects are read without any setup. If a source can't be read, reports say so in their `Ready` condition and don't recommend anything new, because a metric in a dashboard Kardigan can't read would look unused.

## Granting access to budgets

Bind the `<release>-budget-editor` or `<release>-budget-viewer` ClusterRole to the people who manage budgets. Both can read reports. A RoleBinding limits it to one namespace.

## CRD lifecycle

The CRDs are chart templates, so `helm upgrade` updates them. They're annotated `helm.sh/resource-policy: keep`, so `helm uninstall` leaves them and every budget in place. To remove them too, run `kubectl delete crd cardinalitybudgets.kardigan.paulgreco.ca cardinalityreports.kardigan.paulgreco.ca`.

If the CRDs already exist from `make install` or `install.yaml`, Helm refuses to adopt them. Install with `--set crds.install=false`, or delete them first, which also deletes budgets and reports.
