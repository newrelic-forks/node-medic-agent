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
	"strconv"
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
	// deadlineWindow is the wall-clock budget the agent gets to write
	// status.diagnosis from observedAt. NodeWatcher stamps
	// spec.budgets.deadline = observedAt + deadlineWindow at case
	// creation time. The NHD reconciler watches `now > deadline` while
	// phase=Diagnosing and goes Failed{DeadlineExceeded} on hit (modulo
	// one retry, see retryDeadlineExtension). Bumped from the original
	// 60s to 5m for clusters where probes can run long (Azure CLI hangs,
	// deep ssh fan-out, ambiguous-pattern decision trees).
	deadlineWindow time.Duration
	// retryDeadlineExtension is how much spec.budgets.deadline gets
	// pushed forward when a case hits DeadlineExceeded with retryCount==0.
	// FR-7 caps the number of retries at 1; effective wall-clock budget
	// is therefore deadlineWindow + retryDeadlineExtension.
	retryDeadlineExtension time.Duration
	metricsAddr            string
	probeAddr              string
	// stubAgent short-circuits POST /diagnose: every call is treated
	// as 202 `queued` without touching the network. Demo helper for
	// when Scope 3's agent service isn't deployed yet — the operator
	// hand-patches status.diagnosis via `kubectl apply --subresource=status`
	// to drive the gate / cordon path. NEVER set in production.
	stubAgent bool
	// useBlockKit selects the Slack message format. true (default once
	// Spec 003 ships) routes terminal-phase posts through the
	// BuildBlockKit* builders; false routes through the legacy
	// BuildApplied/BuildHumanInLoop/BuildCritical builders. The
	// legacy builders stay alive in messages.go as the demo-day
	// rollback path. Helm value: config.useBlockKit. Env: USE_BLOCK_KIT.
	useBlockKit bool
	// uiBaseURL is the base URL the "View full diagnosis" Block Kit
	// button points at. Default http://localhost:8080 matches the
	// quickstart `kubectl port-forward` demo path. Helm value:
	// config.uiBaseURL. Env: UI_BASE_URL.
	uiBaseURL string
	// slackChannel is a display string only — actual webhook routing
	// lives in the nodemedic-slack-webhook Secret URL. Carried so the
	// controller's startup log line can name the channel for FR-4
	// visibility. Helm value: config.slackChannel. Env: SLACK_CHANNEL.
	slackChannel string
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

	// FR-4 startup log line: name all three Block Kit flags so the
	// operator can confirm chart values reached the binary. Event tag
	// matches the schema asserted by quickstart.md and T030.
	klog.Info("controller_startup",
		"event", "controller_startup",
		"clusterName", opts.clusterName,
		"agentURL", opts.agentURL,
		"watchedConditions", opts.watchedConditions,
		"minConfidence", opts.minConfidence,
		"minEvidenceSources", opts.minEvidenceSources,
		"debounceWindow", opts.debounceWindow,
		"deadlineWindow", opts.deadlineWindow,
		"retryDeadlineExtension", opts.retryDeadlineExtension,
		"useBlockKit", opts.useBlockKit,
		"uiBaseURL", opts.uiBaseURL,
		"slackChannel", opts.slackChannel,
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
	if agentToken == "" && !opts.stubAgent {
		klog.Info("WARNING: NODEMEDIC_AGENT_TOKEN env is empty; agent calls will likely 401")
	}
	slackWebhook := os.Getenv("NODEMEDIC_SLACK_WEBHOOK_URL")
	if slackWebhook == "" {
		klog.Info("WARNING: NODEMEDIC_SLACK_WEBHOOK_URL env is empty; Slack posts will fail")
	}

	var agentCli controller.AgentClient
	if opts.stubAgent {
		klog.Info("--stub-agent=true; bypassing real agent service. NOT FOR PRODUCTION.")
		agentCli = &agentclient.StubAgent{}
	} else {
		agentCli = agentclient.New(opts.agentURL, agentToken)
	}
	slackCli := notifier.NewSlack(slackWebhook)

	nhdRec := &controller.NHDReconciler{
		Client:                 mgr.GetClient(),
		Recorder:               mgr.GetEventRecorderFor("nodemedic-controller"),
		Agent:                  agentCli,
		Slack:                  slackCli,
		MinConfidence:          opts.minConfidence,
		MinEvidenceSources:     opts.minEvidenceSources,
		ClusterName:            opts.clusterName,
		Namespace:              opts.namespace,
		RetryDeadlineExtension: opts.retryDeadlineExtension,
		UseBlockKit:            opts.useBlockKit,
		UIBaseURL:              opts.uiBaseURL,
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
		DeadlineWindow:    opts.deadlineWindow,
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

	fs.DurationVar(&opts.deadlineWindow, "deadline-window", 5*time.Minute,
		"wall-clock budget the agent has to write status.diagnosis from observedAt. "+
			"NodeWatcher stamps spec.budgets.deadline = observedAt + this value at case creation. "+
			"On DeadlineExceeded the reconciler retries once with --retry-deadline-extension; "+
			"effective ceiling is deadline-window + retry-deadline-extension.")
	fs.DurationVar(&opts.retryDeadlineExtension, "retry-deadline-extension", 60*time.Second,
		"how far spec.budgets.deadline is pushed forward on a single DeadlineExceeded retry. "+
			"FR-7 caps retries at 1. Set 0 to disable retry-side extension entirely.")

	fs.StringVar(&opts.metricsAddr, "metrics-bind-address", ":9443",
		"address on which the metrics endpoint binds")
	fs.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081",
		"address on which the healthz/readyz endpoints bind")

	fs.BoolVar(&opts.stubAgent, "stub-agent", false,
		"DEMO HELPER ONLY: bypass POST /diagnose; pretend every call returns 202 queued. "+
			"Use while Scope 3's agent isn't deployed; hand-patch status.diagnosis "+
			"via `kubectl apply --subresource=status` to drive the gate path. "+
			"NEVER set in production.")

	// Spec 003 US1 flags. All three accept env-var fallbacks via
	// envOrFlag below so the chart can plumb them as plain env vars on
	// the Deployment env block.
	fs.BoolVar(&opts.useBlockKit, "use-block-kit", parseBoolEnv("USE_BLOCK_KIT", true),
		"if true, post terminal-phase Slack messages in Block Kit format; "+
			"if false, fall back to the legacy plain-text builders. "+
			"Helm value: config.useBlockKit. Env: USE_BLOCK_KIT.")
	fs.StringVar(&opts.uiBaseURL, "ui-base-url", envOr("UI_BASE_URL", "http://localhost:8080"),
		"base URL the Block Kit \"View full diagnosis\" button points at. "+
			"Helm value: config.uiBaseURL. Env: UI_BASE_URL.")
	fs.StringVar(&opts.slackChannel, "slack-channel", envOr("SLACK_CHANNEL", "#nodemedic-demo"),
		"Slack channel name (display only — webhook routing is in the "+
			"nodemedic-slack-webhook Secret). Helm value: config.slackChannel. Env: SLACK_CHANNEL.")

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

// envOr returns os.Getenv(key) if non-empty, else def. Used so flags
// can fall back to env vars without hand-rolling each branch.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseBoolEnv reads key from the environment and parses it as a bool.
// Unset, empty, or unparseable → def. Accepts the same shapes as
// strconv.ParseBool (1/0/t/f/true/false/T/F/TRUE/FALSE/True/False).
func parseBoolEnv(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// validateClusterName enforces Constitution Article I.9 at the binary
// level: the controller refuses to start unless --cluster-name is one of
//   - "cf1z" (the Azure kubeadm test cluster used for the hackathon —
//     legacy CF naming alongside jc1z / sk1z)
//   - any name with a "test-" prefix (AWS/EKS test clusters, e.g.
//     "test-odd-wire")
//
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
