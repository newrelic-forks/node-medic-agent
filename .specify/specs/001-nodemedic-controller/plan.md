# Implementation Plan: NodeMedic Controller

**Branch**: `hackathon-2026/scope2-controller` (cut from `hackathon-2026/cf1z-baseline` 2026-06-12; controller code lives in **this** repo, namespaced under `cmd/nodemedic-controller/` and `internal/nodemedic/...` rather than a separate `nodemedic-controller` repo as originally suggested in `nodemedic-scope.md` §5.1) | **Date**: 2026-06-12 | **Spec**: [`spec.md`](./spec.md)

**Input**: Feature specification at `.specify/specs/001-nodemedic-controller/spec.md`

**Constitution**: [`.specify/memory/constitution.md`](../../memory/constitution.md) v1.0

## Summary

NodeMedic Controller is a single-replica, namespaced controller-runtime operator for one Kubernetes test cluster. It watches `Node` + `Event` for NPD-flipped `NodeCondition`s in a configurable allowlist, resolves cloud metadata off the `Node` object (labels + `spec.providerID`), creates one `NodeHealthDiagnosisAI` (NHD) CR per `(node, condition)` event, calls the agent over `POST /diagnose`, reconciles the NHD phase machine, and on `Diagnosed` applies the Constitution Article I.3 confidence gate. Pass → `kubectl cordon`-equivalent patch (`spec.unschedulable=true`) + Slack `Applied`. Fail → no mutation + Slack `HumanInLoop` (`severity=critical` framing). Agent failure → retry once, then Slack `Critical`. Same binary, same Helm chart, runs unchanged on EKS and Azure kubeadm test clusters.

**Approach (from research):** controller-runtime v0.19.x scaffolded by Kubebuilder; CRD + types vendored from Scope 3 once the schema is published, frozen in our `api/v1alpha1/` until Scope 3 bumps it; two informers (Node-driven case creator, NHD-driven reconciler) sharing one `manager`; Slack via stdlib `net/http`; metrics via the manager's built-in Prometheus registry; envtest for the reconciler, gomock'd HTTP transport for the Slack/agent clients, real fixtures captured Day 1 AM for the EKS/Azure providerID parsers.

## Technical Context

