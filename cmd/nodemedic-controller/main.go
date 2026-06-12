/*
Copyright 2026 The New Relic Container Fabric authors.

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

// Command nodemedic-controller is the entrypoint for the NodeMedic
// Controller (Scope 2 of the AFA 2026 hackathon).
//
// In Phase 2 (this commit) it stands up the manager + scheme + metrics
// without registering any reconcilers. Reconcilers wire in during
// Phase 3 (User Story 1) per
// .specify/specs/001-nodemedic-controller/tasks.md T044.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nodemedicv1alpha1 "k8s.io/node-problem-detector/api/v1alpha1"
	"k8s.io/node-problem-detector/internal/nodemedic/agentclient"
	"k8s.io/node-problem-detector/internal/nodemedic/controller"
	"k8s.io/node-problem-detector/internal/nodemedic/metrics"
	"k8s.io/node-problem-detector/internal/nodemedic/notifier"
)

// scheme holds the registered API types for this manager. Built once at
// startup; never mutated after Start.
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodemedicv1alpha1.AddToScheme(scheme))
}

// options bundles the controller flags. Defaults match spec NFR-6.
type options struct {
	watchedConditions  string
	minConfidence      float64
	minEvidenceSources int
	agentURL           string
	clusterName        string
	namespace          string
	debounceWindow     time.Duration
	metricsAddr        string
	probeAddr          string
}

func main() {
	if err := run(); err != nil {
		// stderr because we may not have a logger yet
		fmt.Fprintf(os.Stderr, "nodemedic-controller: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	opts := parseFlags()

	// Constitution Article I.9 hard guard. The Helm chart also rejects
	// non-test cluster names; this is the binary-level belt + suspenders.
	if err := validateClusterName(opts.clusterName); err != nil {
		return err
	}

	// Set up the controller-runtime logger first so any subsequent error
	// from manager construction lands in the right format.
	zlog := zap.New(zap.UseDevMode(false))
	ctrl.SetLogger(zlog)
	klog := ctrl.Log.WithName("nodemedic-controller")

	klog.Info("starting NodeMedic Controller",
		"clusterName", opts.clusterName,
		"agentURL", opts.agentURL,
		"watchedConditions", opts.watchedConditions,
		"minConfidence", opts.minConfidence,
		"minEvidenceSources", opts.minEvidenceSources,
		"debounceWindow", opts.debounceWindow,
	)

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return fmt.Errorf("get rest config: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: opts.metricsAddr,
		},
		HealthProbeBindAddress: opts.probeAddr,
		// No leader election in v1 — single replica per spec NFR-2 +
		// research R-7. Promotes to true post-hackathon if needed.
		LeaderElection: false,
	})
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	// Register all NFR-3 collectors against the manager's metrics
	// registry before any reconciler starts.
	metrics.MustRegister()

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	// Wire the NHD reconciler + node watcher (Phase 3 / US1).
	// Build the agent + Slack clients first so we can panic-fast at
	// startup if a Secret-mounted env var is missing rather than
	// discover it mid-reconcile.
	agentToken := os.Getenv("NODEMEDIC_AGENT_TOKEN")
	if agentToken == "" {
		klog.Info("WARNING: NODEMEDIC_AGENT_TOKEN env is empty; agent calls will likely 401")
	}
	slackWebhook := os.Getenv("NODEMEDIC_SLACK_WEBHOOK_URL")
	if slackWebhook == "" {
		klog.Info("WARNING: NODEMEDIC_SLACK_WEBHOOK_URL env is empty; Slack posts will fail")
	}

	agentCli := agentclient.New(opts.agentURL, agentToken)
	slackCli := notifier.NewSlack(slackWebhook)

	nhdRec := &controller.NHDReconciler{
		Client:             mgr.GetClient(),
		Recorder:           mgr.GetEventRecorderFor("nodemedic-controller"),
		Agent:              agentCli,
		Slack:              slackCli,
		MinConfidence:      opts.minConfidence,
		MinEvidenceSources: opts.minEvidenceSources,
		ClusterName:        opts.clusterName,
		Namespace:          opts.namespace,
	}
	if err := nhdRec.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NHD reconciler: %w", err)
	}

	nodeWatcher := &controller.NodeWatcher{
		Client:            mgr.GetClient(),
		Recorder:          mgr.GetEventRecorderFor("nodemedic-controller"),
		WatchedConditions: parseWatchedConditions(opts.watchedConditions),
		Debounce:          controller.NewDebounceMap(opts.debounceWindow),
		ClusterName:       opts.clusterName,
		Namespace:         opts.namespace,
		MaxTurns:          15,
		MaxBudgetUSD:      "0.50",
		DeadlineWindow:    60 * time.Second,
	}
	if err := nodeWatcher.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup node watcher: %w", err)
	}

	klog.Info("controllers wired", "watchedConditions", opts.watchedConditions)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}

func parseFlags() options {
	fs := flag.NewFlagSet("nodemedic-controller", flag.ExitOnError)

	var opts options

	// Default watched-conditions list covers the seven fault classes from
	// nodemedic-scope.md §4.2 (spec FR-1 last paragraph).
	fs.StringVar(&opts.watchedConditions, "watched-conditions",
		"ConntrackSaturated,FDExhaustion,PIDExhaustion,InodeExhaustion,DiskFill,DNSPartition,IMDSThrottle",
		"comma-separated NodeCondition.type values that trigger case creation")

	// Constitution Article I.3 thresholds (default 0.7 / 2). Tunable via
	// flag for demo, but the structure of the gate is hard-coded.
	fs.Float64Var(&opts.minConfidence, "min-confidence", 0.7,
		"minimum confidence score for the gate to pass")
	fs.IntVar(&opts.minEvidenceSources, "min-evidence-sources", 2,
		"minimum number of distinct evidence sources for the gate to pass")

	fs.StringVar(&opts.agentURL, "agent-url",
		"http://nodemedic-agent.cf-monitoring.svc:8080/diagnose",
		"agent service URL receiving POST /diagnose")

	fs.StringVar(&opts.clusterName, "cluster-name", "",
		"cluster name (required; MUST be `cf1z` or start with `test-`)")

	fs.StringVar(&opts.namespace, "namespace", "cf-monitoring",
		"namespace where NodeHealthDiagnosisAI CRs are created")

	fs.DurationVar(&opts.debounceWindow, "debounce-window", 30*time.Second,
		"per (node, condition) debounce window for trigger detection")

	fs.StringVar(&opts.metricsAddr, "metrics-bind-address", ":9443",
		"address on which the metrics endpoint binds")
	fs.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081",
		"address on which the healthz/readyz endpoints bind")

	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag.ExitOnError already calls os.Exit(2) on parse failure;
		// this branch is defensive.
		os.Exit(2)
	}
	return opts
}

// parseWatchedConditions splits the comma-separated --watched-conditions
// flag into a normalized slice (trimmed, empty entries dropped).
func parseWatchedConditions(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateClusterName enforces Constitution Article I.9 at the binary
// level: the controller refuses to start unless --cluster-name is one of
//   - "cf1z" (the Azure kubeadm test cluster used for the hackathon —
//     legacy CF naming alongside jc1z / sk1z)
//   - any name with a "test-" prefix (AWS/EKS test clusters, e.g.
//     "test-odd-wire")
// Anything else — empty, "stg-*", "us-*", "eu-*", or other production
// shapes — is rejected before the manager comes up.
func validateClusterName(name string) error {
	if name == "" {
		return errors.New("--cluster-name is required (Constitution Article I.9)")
	}
	if name == "cf1z" {
		return nil
	}
	if strings.HasPrefix(name, "test-") {
		return nil
	}
	return fmt.Errorf("--cluster-name=%q must be either `cf1z` (Azure kubeadm) "+
		"or start with `test-` (AWS/EKS test clusters) "+
		"(Constitution Article I.9: test clusters only)", name)
}
