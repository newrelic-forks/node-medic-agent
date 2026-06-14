# Implementation Plan: NodeMedic On-Call UI + Slack Format Upgrade

**Branch**: `hackathon-2026/cf1z-baseline` (UI code lands directly on this branch — same pattern Specs 001 and 002 used. If parallel work surfaces, a `hackathon-2026/scope4-oncall-ui` branch can be cut from `cf1z-baseline` without a plan revision.) | **Date**: 2026-06-14 | **Spec**: [`spec.md`](./spec.md)

**Input**: Feature specification at `.specify/specs/003-nodemedic-oncall-ui/spec.md`

**Constitution**: [`.specify/memory/constitution.md`](../../memory/constitution.md) (single living doc; Article I.2's deliberate scope expansion for human-initiated drain is bound below in the Constitution Check table)

## Summary

NodeMedic On-Call UI is a Go-backed, server-rendered HTML surface that turns the existing `NodeHealthDiagnosisAI` (NHD) CRD into a glance-and-act on-call experience. It ships two routes — a 24h list view (`/`) and a per-case detail page (`/cases/<nhd-name>`) — plus three action endpoints that mutate the underlying `Node` directly via the kube API (uncordon, drain via `pods/eviction` SSE-streamed, clear-skipDeletion). It also replaces the controller's plain-text Slack post with a Block Kit message whose primary action is a deep-link to the per-case page. No new CRD, no new cross-scope HTTP contract, no agent or controller code paths change beyond the Slack message builder. The audit trail is one new annotation on the NHD (`nodemedic.cf.newrelic.com/ui-action-history`, FIFO ring buffer of 20 entries) plus structured stdout logs.

**Approach (from research):** Go 1.24 + stdlib `net/http` + `html/template` for SSR; `controller-runtime`'s split client (cached informer for reads, direct client for writes); native `EventSource` over `http/Flusher.Flush()` for the drain stream; plain `fetch()` + `setInterval(_, 30000)` for the list-view refresh; `BackgroundConcurrency=3` for the eviction loop (tasks complete in parallel, results stream out in arrival order); fields-grid Block Kit shape (`section.fields`, two columns) with a `text` mrkdwn fallback in `attachments[].fallback`; new Helm chart at `deployment/helm/nodemedic-oncall-ui/` mirroring the agent's chart structure (`requireTestCluster` guard, per-cloud values split, ServiceAccount + ClusterRole pattern); new `Dockerfile.nodemedic-oncall-ui` (multi-stage, `linux/amd64`); image tag `cf-registry.nr-ops.net/container-fabric/nodemedic-oncall-ui:dev-cf1z-<sha>` consistent with controller + agent.

The Slack channel + UI base URL stay per-install Helm values (`config.slackChannel`, `config.uiBaseURL`); neither is bound in chart defaults.

## Technical Context

**Language/Version**: Go 1.24.x. Same major as the controller (`go.mod` line 5 in this repo: `go 1.24`). Reusing the controller's toolchain and CI shape is the cheapest reviewer-cognitive-load decision; the UI is the third Go binary in this repo (NPD, controller, UI). The agent stays Python — the UI is not adjacent to the agent's runtime.

**Primary Dependencies**:
- `net/http` (stdlib) — HTTP server, the three action endpoints, the SSE drain stream, the static-asset serving. No `gin` / `echo` / `chi` (research R-1).
- `html/template` (stdlib) — server-rendered list and per-case pages. Trusted-by-default escaping is the right default for a surface that reads NHD content the agent wrote (LLM output) and renders it into HTML.
- `sigs.k8s.io/controller-runtime` v0.21.x — only for the **client builder + scheme registration**. The UI is not a controller, so the manager and reconciler machinery is unused; `client.New(...)` returns a typed split client that reads from the cached informer (list/get) and writes to the apiserver (eviction, annotation patches). Same dep the controller already pulls (verified via `go.sum`).
- `k8s.io/client-go` v0.33.x — pulled transitively by controller-runtime; we touch its `pods/eviction` subresource API directly (`client-go`'s `policyv1.Eviction`).
- `k8s.io/api`, `k8s.io/apimachinery` v0.33.x — same provenance.
- `k8s.io/node-problem-detector/api/v1alpha1` (this repo) — the Go types for `NodeHealthDiagnosisAI` are owned by Spec 001 and live at `api/v1alpha1/nodehealthdiagnosisai_types.go`. The UI imports the package directly; no schema duplication.
- Vanilla HTML/CSS/JS — no framework, no build step, no `node_modules`. The JS surface is ~150 lines covering: per-action POST, drain SSE consumer, `setInterval` refresh on the list page, action-button enable/disable bookkeeping, "show more / show less" toggles for evidence entries.

**Storage**: N/A. The UI is stateless. All state lives in the apiserver — NHDs (read), Nodes (read/patch), Pods (read), the `ui-action-history` annotation on the NHD (read/patch). No SQLite, no Redis, no in-memory cache beyond controller-runtime's informer (which is shared across requests within the process and refreshed by the apiserver's watch — no cache-coherence work for us). Drain in-flight state is a process-local `sync.Map[node-name -> *DrainProgress]` so concurrent button clicks during a drain are coalesced (FR-9 G9 idempotency on uncordon/clear; "drain in progress" indicator on drain).

**Testing**:
- **Unit (Go)**: `go test ./...` against pure-function targets — `slack/messages.BuildBlockKit*` golden-output tests (mirror the existing `internal/nodemedic/notifier/messages_test.go` shape), `internal/oncall/audit.appendEntry` (FIFO ring buffer at N=20, FR-18a invariants), `internal/oncall/drain.podShouldEvict` (DaemonSet / mirror / `system-node-critical` priority filter), `internal/oncall/handlers.validateNodeMatchesCase` (FR-17 defensive belt). Table-driven, no mocks.
- **HTTP integration (Go)**: `httptest.NewServer` against the assembled handler chain with a fake kube client (`sigs.k8s.io/controller-runtime/pkg/client/fake`). Fixtures under `tests/oncall_ui/fixtures/`: a cordoned-node NHD with `decision=Applied`, a HumanInLoop NHD, a node-reclaimed NHD (FR-17a), an NHD with 25 pre-existing audit entries (FR-18a). Exercises the three action endpoints' happy paths + idempotent paths + 400 (mis-routed) + 200 with "already reclaimed" body.
- **Drain SSE contract test**: `httptest.NewRecorder` + a hand-rolled `http.Flusher` capture; assert the on-the-wire frames are `event: <name>\ndata: <json>\n\n` shaped, terminator `event: complete` is present even when the loop hits zero pods.
- **Helm lint**: `helm lint deployment/helm/nodemedic-oncall-ui --set clusterName=cf1z`. Cluster-name guard is the chart's only required input.
- **E2E**: AC-1 through AC-14 + AC-14a/b/c from spec §8, run by hand on cf1z. The deployed `chaos-kubelet-unhealthy` cronjob is the case-generator. The Slack post is verified against the configured channel; the UI is verified through `kubectl port-forward svc/nodemedic-oncall-ui 8080:8080`.

**Target Platform**: Linux container, `linux/amd64`. cf1z is amd64-only (Azure VMs); the controller and agent images are amd64-only for the same reason. The Dockerfile uses `--platform=$BUILDPLATFORM` on the build stage so Apple Silicon dev hosts cross-compile cleanly via Colima; the runtime stage stays `linux/amd64`.

**Project Type**: Long-lived synchronous HTTP service (Go + stdlib). Single-binary container. Coexists with NPD, the Spec 001 controller, and the Spec 002 agent in the same repo: UI code lands under `cmd/nodemedic-oncall-ui/` (entry point) + `internal/oncall/...` (handlers, audit, drain, slack helpers shared with the controller's notifier package). No existing path changes except for Spec 001's Slack-notifier package, which we **extend** (new Block Kit builder functions sit alongside the existing plain-text builders; the controller's call site moves to the new builders behind a feature flag bound by a Helm value).

**Performance Goals**:
- List view request → first byte: ≤ 200 ms p99 on cf1z (informer cache hit; no apiserver round-trip).
- Per-case page request → first byte: ≤ 250 ms p99 (one informer get + one direct apiserver get for the Node — Nodes aren't watched by the informer to keep RBAC tight, see FR-21).
- Action endpoint (uncordon, clear-skip-deletion): apiserver patch + audit annotation patch + page refresh = 2 sequential apiserver ops, target ≤ 800 ms p99.
- Drain endpoint: per-pod eviction call ≤ 250 ms p95; summary write at end of stream ≤ 500 ms; total wall-clock dominated by PDB + grace-period (the apiserver enforces both). Target: 30 typical pods drain in ≤ 90 s on cf1z. The progress stream gives the engineer a sense of motion the whole time.
- Slack post latency: unchanged from today (controller's existing `notifier.Slack.Post` retry policy: 3 attempts, 1 s/2 s/4 s backoff, 5 s per-attempt timeout — we only change the payload bytes).

**Constraints**:
- **Credential-layer least privilege is the safety boundary** (Constitution Article I.1). UI ServiceAccount carries the four verb sets in spec FR-21 / G8 / AC-12 only — `nodes get/list/watch/patch`, `pods/eviction create`, `pods get/list/watch`, `nodemedic.cf.newrelic.com/nodehealthdiagnosisais get/list/watch/patch`. No `delete`, no `create` on nodes, no broader resource access. CI grep test (parallel to the agent's existing `nodes/patch` guard, but mirrored: the UI MUST have `nodes/patch`, MUST NOT have `nodes/delete`) fails the build on drift.
- **Cordon-only stays a controller property** (Constitution Article I.2). The UI's Drain button is the **deliberate scope expansion** named in spec §0 Q5 + §3 — human-initiated, gated by a confirmation modal that lists the pods, executed against `pods/eviction` (which respects PDBs and `system-node-critical`). No agent or controller path reaches eviction; the UI is the only surface that does. The constitution's literal text is "the cordon executor is not an LLM tool" — the UI is human-driven, the gate is the confirmation modal, and the eviction call is RBAC-scoped to the UI ServiceAccount only.
- **Every action is observable** (Constitution Article I.4). Two surfaces per action: structured stdout JSON log line (FR-18) AND ring-buffer audit annotation on the NHD CR (FR-18a). The annotation is the durable, kubectl-readable surface; the stdout is the live tail.
- **Non-production clusters only** (Constitution Article I.5). The Helm chart's `_helpers.tpl` accepts `cf1z`/`jc1z`/`sk1z`/`test-*` and rejects everything else (mirrors the controller and agent charts byte-for-byte). The UI binary's `validateClusterName` startup guard is the belt to the chart's suspenders (mirrors the controller's `cmd/nodemedic-controller/main.go` pattern).
- **Two cross-scope contracts only** (Constitution Article II.1). The UI introduces ZERO new cross-scope contracts. It reads the existing NHD CRD (Spec 001 owns the schema) and the existing Node/Pod core resources. It writes one new annotation on the NHD CR (`nodemedic.cf.newrelic.com/ui-action-history`) — new field, but on an existing object, with no schema change to the CRD itself. The annotation is documented in [`contracts/ui-action-history.schema.json`](./contracts/ui-action-history.schema.json) for forward-compat.
- **Slack post stays a controller responsibility**. The UI is not a Slack client. The notifier package in `internal/nodemedic/notifier/` keeps owning the webhook secret, the retry policy, and the post call site. We extend the message **builder** (`messages.go`); we don't move the post path. This keeps the cross-scope surface (controller → Slack) singular.
- **Image registry path locked**: `cf-registry.nr-ops.net/container-fabric/nodemedic-oncall-ui:dev-cf1z-<short-sha>`, mirroring the controller and agent paths. Developer cf-registry creds work — no service account required (verified empirically against the controller image push 2026-06-14, see commit `61865c62`).
- **kubectl port-forward is the demo access path**. The UI is a `ClusterIP` Service in `cf-monitoring`. The Slack 'View full diagnosis' button URL defaults to `http://localhost:8080/cases/<nhd-name>` via Helm value `config.uiBaseURL`. Public ingress + DNS + TLS are post-hackathon hardening, not a v1 concern.

**Scale/Scope**:
- 1 UI instance per cluster (single replica). Stateless; HPA is unnecessary at the demo's load profile.
- Typical demo load: 1 engineer, ~5 list-view loads + ~3 detail-view loads + ~2 action clicks per case. Peak rehearsal: 10 concurrent NHDs in the list view; ~30 pods in a single drain.
- One target cluster on Day 1: cf1z. Other CF dev clusters (`jc1z`, `sk1z`) and `test-*` clusters are surface-eligible but not part of the demo flow.
- One demo NodeCondition path on Day 1: `KubeletUnhealthy` (Spec 002 Phase 5's headline win). `ContainerRuntimeUnhealthy` is also viable; the UI is condition-agnostic.

**Plan-phase decisions** (the four §10 callouts from the spec):

These are bound here, not deferred to tasks.md. Research details + alternatives are in [`research.md`](./research.md).

- **PD-1 → Block Kit JSON shape**: `section.fields` two-column grid (research R-2). Up to 4 fields rendered as a 2×2 grid: `Decision`, `Confidence`, `Trigger`, `rcaCategory`. Header is `header` block with severity emoji + verb + node + cluster. Action button is in an `actions` block, single primary `button` with `style: "primary"`. `text` (top-level mrkdwn fallback) carries a single-line plain-text version for clients that don't render Block Kit (Slack legacy mobile, certain bridges); `attachments[].color` carries severity per Clarifications session §0 Q8.
- **PD-2 → Server-side rendering vs client-side fetch**: SSR with `html/template` for the initial paint of both `/` and `/cases/<nhd-name>`; small `fetch()` calls for (a) list auto-refresh (re-renders the table tbody from JSON), (b) per-action POSTs, (c) drain SSE consumption. **No HTMX, no React, no build step.** (Research R-1.) The JS surface stays under 150 lines; the HTML stays readable in `view-source`.
- **PD-3 → Pod eviction loop concurrency**: `sem := make(chan struct{}, 3)` — three in-flight evictions max. Per-pod results stream out as they arrive (preserving the spec's serial-with-progress contract from FR-16; the engineer perceives motion either way) but parallelism keeps a 30-pod node draining in ~30 s instead of ~90 s. PDB-violated evictions return immediately and don't block the cap. (Research R-3.)
- **PD-4 → List-view auto-refresh implementation**: `setInterval(refreshList, 30000)` calling `GET /api/cases` and re-rendering the `<tbody>` via template literals. SSE is reserved for drain (where push semantics matter); list view can poll without burning anything visible to the engineer. (Research R-4.)

**Clarifications resolved**: All 12 spec-level clarifications (§0) are bound by the spec itself; this plan does not revisit them. The four PD-* decisions above are the plan-level questions §10 explicitly named.

No open clarifications remain. Phase 0 research is final; Phase 1 artifacts reflect these decisions.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The spec already binds the constitution-relevant trade-offs in §1 (hackathon scope), §3 (non-goals), and §0 Q5 (Drain-vs-Article-I.2). This table cross-checks each Article against the plan-phase technology choices.

| Article | Rule | How this plan satisfies it |
|---|---|---|
| I.1 Credential-layer least privilege | Every credential mounted to the UI pod scoped to the minimum verbs | Helm chart renders one ClusterRole with: `nodes get/list/watch/patch`, `pods/eviction create`, `pods get/list/watch`, `nodemedic.cf.newrelic.com/nodehealthdiagnosisais get/list/watch/patch`. No `delete`, no `create` on nodes. Verified by AC-12 + a CI grep test (`! grep -E 'nodes/(delete\|create)\|secrets\|configmaps' deployment/helm/nodemedic-oncall-ui/templates/clusterrole.yaml`). The UI pod has NO Anthropic / NR / Azure / AWS credentials — those belong to the agent. ✅ |
| I.2 Cordon-only (drain prohibition) | No drain, eviction, deletion, termination — that's the "cordon executor is not an LLM tool" rule | The agent and controller paths are unchanged: the agent has no `nodes/patch`; the controller only cordons. The UI's Drain button is the **deliberate scope expansion** named in spec §0 Q5. The constitution's literal prohibition is on the LLM-driven path; the UI is human-driven, with a confirmation modal that names every pod that would be evicted, executed via the `pods/eviction` subresource (PDB-respecting, `system-node-critical`-skipping). The expansion is captured here, not slipped in. CI grep test: the controller chart still has no `pods/eviction` verb (`! grep "pods/eviction" deployment/helm/nodemedic-controller/templates/clusterrole.yaml`); the agent chart still has no `nodes/patch` verb. ✅ |
| I.3 Confidence gate is binding | `confidence ≥ 0.7`, ≥2 distinct sources, action ∈ {Cordon, DrainAndCordon} | N/A for this plan — the gate is the controller's responsibility (Spec 001 FR-6) and the UI does not run a gate. The UI does respect the gate's outcome by **rendering the controller's decision** in both the Slack header (severity emoji per FR-2) and the per-case page banner. ✅ |
| I.4 Every action observable | Hooks / log lines / per-action records | Two surfaces, both bound: (1) FR-18 — structured JSON stdout log line per action call (`{ts, event:"ui_action", action, nhd_name, node, actor, result, duration_ms}`); (2) FR-18a — append-only ring-buffer annotation on the NHD CR. The annotation is the durable surface; the stdout log is the live tail. Both share `nhd_name` so `kubectl logs deploy/nodemedic-oncall-ui --since=1h \| jq 'select(.event == "ui_action")'` reconstructs the same audit trail the annotation shows. ✅ |
| I.5 Non-production clusters only | Deployment on `cf1z` / `jc1z` / `sk1z` / `test-*` only | Helm `_helpers.tpl` `requireTestCluster` accepts the four shapes only; rejects everything else. Mirrors the controller and agent charts. UI binary's `validateClusterName` startup guard is the belt to the chart's suspenders (refuses to start if `CLUSTER_NAME` env is empty or doesn't match the allowlist — FR-23). ✅ |
| I (hackathon simp.) Anonymous access | UI is anonymous behind `kubectl port-forward` | Spec FR-3 + §3 non-goals + §0 Q4. Action log lines record `actor: "demo-anonymous"` (FR-18). SSO restoration is post-hackathon. ✅ |
| I (hackathon simp.) Ephemeral audit | No durable audit store beyond NHD annotation + stdout | Spec §3 non-goals + FR-18 + FR-18a. The NHD annotation is etcd-backed (durable for the CR's lifetime); stdout is durable for the pod + log-rotation window. ✅ |
| II.1 Cross-scope contracts | NHD CRD + `POST /diagnose` only | The UI introduces zero new cross-scope contracts. It reads the existing NHD CRD and core Node/Pod resources. It writes one new annotation key (`nodemedic.cf.newrelic.com/ui-action-history`) **on an existing object** — a payload change, not a schema change. The annotation's JSON Schema is documented in [`contracts/ui-action-history.schema.json`](./contracts/ui-action-history.schema.json) for forward-compat with later UI versions, but it is NOT a CRD change and does NOT introduce a new RPC. ✅ |
| II.2 CRD owned by Scope 3 | Schema authority lives with the agent | The UI does not own any CRD field. The new annotation is a `metadata.annotations.<key>` payload — not a `spec.*` or `status.*` field — so this article is not engaged. ✅ |
| II.3 Day 1 stub | Each scope ships a stub | Spec 003 doesn't have a Day 1 stub requirement (Specs 001 and 002 are deployed; this plan ships against the live system). The closest analog: the Slack message builder ships a feature-flag default (`config.useBlockKit=true` from the start) so the controller's call site can fall back to the existing plain-text builder by Helm-value flip if Block Kit rendering is broken on demo morning. The flag lives in the controller's chart, not the UI's. ✅ |
| II.4 Agent does not call controller | One direction enforced | N/A — neither the agent nor the controller calls the UI. The UI is a sink, not a source. ✅ |
| II.5 Controller does not call MCP | Tool surface belongs to the agent | N/A. ✅ |
| II.6 Cluster topology passed in | Controller writes provider/region/instanceId on `spec.case` | The UI reads these fields verbatim from `spec.case` and renders them on the per-case page. No discovery, no inference. ✅ |
| II.7 One binary, two clouds | Same image, AWS + Azure | One `Dockerfile.nodemedic-oncall-ui`, one image. The UI is cloud-agnostic (it never calls AWS or Azure SDKs); per-cluster Helm install only differs in the cluster-name + image-tag values. ✅ |
| III.1 Cite, don't claim | Verifiable references | This plan, research.md, data-model.md, contracts/, quickstart.md cite spec sections, file:line, and clarification entries. ✅ |
| III.2 Stub before integrate | Day 1 stubs mandatory | See II.3. The UI ships against the existing live controller + agent; no stub is needed since both upstream scopes are already deployed and stable on cf1z. ✅ |
| III.3 Out of scope stays out | No SSO, no auth, no public ingress, no multi-cluster | Spec §3 non-goals is comprehensive and binding (13 items). The plan introduces no scope additions. The deliberate scope expansion (human-initiated drain) is bound in I.2 above and is not a non-goals violation — it's an explicit, captain-acknowledged exception with a named safety gate. ✅ |
| III.4 Demo flow drives priorities | KubeletUnhealthy demo path first | The plan's tasks-phase sequence (deferred to `/speckit-tasks`) will land US1 + US2 + US3 (Slack format + list/detail views + uncordon/clear-skipDeletion) before US4 (drain) before US5 (audit history surfacing). Drain is P2 specifically because the demo's headline is "click → uncordon → MLC reclaim", not "click → drain". ✅ |
| III.5 Ask before pivoting | Surface blockers, don't silently rearchitect | All 12 clarifications were resolved through the `/speckit-clarify` pass before `/speckit-plan` ran. Any new blocker discovered in Phase 2 implementation goes back to a captain. ✅ |

**Gate result: PASS.** One deliberate scope expansion (human-initiated drain via the UI) is captured in Article I.2 above with the spec §0 Q5 rationale and the safety gate explicitly named. No other deviations from the constitution. Complexity Tracking section below is empty.

## Project Structure

### Documentation (this feature)

```text
.specify/specs/003-nodemedic-oncall-ui/
├── plan.md              # this file
├── spec.md              # feature spec (already authored, plan-ready)
├── research.md          # Phase 0 output (this run)
├── data-model.md        # Phase 1 output (this run)
├── quickstart.md        # Phase 1 output (this run)
├── contracts/
│   ├── oncall-ui-api.yaml          # OpenAPI 3 for the UI's HTTP endpoints (list, detail, three actions, SSE drain)
│   ├── ui-action-history.schema.json # JSON Schema for the new NHD annotation payload
│   └── slack-block-kit.md          # Frozen golden Block Kit JSON bodies (Applied, HumanInLoop, Failed)
├── checklists/                     # already populated by /speckit-checklist
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (in this repo — coexists with NPD + the controller + the agent)

The UI lives alongside the existing Go binaries (NPD, controller) and the Python agent. All existing paths are untouched **except** `internal/nodemedic/notifier/messages.go`, which gains new Block Kit builder functions next to the existing plain-text ones. The controller's call site flips to the new builders behind a Helm value (`config.useBlockKit`, default `true` once Spec 003 ships).

```text
node-medic-agent/                        # this repo (Go module: k8s.io/node-problem-detector)
├── cmd/
│   ├── nodeproblemdetector/             # existing NPD command (untouched)
│   ├── healthchecker/                   # existing (untouched)
│   ├── logcounter/                      # existing (untouched)
│   ├── nodemedic-controller/            # Spec 001 controller
│   │   └── main.go                      # MODIFIED — wires the new Block Kit builders behind --use-block-kit flag
│   ├── nodemedic-agent/                 # Spec 002 agent (untouched)
│   └── nodemedic-oncall-ui/             # NEW — UI binary entry point
│       └── main.go                      # parses flags, validates cluster name, builds the kube client, starts the HTTP server
├── api/                                 # Spec 001 CRD types (untouched — schema authority)
│   └── v1alpha1/
│       └── nodehealthdiagnosisai_types.go
├── internal/
│   ├── nodemedic/                       # Spec 001 controller (mostly untouched)
│   │   ├── notifier/
│   │   │   ├── messages.go              # MODIFIED — adds BuildBlockKitApplied/HumanInLoop/Failed alongside existing builders
│   │   │   ├── messages_test.go         # MODIFIED — golden tests for the new builders
│   │   │   ├── slack.go                 # untouched (the post path doesn't change)
│   │   │   └── slack_test.go            # untouched
│   │   └── ...                          # other controller internals untouched
│   └── oncall/                          # NEW — UI internals
│       ├── server/
│       │   ├── server.go                # http.Server assembly, route table, graceful shutdown
│       │   ├── render.go                # html/template loader + executors
│       │   └── middleware.go            # access logging, request-id, recovery
│       ├── handlers/
│       │   ├── list.go                  # GET / + GET /api/cases (FR-5..FR-8)
│       │   ├── detail.go                # GET /cases/<nhd-name> (FR-9..FR-13)
│       │   ├── uncordon.go              # POST /api/cases/<nhd-name>/actions/uncordon (FR-14)
│       │   ├── drain.go                 # POST /api/cases/<nhd-name>/actions/drain (FR-16, SSE)
│       │   ├── clear_skip.go            # POST /api/cases/<nhd-name>/actions/clear-skip-deletion (FR-15)
│       │   └── helpers.go               # validateNodeMatchesCase (FR-17), nodeReclaimed (FR-17a) — shared by the three action handlers
│       ├── audit/
│       │   ├── annotation.go            # appendEntry (FIFO ring buffer at N=20, FR-18a) + readEntries
│       │   └── log.go                   # structured stdout JSON logger for ui_action events (FR-18)
│       ├── drain/
│       │   ├── plan.go                  # podShouldEvict filter (DaemonSet, mirror, system-node-critical) — pure function
│       │   ├── execute.go               # eviction loop with sem-bounded concurrency (PD-3) + SSE event emitter
│       │   └── progress.go              # process-local sync.Map[nodeName] *DrainProgress for "drain in flight" coalescing
│       ├── kube/
│       │   ├── client.go                # controller-runtime client builder, scheme registration, in-cluster config
│       │   └── informer.go              # informer for NHD list/get; direct client for Nodes/Pods/eviction
│       ├── render/                      # html/template files + their data structs
│       │   ├── templates/
│       │   │   ├── layout.html.tmpl
│       │   │   ├── list.html.tmpl
│       │   │   ├── detail.html.tmpl
│       │   │   └── _action_history.html.tmpl
│       │   ├── static/
│       │   │   ├── styles.css
│       │   │   ├── list.js              # setInterval + fetch + tbody re-render
│       │   │   ├── detail.js            # action POSTs + SSE consumer + show-more toggles
│       │   │   └── modals.js            # confirmation modal scaffolding (uncordon, drain, clear-skip)
│       │   └── data.go                  # ListPageData, DetailPageData, ActionHistoryEntry — fed to html/template
│       └── slack/                       # NOT A CONTROLLER PACKAGE — this is the UI's Block Kit URL builder, used by the controller
│           └── url.go                   # BuildCaseURL(uiBaseURL, nhdName) — single function, kept here so the URL shape lives in one place; the controller imports this package
├── deployment/
│   ├── helm/
│   │   ├── nodemedic-controller/        # MODIFIED — adds config.uiBaseURL + config.useBlockKit values
│   │   ├── nodemedic-agent/             # untouched
│   │   └── nodemedic-oncall-ui/         # NEW — mirrors the agent chart's shape exactly
│   │       ├── Chart.yaml
│   │       ├── values.yaml              # defaults; clusterName unset; image.tag empty
│   │       ├── values-azure.yaml        # cf1z install
│   │       ├── values-eks.yaml          # placeholder for future test-* install
│   │       └── templates/
│   │           ├── _helpers.tpl         # requireTestCluster (cf1z + jc1z + sk1z + test-*) — same pattern as agent chart
│   │           ├── deployment.yaml      # UI Deployment (1 replica, ClusterIP exposed via Service)
│   │           ├── service.yaml         # ClusterIP :8080 → :8080
│   │           ├── serviceaccount.yaml
│   │           ├── clusterrole.yaml     # FR-21 minimal verbs
│   │           └── clusterrolebinding.yaml
│   └── ...                              # existing deploy files untouched
├── tests/
│   └── oncall_ui/                       # NEW — Go tests for the UI live alongside the package
│       ├── unit/
│       │   ├── audit_ringbuffer_test.go
│       │   ├── drain_filter_test.go
│       │   ├── handlers_validate_test.go
│       │   └── slack_block_kit_test.go  # builder golden tests (also lives in internal/nodemedic/notifier — see below)
│       ├── integration/
│       │   ├── list_handler_test.go
│       │   ├── detail_handler_test.go
│       │   ├── action_uncordon_test.go
│       │   ├── action_clear_skip_test.go
│       │   ├── action_drain_sse_test.go
│       │   ├── action_node_reclaimed_test.go  # FR-17a
│       │   └── action_mismatch_test.go        # FR-17 defensive belt
│       └── fixtures/
│           ├── nhd_applied.yaml
│           ├── nhd_human_in_loop.yaml
│           ├── nhd_node_reclaimed.yaml
│           └── nhd_audit_full_buffer.yaml     # 20 pre-existing entries — FR-18a boundary
├── Dockerfile                           # existing NPD image (untouched)
├── Dockerfile.nodemedic-controller      # Spec 001 image (untouched)
├── Dockerfile.nodemedic-agent           # Spec 002 image (untouched)
├── Dockerfile.nodemedic-oncall-ui       # NEW — multi-stage Go build, linux/amd64 runtime, distroless static base
├── Makefile                             # extended with `nodemedic-oncall-ui-*` targets;
│                                        #   NPD + controller + agent targets unchanged
├── go.mod / go.sum / vendor/            # the UI imports controller-runtime + client-go, already present
├── .github/workflows/
│   ├── nodemedic-ci.yml                 # Spec 001 controller CI (untouched)
│   ├── nodemedic-agent-ci.yml           # Spec 002 agent CI (untouched)
│   ├── nodemedic-oncall-ui-ci.yml       # NEW — go test + helm lint + RBAC grep guard
│   └── ...                              # existing NPD workflows (untouched)
└── .specify/specs/003-nodemedic-oncall-ui/  # spec-kit artifacts (this plan)
```

**Structure Decision**: The UI is a third Go binary in this repo (NPD, controller, UI) and follows the controller's layout pattern exactly: `cmd/<binary>/main.go` for the entry point, `internal/<package>/...` for the domain logic, `tests/<package>/...` for tests outside `internal/`. The agent's Python tree (`nodemedic_agent/...`) is unaffected; Go and Python coexist at the repo root because Go's `*.go` package detection and the agent's `pyproject.toml` package discovery never collide.

**Slack-builder location**: The new Block Kit builders extend the existing `internal/nodemedic/notifier/messages.go` rather than living under `internal/oncall/slack/messages.go`. Reason: the post path (`internal/nodemedic/notifier/slack.go`) belongs to the controller, the call site is in the controller's reconciler, and keeping the message + post code in one package keeps the controller chart's Slack secret mounted only in one place. The UI's `internal/oncall/slack/url.go` is just a small URL builder the controller imports — it doesn't post anything.

**Helm chart shape**: Mirrors the agent's `deployment/helm/nodemedic-agent/` exactly — same `_helpers.tpl` `requireTestCluster` macro, same per-cloud values split, same one-resource-per-template-file layout. A reviewer who has read the agent chart can read this chart in five minutes.

**Build isolation**: UI has its own Dockerfile (`Dockerfile.nodemedic-oncall-ui`) and Make target prefix (`make nodemedic-oncall-ui-*`). The controller's, agent's, and NPD's targets stay untouched. CI gets a separate workflow that triggers only on UI paths.

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

(No violations beyond the constitution-permitted hackathon-scope simplifications. The one deliberate scope expansion — human-initiated drain through the UI — is captured in the Constitution Check table under Article I.2 with the spec §0 Q5 rationale and the safety gate explicitly named. Section intentionally left empty.)