**Language/Version**: Go 1.25.x (matches the NPD fork's `go.mod`; controller-runtime v0.19+ requires ≥1.22 so this is comfortably above floor).

**Primary Dependencies**:
- `sigs.k8s.io/controller-runtime` v0.19.x (manager, informers, predicate filters, server-side apply, status subresource)
- `k8s.io/client-go` v0.31.x and `k8s.io/api`/`apimachinery` v0.31.x (matches controller-runtime v0.19)
- `sigs.k8s.io/controller-tools` (`controller-gen`) for CRD + deepcopy + RBAC manifest generation
- `github.com/google/uuid` for `caseId` generation (UUIDv4)
- `github.com/prometheus/client_golang` (already pulled transitively by controller-runtime; we register custom metrics on the manager's registry)
- `github.com/stretchr/testify` for assertions
- `sigs.k8s.io/controller-runtime/pkg/envtest` for the binary-backed apiserver test harness
- Slack: stdlib `net/http` + `encoding/json` (no third-party SDK — keeps the dep surface minimal; Block Kit JSON is hand-built)

**Storage**: N/A. All state lives in the NHD CR (`status.phase`, `status.conditions[]`, `status.action.*`) and Kubernetes Events on the `Node`/NHD. No database, no local volumes.

**Testing**:
- Unit: Go `testing` + `testify`. Pure functions only — `confidenceGate`, `resolveProvider`, `parseProviderID`, `nameForCase`, `debounceKey`. Table-driven, no mocks.
- Controller integration: `envtest` (real apiserver + etcd, no kubelet). One reconciler per test, fake `Node` + canned NHD `status.diagnosis` written by the test, assert phase transitions + cordon patch + Slack mock invocations.
- Contract: golden-file tests over the Slack JSON payload and the `POST /diagnose` request body. Locks the cross-scope contract so a refactor can't silently break Scope 3.
- E2E: AC-1 through AC-10 from spec §8, run by hand on `test-odd-wire` (EKS) and one Azure kubeadm test cluster on Day 2.

**Target Platform**: Linux container (`linux/amd64` + `linux/arm64`) running on Kubernetes ≥1.28, deployed via Helm into namespace `container-fabric` on test-prefixed clusters only (Constitution Article I.9). Single Deployment, one replica. No leader election in v1 (NFR-2 marks it stretch).

**Project Type**: Kubernetes controller (single-binary Go service). Kubebuilder-style layout, but **lives in this repo** alongside the NPD fork (decision 2026-06-12). Module name stays `k8s.io/node-problem-detector`; controller code is namespaced under `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/...`, `config/nodemedic/`, `deployment/helm/nodemedic-controller/`, and `test/nodemedic/` so the existing NPD tree (`pkg/`, `cmd/nodeproblemdetector`, `cmd/healthchecker`, `cmd/logcounter`) is undisturbed. Build separation: NPD has `Dockerfile` + existing `Makefile` targets; controller has `Dockerfile.nodemedic-controller` + new `nodemedic-*` Makefile targets.

**Performance Goals** (from spec NFR-1):
- Condition `True` patch → NHD `Create` ≤ 5 s (informer ResyncPeriod stays at the controller-runtime default; predicate filters in-line so no requeue churn).
- NHD `Create` → `POST /diagnose` 202 ≤ 2 s (HTTP timeout 1.5 s; one retry on 429/5xx within budget).
- `phase=Diagnosed` observed → cordon executed ≤ 5 s (single `Patch` against apiserver).
- `phase=Diagnosed` observed → Slack visible ≤ 10 s (3 retries at 1/2/4 s = 7 s worst-case before final attempt).
- End-to-end NPD-signal-to-cordon: ≤ 35 s of controller-side overhead. Agent loop (~50 s) is the bulk of the user-visible latency; not a controller goal.

**Constraints**:
- **Read-only by default + cordon-only mutation** (Constitution Article I.1, I.2). Only `Node.spec.unschedulable` is mutated; nothing else, ever.
- **RBAC strictly limited per spec NFR-4**: `nhd` get/list/watch/create/update/patch (+ status); `nodes` get/list/watch/patch; `events` create/patch. Helm chart MUST refuse to install with broader role.
- **Provider-agnostic above the FR-2 boundary** (Constitution Article II.7). Cloud-specific code lives only in `parseProviderID` dispatch.
- **Cluster topology passed in, not discovered** (Constitution II.6). `--cluster-name` flag, no in-cluster guess.
- **Permission mode = `default`** for any tooling we run locally (Constitution Article I.6); the controller itself has no LLM tool surface.
- **No drain, no eviction, no pod deletion, no node termination, no automatic uncordon** (spec §3, Constitution I.2).

**Scale/Scope**:
- 1 controller instance per cluster.
- Node count per cluster: hundreds (test clusters; production scale is out of scope for v1).
- Concurrent in-flight NHDs: realistically 1–2 during demo, theoretically up to one per fault class per node — controller-runtime workqueue handles this.
- Cluster providers in v1: AWS EKS test cluster (e.g. `test-odd-wire`) + one Azure kubeadm test cluster (Constitution Article I.9, II.7).

**Clarifications resolved** (captain-confirmed 2026-06-12; full Decision/Rationale in [`research.md`](./research.md)):

- **CL-1 → manual `helm install`.** No Artifactory publishing during the hackathon. Ship the chart as a tarball in the controller repo at `deploy/helm/nodemedic-controller-0.1.0.tgz`; deploy via `helm --kube-context=test-... upgrade --install nodemedic-controller ./deploy/helm/nodemedic-controller -f values-<eks|azure>.yaml --set clusterName=test-...` from a laptop. (See [research R-3](./research.md#r-3-helm-chart-hosting-closes-spec-10-cl-1).)
- **CL-2 → chart installs CRD with `helm.sh/resource-policy: keep`** + `--set installCRD=false` toggle. README documents the one-line handshake with Scope 3. (See [research R-4](./research.md#r-4-crd-installation-in-the-chart-closes-spec-10-cl-2).)
- **CL-3 → agent treats duplicate `caseId` as idempotent.** Repeat POST → `202 {status: queued}` if in-flight, `202 {status: completed}` if already written, `409` tolerated and treated as 202. Locked in [`contracts/post-diagnose.md`](./contracts/post-diagnose.md). (See [research R-5](./research.md#r-5-idempotent-post-diagnose-closes-spec-10-cl-3).)
- **CL-4 → no Slack "View CR" button.** Slack message body includes a fenced markdown block with the exact `kubectl --context=<cluster> -n container-fabric get nhd <name> -o yaml` command. (See [research R-6](./research.md#r-6-slack-view-cr-link-target-closes-spec-10-cl-4).)
- **CL-5 → single replica, no leader election in v1.** Restart-window behavior is covered by FR-11; accepted. controller-runtime leader election can be turned on post-hackathon if we promote past test clusters. (See [research R-7](./research.md#r-7-multi-replica-leader-election-closes-spec-10-cl-5).)

No open clarifications remain. Phase 0 research is final; Phase 1 artifacts already reflect these decisions.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Article | Rule | How this plan satisfies it |
|---|---|---|
| I.1 Read-only by default | Every tool the agent can call is read-only; cordon executor is not an LLM tool | Controller has no LLM tool surface at all. Cordon executor (`internal/cordon`) is plain Go; invoked only by the reconciler after the gate passes. ✅ |
| I.2 Cordon-only | No drain, eviction, deletion, or termination | FR-8 patches `spec.unschedulable=true` and refuses any other field. No `Drain`/`Evict`/`Delete` calls anywhere in the codebase; CI grep test will fail the build if introduced. ✅ |
| I.3 Confidence gate is binding | `confidence ≥ 0.7`, ≥2 distinct evidence sources, action ∈ {Cordon, DrainAndCordon} | FR-6 implements this exactly. Pure function `confidenceGate(nhd) (Decision, reason)` with table-driven tests covering each clause. Thresholds are flags but the *structure* of the check is hard-coded. ✅ |
| I.4 Tool allow-list | Agent's `can_use_tool` rejects anything not in the allow-list | N/A for the controller (no LLM). Cross-scope: the controller never calls MCP tools (Article II.5). ✅ |
| I.5 SSH commands prefix-matched | `ssh_run` accepts only documented prefixes | N/A for the controller. Documented for the record. ✅ |
| I.6 Permission mode = `default` | Never `bypassPermissions` | N/A for the controller binary. For local dev tools (kubebuilder, controller-gen), we use them as documented. ✅ |
| I.7 Every tool call audited | `PreToolUse` hook writes JSONL audit | N/A for the controller. The agent owns this. The controller's `status.conditions[]` history is the equivalent audit trail for *its* decisions (NFR-7). ✅ |
| I.8 Hard caps on the loop | `max_turns` and `max_budget_usd` enforced | The controller writes `spec.budgets.maxTurns=15` and `maxBudgetUSD="0.50"` and a 60 s `deadline`. Enforcement is on the agent; the controller observes the deadline and triggers FR-7 retry/fail on overrun. ✅ |
| I.9 Test clusters only | Fault injection and demos run on `test-*` clusters | Helm chart's `values.yaml` ships `clusterName: ""` with no default; `templates/_helpers.tpl` emits a `helm install` failure if the resolved name doesn't start with `test-`. (Hard guard, not a comment.) ✅ |
| II.1 Two cross-scope contracts | NHD CRD + `POST /diagnose` only | Plan's `contracts/` directory holds exactly these two artifacts. No shared filesystem, no shared DB, no internal endpoint. ✅ |
| II.2 CRD owned by Scope 3 | Schema changes in same PR | We vendor the CRD types from Scope 3 (or generate from their published OpenAPI), pinned at `v1alpha1`. Local edits are forbidden; if Scope 3 publishes a schema change we re-generate as a single PR coordinated with Scope 3 captains. ✅ |
| II.3 Stub on Day 1 AM | Each scope ships a stub | Stub commitment: by Day 1 12:00, this controller `Create`s an NHD with hand-canned `status.diagnosis` (no agent call) and exercises gate + cordon + Slack. Lets Scope 1/3 unblock against a real CR. ✅ |
| II.4 Agent does not call controller | Agent writes CR; controller watches | One direction enforced by RBAC: agent has `create/get/update` on NHD; controller has read on agent's nothing — controller calls `POST /diagnose` outbound, agent never calls back. ✅ |
| II.5 Controller does not call MCP | Tool surface belongs to the agent | Controller binary has no MCP client, no LLM SDK. CI grep test (`! grep -rn 'anthropic\|mcp_servers\|claude_agent' internal/`) fails the build if introduced. ✅ |
| II.6 Cluster topology passed in | Controller resolves provider/region/instanceId from Node, writes into spec.case | FR-2 + Day-1-AM fixture-driven unit tests for both EKS and Azure providerID formats. The agent receives a populated `provider`/`region`/`instanceId` and dispatches off it; never infers. ✅ |
| II.7 One binary, two clouds | Same agent process for AWS + Azure | Same controller binary too: `parseProviderID` switches on URI scheme; everything else is provider-agnostic. Helm values switch only `--cluster-name`, kubeconfig context, and Slack webhook. ✅ |
| III.1 Cite, don't claim | Verifiable references | Every FR in the spec links to file:line / scope.md section. Plan and research will follow the same standard. ✅ |
| III.2 Stub before integrate | Day 1 AM stubs mandatory | See II.3 above. End-to-end on real components is Day 1 PM. ✅ |
| III.3 Out of scope stays out | No eval, drain, multi-region, etc. | Reflected in FR-10 (no auto-uncordon), spec §3 (no PagerDuty in v1, no eval), Helm chart drops PagerDuty notifier from §5.4. ✅ |
| III.4 Demo flow drives priorities | Conntrack on EKS first, then Azure | Tasks are sequenced this way; AC-1..7 on EKS, AC-8 (Azure parity) gated after. ✅ |
| III.5 Ask before pivoting | Surface blockers, don't silently rearchitect | NEEDS CLARIFICATION items above are the surface. Anything new gets raised to captains, not coded around. ✅ |

**Gate result: PASS.** No violations. Complexity Tracking section below stays empty.

## Project Structure

### Documentation (this feature)

```text
.specify/specs/001-nodemedic-controller/
├── plan.md              # this file
├── spec.md              # feature spec (already authored)
├── research.md          # Phase 0 output (this run)
├── data-model.md        # Phase 1 output (this run)
├── quickstart.md        # Phase 1 output (this run)
├── contracts/
│   ├── nhd-crd.yaml             # vendored from Scope 3 — frozen v1alpha1 schema
│   └── post-diagnose.md         # reference + golden request body
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (in this repo — NPD-fork tree, controller code namespaced)

Kubebuilder-style layout adjusted to coexist with NPD. Existing NPD paths (`cmd/nodeproblemdetector/`, `cmd/healthchecker/`, `cmd/logcounter/`, `pkg/...`, `config/{plugin,exporter,...}/`, `deployment/{cf1z,...}/`, `test/{e2e,kernel_log_generator}/`) are untouched.

```text
node-medic-agent/                    # this repo (module: k8s.io/node-problem-detector)
├── cmd/
│   ├── nodeproblemdetector/         # existing NPD command (untouched)
│   ├── healthchecker/               # existing (untouched)
│   ├── logcounter/                  # existing (untouched)
│   └── nodemedic-controller/        # NEW
│       └── main.go                  # flag parsing, manager.New, signal handling
├── api/                             # NEW
│   └── v1alpha1/
│       ├── groupversion_info.go
│       ├── nodehealthdiagnosisai_types.go
│       └── zz_generated.deepcopy.go
├── internal/                        # NEW (NPD itself uses pkg/, not internal/)
│   └── nodemedic/
│       ├── controller/
│       │   ├── nodewatcher.go       # FR-1 informer A
│       │   ├── case_creator.go      # FR-2/FR-3
│       │   ├── nhd_reconciler.go    # FR-5 phase machine
│       │   ├── confidence_gate.go   # FR-6 pure function
│       │   ├── cordon.go            # FR-8
│       │   ├── debounce.go          # FR-1
│       │   ├── nameformat.go
│       │   └── types.go             # Case, Decisions, GateResult
│       ├── agentclient/
│       │   ├── client.go            # POST /diagnose
│       │   └── types.go             # request/response structs
│       ├── notifier/
│       │   ├── slack.go             # FR-9 Block Kit via stdlib http
│       │   └── messages.go          # Applied / HumanInLoop / Critical builders
│       ├── metrics/
│       │   └── metrics.go           # NFR-3 collectors
│       └── providerid/
│           ├── parse.go             # FR-2 AWS + Azure URI parsers
│           └── parse_test.go        # fixture-driven
├── config/
│   ├── plugin/                      # existing NPD plugin configs (untouched)
│   ├── exporter/                    # existing (untouched)
│   ├── ...                          # existing (untouched)
│   └── nodemedic/                   # NEW
│       ├── crd/                     # generated by controller-gen
│       └── rbac/                    # role.yaml + binding (NFR-4 minimal)
├── deployment/
│   ├── cf1z/                        # existing CF NPD deploy (untouched)
│   ├── node-problem-detector*.yaml  # existing (untouched)
│   ├── rbac.yaml                    # existing NPD rbac (untouched)
│   └── helm/                        # NEW
│       └── nodemedic-controller/
│           ├── Chart.yaml
│           ├── values.yaml          # default flags
│           ├── values-eks.yaml      # test-odd-wire override
│           ├── values-azure.yaml    # azure kubeadm override
│           └── templates/
│               ├── deployment.yaml
│               ├── serviceaccount.yaml
│               ├── clusterrole.yaml
│               ├── clusterrolebinding.yaml
│               ├── crd.yaml         # gated by .Values.installCRD
│               └── _helpers.tpl     # cluster-name guard (Constitution I.9)
├── test/
│   ├── e2e/                         # existing NPD e2e (untouched)
│   ├── kernel_log_generator/        # existing (untouched)
│   └── nodemedic/                   # NEW
│       ├── envtest/
│       │   └── reconciler_test.go
│       └── fixtures/
│           ├── node-eks.yaml        # captured Day 1 AM
│           └── node-azure.yaml      # captured Day 1 AM
├── hack/
│   └── boilerplate.go.txt           # already present per existing NPD; re-used
├── Dockerfile                       # existing NPD image (untouched)
├── Dockerfile.nodemedic-controller  # NEW — distroless static, multi-arch
├── Makefile                         # extended with `nodemedic-*` targets; NPD targets unchanged
├── go.mod                           # module: k8s.io/node-problem-detector (existing)
├── go.sum
├── vendor/                          # existing; will grow by ~controller-runtime + deps in Phase 2
└── .specify/specs/001-nodemedic-controller/   # spec-kit artifacts (this plan)
```

**Structure Decision**: Same Go module as NPD (`k8s.io/node-problem-detector`); imports inside the controller use `k8s.io/node-problem-detector/api/v1alpha1`, `k8s.io/node-problem-detector/internal/nodemedic/...`, etc. One Deployment, one Helm chart, one binary. Internals split by concern (`controller`, `agentclient`, `notifier`, `metrics`, `providerid`) under `internal/nodemedic/` so the reconciler stays narrative. No webhooks, no admission, no leader election in v1. CRD types live in `api/v1alpha1/` and are generated by `controller-gen`; the schema is owned by Scope 3 (Constitution II.2) and we re-generate to match contracts/nhd-crd.yaml.

**Build isolation**: Controller has its own Dockerfile (`Dockerfile.nodemedic-controller`) and its own Makefile target prefix (`make nodemedic-*`). NPD's existing `Dockerfile` and existing Make targets are untouched. CI gets a separate `.github/workflows/nodemedic-ci.yml` workflow that triggers only on controller path changes; existing NPD CI workflows are untouched.

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

(No violations. Section intentionally left empty.)
