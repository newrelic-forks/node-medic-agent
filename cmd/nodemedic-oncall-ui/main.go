// Command nodemedic-oncall-ui is the entrypoint for the NodeMedic
// On-Call UI (Scope 4 of the AFA 2026 hackathon).
//
// Phase 2 stands up the HTTP server with stub handlers for every
// route. Phases 4/5/6 attach real handlers per
// .specify/specs/003-nodemedic-oncall-ui/tasks.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"k8s.io/node-problem-detector/internal/oncall/handlers"
	"k8s.io/node-problem-detector/internal/oncall/kube"
	"k8s.io/node-problem-detector/internal/oncall/render"
	"k8s.io/node-problem-detector/internal/oncall/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nodemedic-oncall-ui: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseConfig()

	if err := server.ValidateClusterName(cfg.ClusterName); err != nil {
		return err
	}

	kc, _, err := kube.BuildClient()
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	srv, err := server.New(cfg, kc)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	// Phase 4 (US2) — list + detail handlers.
	listDeps := handlers.ListDeps{Client: kc, Namespace: cfg.Namespace, Logger: srv.Logger()}
	detailDeps := handlers.DetailDeps{Client: kc, Namespace: cfg.Namespace, Logger: srv.Logger()}
	srv.SetRouteHandler("GET /", handlers.ListHTML(listDeps))
	srv.SetRouteHandler("GET /api/cases", handlers.ListJSON(listDeps))
	srv.SetRouteHandler("GET /cases/{nhd}", handlers.DetailHTML(detailDeps))
	srv.SetStaticHandler(http.StripPrefix("/static/", http.FileServer(http.FS(render.StaticFS()))))

	// Phase 5 (US3) — uncordon + clear-skip-deletion handlers wired
	// in attachActions; falls through to 501 stub if helpers aren't
	// loaded yet (e.g. during a partial Phase-5 build).
	attachActions(srv, kc, cfg)

	srv.Logger().Log("startup_complete",
		"listen_addr", cfg.ListenAddr,
		"cluster_name", cfg.ClusterName,
		"namespace", cfg.Namespace,
		"ui_base_url", cfg.UIBaseURL,
		"drain_concurrency", cfg.DrainConcurrency,
		"audit_buffer_size", cfg.AuditBufferSize,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return srv.Run(ctx)
}

// parseConfig binds CLI flags + env vars per data-model.md §9. Env vars
// take precedence over flag defaults but flags override env (flag-after-
// env is the standard Go pattern: `flag.StringVar` defaults pull from
// env, then `fs.Parse` overrides).
func parseConfig() server.Config {
	fs := flag.NewFlagSet("nodemedic-oncall-ui", flag.ExitOnError)

	cfg := server.Config{}

	fs.StringVar(&cfg.ListenAddr, "listen-addr", env("LISTEN_ADDR", ":8080"),
		"address the HTTP server binds to")
	fs.StringVar(&cfg.ClusterName, "cluster-name", env("CLUSTER_NAME", ""),
		"cluster name (required; MUST be cf1z|jc1z|sk1z or start with test-)")
	fs.StringVar(&cfg.Namespace, "namespace", env("NAMESPACE", "cf-monitoring"),
		"namespace where NodeHealthDiagnosisAI CRs are read")
	fs.StringVar(&cfg.UIBaseURL, "ui-base-url", env("UI_BASE_URL", "http://localhost:8080"),
		"base URL the UI is reachable at (used for self-aware deep-links if any)")
	fs.StringVar(&cfg.LogLevel, "log-level", env("LOG_LEVEL", "info"),
		"log level: info | debug | warn | error")
	fs.IntVar(&cfg.DrainConcurrency, "drain-concurrency", envInt("DRAIN_CONCURRENCY", 3),
		"max in-flight evictions per drain (research R-3)")
	fs.IntVar(&cfg.AuditBufferSize, "audit-buffer-size", envInt("AUDIT_BUFFER_SIZE", 20),
		"FIFO ring buffer size for the ui-action-history annotation (FR-18a)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	return cfg
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
