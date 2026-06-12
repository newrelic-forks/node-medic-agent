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
	"k8s.io/node-problem-detector/internal/nodemedic/metrics"
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

	// Phase 3 (T044) wires the reconciler + node watcher here.
	// For Phase 2, the manager runs with no controllers — it serves
	// /metrics, /healthz, /readyz and exits cleanly on SIGTERM.
	klog.Info("manager ready; reconcilers wire in Phase 3 (US1)")

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
		"http://nodemedic-agent.container-fabric.svc:8080/diagnose",
		"agent service URL receiving POST /diagnose")

	fs.StringVar(&opts.clusterName, "cluster-name", "",
		"cluster name (required; MUST start with `test-`)")

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

// validateClusterName enforces Constitution Article I.9 at the binary
// level: the controller refuses to start if --cluster-name is missing
// or doesn't start with the literal prefix `test-`.
func validateClusterName(name string) error {
	if name == "" {
		return errors.New("--cluster-name is required (Constitution Article I.9)")
	}
	if !strings.HasPrefix(name, "test-") {
		return fmt.Errorf("--cluster-name=%q must start with `test-` "+
			"(Constitution Article I.9: test clusters only)", name)
	}
	return nil
}
