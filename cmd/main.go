/*
Copyright 2026 Paul Greco.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/controller"
	"github.com/pwgreco/kardigan/internal/grafana"
	"github.com/pwgreco/kardigan/internal/mimir"
	"github.com/pwgreco/kardigan/internal/recommend"
	"github.com/pwgreco/kardigan/internal/usage"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kardiganv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var mimirURL, mimirTenant, adminNamespaces, grafanaURL string
	var mimirTimeout, grafanaTimeout, reportInterval time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	flag.StringVar(&mimirURL, "mimir-url", "http://localhost:9009", "Base URL of Mimir's HTTP API.")
	flag.StringVar(&mimirTenant, "mimir-tenant", "",
		"Tenant sent as X-Scope-OrgID. Leave empty when Mimir multitenancy is off.")
	flag.DurationVar(&mimirTimeout, "mimir-timeout", 10*time.Second, "Timeout for each Mimir request.")
	flag.StringVar(&adminNamespaces, "budget-admin-namespaces", "",
		"Comma-separated namespaces whose budgets may target any namespace. Budgets elsewhere may only target their own.")
	flag.StringVar(&grafanaURL, "grafana-url", "",
		"Base URL of Grafana, whose dashboards show which metrics are used. The token comes from GRAFANA_TOKEN. "+
			"Without it, only rules are checked and recommendations have low confidence.")
	flag.DurationVar(&grafanaTimeout, "grafana-timeout", 30*time.Second, "Timeout for each Grafana request.")
	flag.DurationVar(&reportInterval, "report-interval", time.Hour, "How often to regenerate each CardinalityReport.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	grafanaToken := os.Getenv("GRAFANA_TOKEN")
	if err := validateUpstreams(mimirURL, grafanaURL, mimirTimeout, grafanaTimeout); err != nil {
		setupLog.Error(err, "Invalid flags")
		os.Exit(1)
	}
	if grafanaURL != "" && grafanaToken == "" {
		setupLog.Info("GRAFANA_TOKEN is empty, so Grafana is read anonymously. " +
			"A dashboard that can't be read anonymously looks unused")
	}
	if strings.HasPrefix(grafanaURL, "http://") && grafanaToken != "" {
		setupLog.Info("Sending the Grafana token over plain HTTP. Use an https:// URL unless the network is trusted")
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. Scrapers then have to skip verification,
	// so set --metrics-cert-path to serve a real certificate where that matters.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "223a6969.kardigan.paulgreco.ca",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	mimirClient := &mimir.Client{
		BaseURL: mimirURL,
		Tenant:  mimirTenant,
		HTTP:    &http.Client{Timeout: mimirTimeout},
	}
	budgetAdmins := strings.Split(adminNamespaces, ",")

	if err := (&controller.CardinalityBudgetReconciler{
		Client:          mgr.GetClient(),
		Counter:         mimirClient,
		AdminNamespaces: budgetAdmins,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cardinalitybudget")
		os.Exit(1)
	}

	usageSources := []usage.Source{
		{Name: "mimir-ruler", Fetch: mimirClient.RuleExpressions},
		{Name: "prometheusrules", Fetch: controller.PrometheusRuleExpressions(mgr.GetAPIReader())},
	}
	if grafanaURL != "" {
		grafanaClient := &grafana.Client{
			BaseURL: grafanaURL,
			Token:   grafanaToken,
			HTTP:    &http.Client{Timeout: grafanaTimeout},
		}
		usageSources = append(usageSources, usage.Source{Name: recommend.DashboardSource, Fetch: grafanaClient.Expressions})
	}

	if err := (&controller.CardinalityReportReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Source:          mimirClient,
		Usage:           &usage.Cache{Sources: usageSources, TTL: reportInterval},
		AdminNamespaces: budgetAdmins,
		Interval:        reportInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "cardinalityreport")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// validateUpstreams rejects a Mimir or Grafana address the HTTP client couldn't
// use, or a timeout that would let a stuck request hang a reconcile forever. An
// empty grafanaURL means Grafana isn't configured.
func validateUpstreams(mimirURL, grafanaURL string, mimirTimeout, grafanaTimeout time.Duration) error {
	if err := validateBaseURL("mimir-url", mimirURL); err != nil {
		return err
	}
	if grafanaURL != "" {
		if err := validateBaseURL("grafana-url", grafanaURL); err != nil {
			return err
		}
	}
	if mimirTimeout <= 0 || grafanaTimeout <= 0 {
		return errors.New("--mimir-timeout and --grafana-timeout must be greater than zero")
	}
	return nil
}

func validateBaseURL(flagName, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("--%s: %w", flagName, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("--%s must be an http or https URL with a host, got %q", flagName, raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("--%s must not contain credentials", flagName)
	}
	return nil
}
