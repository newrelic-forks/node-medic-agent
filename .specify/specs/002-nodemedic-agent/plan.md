# Implementation Plan: NodeMedic Agent

**Branch**: `hackathon-2026/cf1z-baseline` (agent code lands directly on this branch — no separate `scope3-agent` branch is cut, mirroring how Spec 001 ended up coexisting with NPD here; the controller landed via PR #2 onto the same branch and the agent will follow the same pattern. If parallel work surfaces, a `hackathon-2026/scope3-agent` branch can be cut from `cf1z-baseline` without a plan revision.) | **Date**: 2026-06-13 | **Spec**: [`spec.md`](./spec.md)

**Input**: Feature specification at `.specify/specs/002-nodemedic-agent/spec.md`

**Constitution**: [`.specify/memory/constitution.md`](../../memory/constitution.md) (single living doc; hackathon-scope simplifications already integrated into this plan)

## Summary

NodeMedic Agent is a single-replica FastAPI service that receives `POST /diagnose` from the NodeMedic Controller (Spec 001), runs an isolated Claude Agent SDK loop per case against the internal nerd-completion gateway, gathers evidence via raw `Bash` (kubectl, aws, az, ssh, /proc) plus the public New Relic HTTP MCP, and writes the diagnosis to `status.diagnosis` of the existing `NodeHealthDiagnosisAI` (NHD) CR. The agent never creates the CR, never cordons a Node, and has no agent-side termination ceiling — its credentials and the controller's gate (Constitution Article I.3) are the safety boundary, not in-process budgets.

**Approach (from research):** Python 3.12 + FastAPI/uvicorn + Anthropic's `claude-agent-sdk` (Python) on the model side; the official `kubernetes` Python client for NHD `Status().Update`s; `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN` env-var protocol points the SDK at nerd-completion (no SDK code change needed); `emit_report` registered as an in-process `@tool` so the runner intercepts it without going through any backend; runbook baked into the image at `/app/prompts/runbook.md`; structured stdout JSON logs are the only audit trail (constitution defers durable JSONL); single `nodemedic-agent` Deployment + ClusterIP `Service` in `cf-monitoring`, mirroring the controller's chart shape.

## Technical Context

**Language/Version**: Python 3.12.x. Anthropic's `claude-agent-sdk` Python package targets Python 3.10+; pinning 3.12 keeps us aligned with the public CPython release that bundles `tomllib`, has `asyncio.TaskGroup`, and has the longest support window relative to the hackathon timeframe. Same major as `nova/k8s-agent-claude-sdk`.

**Primary Dependencies**:
- `claude-agent-sdk` (PyPI) — agent runtime, hooks, tool registration, MCP wiring. Provides `ClaudeSDKClient`, `ClaudeAgentOptions`, `@tool` decorator, `PreToolUse` hook surface. Calls Anthropic over HTTP and respects `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` env vars.
- `fastapi` + `uvicorn[standard]` — async HTTP server for `POST /diagnose`, `GET /healthz`, `GET /readyz`. uvicorn's `--workers 1` keeps the FastAPI app process single per Kubernetes pod (Constitution Article I.5; multi-replica is a stretch goal).
- `kubernetes` (official client) — in-cluster config, NHD `Status().Update` via the dynamic client (no generated CRD types; NHD is read/written through `client.CustomObjectsApi`). The dynamic path keeps us off `kopf` and other operator frameworks the agent does not need.
- `pydantic` v2 — request/response validation (`POST /diagnose` body, `emit_report` payload), config loading. Comes in as a transitive of FastAPI; pinned explicitly so we own the version.
- `httpx` — HTTP client used for the NR MCP server health probe (FR-2 `/readyz`) and any out-of-band probing the runner performs. The Claude Agent SDK has its own internal client; we do not share connections with it.
- `structlog` — structured stdout JSON logging (NFR-3, the only audit trail per the constitution's hackathon-scope simplifications). Each log line carries `caseId`, `tool`, `command-or-args`, `latency_ms`, etc.
- `prometheus-client` — optional `/metrics` exporter (FR-2 marks this OPTIONAL). Wired only if it falls out cheaply through `prometheus-fastapi-instrumentator`; absence of `/metrics` is not a defect.
- Standard utilities pinned in the image (not Python deps): `kubectl` 1.33.x (matches cf1z's k8s 1.33.8), `aws` CLI v2.x, `az` CLI 2.x, `jq`, `curl`, `dig`, `nslookup`, `ss`, OpenSSH client.

**Storage**: N/A. Per-case state lives in an in-memory case table keyed by `caseId` (FR-10) for the lifetime of the pod. The CR (`status.diagnosis`) is the durable record of every successful case; structured stdout logs (NFR-3) are the ephemeral audit trail. No PVC, no DB, no Anthropic Files API. (Production restoration adds back a per-case JSONL on PVC per the constitution's "Production hardening" item 7.)

**Testing**:
- Unit: `pytest` + `pytest-asyncio`. Pure-function targets — `validate_emit_report_payload`, `build_user_prompt(case)`, `resolve_model_at_startup(catalog, requested, fallback)`, `should_overwrite(observed_phase)` (FR-12 phase-conflict guard). Table-driven, no mocks.
- HTTP integration: FastAPI's `TestClient` against the assembled app. Hits `POST /diagnose` with golden bodies (the same fixtures locked into the controller's `internal/nodemedic/agentclient/` golden tests), exercises 202 / 400 / 429 paths, idempotent re-POST, and the FR-1 "no auth header required" relaxation. The Claude Agent SDK is replaced with a fake worker (`async def fake_run_case(case): ...`) so tests don't talk to the gateway.
- Tool-surface contract: golden-file test on the JSON shape emitted by `emit_report` so the controller's gate code can be regression-tested against the same payload. Mirrors how the controller golden-tests its `POST /diagnose` body.
- Runbook lint: `tests/check_runbook.sh` (a bash script — small enough that pytest is overkill) verifies AC-15's seven required runbook clauses by `grep`-pattern matching against `prompts/runbook.md`. Run by CI and by the Helm pre-install Job.
- E2E: AC-1..AC-15 from spec §8, run by hand on cf1z (`hackathon-2026/cf1z-baseline`). The deployed `chaos-containerd-unhealthy` and `chaos-kubelet-unhealthy` cronjobs are the fault injectors. No EKS test target on Day 1 (cf1z is the only available demo cluster — see spec AC-4).

**Target Platform**: Linux container, `linux/amd64` only for Day 1 (cf1z worker nodes are amd64). The Dockerfile uses `--platform=$BUILDPLATFORM` on the Python build stage and the CLI-bundle stage so Apple Silicon dev hosts cross-compile cleanly via Colima; the runtime stage stays `linux/amd64` and no arm64 runtime image is published. Deployed as a single Kubernetes Deployment + ClusterIP Service in namespace `cf-monitoring` on cf1z (Azure kubeadm, k8s 1.33.8), per Constitution Article I.5.

**Project Type**: Long-lived async HTTP service (FastAPI + Claude Agent SDK case workers). Single-binary container. Coexists with the existing NPD tree and the Spec 001 controller in the same repo: agent code lands under `cmd/nodemedic-agent/` (entry point), `nodemedic_agent/` (Python package, at repo root — see Structure Decision below for why not `internal/`), `prompts/` (runbook), `deployment/helm/nodemedic-agent/` (chart), `tests/nodemedic_agent/`. NPD's existing layout (including its Go `test/` directory, sibling to the new Python `tests/` directory), the controller's `cmd/nodemedic-controller/` / `api/v1alpha1/` / `internal/nodemedic/...` tree, and all controller Make targets are untouched.

**Performance Goals** (from spec NFR-1):
- `POST /diagnose` receipt → `202 Accepted` returned: ≤ 1.5 s p99. **Hard ceiling, sourced from the controller's per-attempt HTTP timeout** (`internal/nodemedic/agentclient/client.go` per-attempt `Timeout: 1500 * time.Millisecond` on `hackathon-2026/scope2-controller`). Slower responses surface to the controller as `ResultTimeout` and trigger up to 4 retries on the 1s/2s/4s backoff schedule. The enqueue path (case-table lookup + asyncio task spawn) MUST sit comfortably inside this — soft target ≤ 50 ms p99.
- Loop start → `emit_report` (diagnosis duration): no agent-side ceiling per FR-7. Soft target ~30–50 s on cf1z's `ContainerRuntimeUnhealthy` demo path. The controller's first-attempt deadline is ~60 s (Spec 001 FR-5) and the controller has a one-shot deadline-extension retry of ~60 s, giving the agent up to ~120 s of "the controller is still listening" runway before FR-12 starts suppressing late writes.
- CR write after `emit_report`: ≤ 2 s typical (one re-read + one `Status().Update`).

**Constraints**:
- **Credential-layer least privilege is the primary safety boundary** (Constitution Article I.1). Read-only AWS IRSA (or absent), read-only Azure SP (or absent), read-only NR token, kube ServiceAccount with `get/list/watch` on `nodes`/`pods`/`events` plus `get/list/watch/update` on NHD (`update` includes the `status` subresource per FR-14). Helm chart MUST refuse to render a wider role.
- **Non-production clusters only** (Constitution Article I.5). cf1z and other legacy CF dev clusters (`jc1z`, `sk1z`) plus `test-*` clusters are the permitted surface. Helm chart's cluster-name guard rejects anything outside that allowlist (the controller's chart already enforces this on `cf1z` + `test-*`; the agent chart copies the helper).
- **Cordon-only is the controller's job, not the agent's** (Constitution Article I.2). The agent has NO `nodes/patch` verb in its kube role. Cross-checked by chart review per FR-13's hackathon-scope deferral.
- **Bash with no allow-list** (Constitution hackathon-scope simplification). The agent calls `Bash` directly; the runbook prompt steers usage and the credential layer bounds blast radius. Production restoration: re-add the structured tool allow-list per Constitution "Production hardening" items 1–3.
- **No agent-side termination ceiling** (Constitution hackathon-scope simplification + FR-7). The loop ends only on `emit_report` or model halt. The controller's deadline is the user-visible bound; FR-12 governs the late-write race.
- **Two cross-scope contracts only** (Constitution Article II.1): the NHD CRD (read `spec.*`, write `status.diagnosis.*` + `status.phase` Diagnosing→Diagnosed/…→Failed) and `POST /diagnose` (incoming). No shared filesystem, no shared DB, no other endpoint.
- **Same image, two clouds** (Constitution Article II.7). Per-cluster Helm install picks which cloud's credentials are mounted; the agent code is provider-agnostic and dispatches via the runbook prompt off `case.provider`.
- **Image registry path locked**: `cf-registry.nr-ops.net/container-fabric/nodemedic-agent:dev-cf1z-<short-sha>`, mirroring the controller's path. Verified empirically 2026-06-14 against the controller image push (developer cf-registry creds work — no service account required).
- **Runbook is part of the image**, not a runtime ConfigMap (per Clarifications). Image SHA pins the runbook version. The runbook lint script (AC-15) is the build-time gate.

**Scale/Scope**:
- 1 agent instance per cluster.
- `MAX_CONCURRENT_CASES=32` ceiling. Typical demo load: 1 concurrent case. Peak rehearsal: 10 concurrent cases (NFR-8). 22-case headroom is safety margin, not a target.
- One target cluster on Day 1: cf1z (Azure kubeadm, 1 control plane + 6 general nodes, k8s 1.33.8). Canary node `cf1z-general-nodes-2000002` is the chaos target.
- Two demo NodeConditions on Day 1: `ContainerRuntimeUnhealthy` (primary, AC-3) and `KubeletUnhealthy` (secondary, AC-3b). Both have rehearsable chaos cronjobs deployed and verified end-to-end on cf1z 2026-06-14.

**Clarifications resolved** (in spec §Clarifications + this plan's research):

All 10 clarifications from spec.md's `## Clarifications` section are bound. Plan-phase technology decisions are resolved in [`research.md`](./research.md):

- **PR-1 → Anthropic SDK choice**: `claude-agent-sdk` (Python). Same package `nova/k8s-agent-claude-sdk` uses. Closes the spec's "exact SDK invocation TBD in plan" hedge for `MODEL_CONTEXT_VARIANT`.
- **PR-2 → Web framework**: FastAPI + uvicorn. Async-native, drops in with the Claude Agent SDK's `asyncio` model.
- **PR-3 → Kubernetes client**: official `kubernetes` Python client + `CustomObjectsApi`, no generated types. Avoids dragging in an operator framework for what is effectively two API calls (`get` + `replace_namespaced_custom_object_status`).
- **PR-4 → Concurrency model**: one `asyncio.Task` per case, capped by an `asyncio.Semaphore(MAX_CONCURRENT_CASES)`. The `POST /diagnose` handler itself stays fast — it does the case-table lookup, spawns the task, and returns 202.
- **PR-5 → emit_report registration**: in-process `@tool`-decorated function via `claude-agent-sdk`'s tool-registration API. The decorator wraps a closure that closes over the runner's `Case` object so the runner intercepts the call before any network egress.
- **PR-6 → NR MCP wiring**: configured as `mcp_servers={"nr": <HTTP MCP config>}` on `ClaudeAgentOptions`. The NR token is passed in the MCP config; the runbook prompt instructs the agent to set `account_id=1` on every NRQL.
- **PR-7 → Runbook lint**: `tests/check_runbook.sh` — bash + grep, run by CI and by a Helm pre-install Job that mounts `prompts/runbook.md` and exits non-zero if any of the seven AC-15 clauses is missing.
- **PR-8 → Helm chart shape**: mirror the controller's `deployment/helm/nodemedic-controller/` layout exactly. Same `_helpers.tpl` cluster-name guard, same `Chart.yaml` shape, parallel `values-azure.yaml` (cf1z) + `values-eks.yaml` (future test cluster).
- **PR-9 → CI workflow**: new `.github/workflows/nodemedic-agent-ci.yml` parallel to the controller's `nodemedic-ci.yml`. Path-filter only triggers on agent paths; runs `pytest`, `helm lint`, `tests/check_runbook.sh`. Image build/push is manual for the hackathon (Colima → cf-registry), matching how the controller image lands today.
- **PR-10 → Image build**: `Dockerfile.nodemedic-agent` at repo root (parallel to `Dockerfile.nodemedic-controller`). Multi-stage build with `--platform=$BUILDPLATFORM` on Python build stage, runtime stage stays `linux/amd64` (cf1z is amd64-only).

No open clarifications remain. Phase 0 research is final; Phase 1 artifacts reflect these decisions.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The agent spec already integrates the constitution's hackathon-scope simplifications (spec §1 footer, §3 non-goals, FR-1/FR-3/FR-4/FR-5/FR-7/FR-9/FR-13). This table cross-checks the plan-phase technology choices against each Article and notes the few items deliberately deferred per the constitution.

| Article | Rule | How this plan satisfies it |
|---|---|---|
| I.1 Credential-layer least privilege | Every credential mounted to the agent pod scoped read-only | Helm chart renders kube `ClusterRole` with `get/list/watch` on nodes/pods/events + `get/list/watch/update` on NHD only (FR-14). IRSA policy on AWS install scoped to `ec2:Describe*` + `health:DescribeEvents`; Azure SP scoped to `Reader` on the cluster RG; NR token scoped to staging account `1`. Verified by captain chart review (FR-13 deferred for hackathon scope per constitution). ✅ |
| I.2 Cordon-only | No drain, eviction, deletion, termination, cordon — that's the controller's job | Agent kube role explicitly has NO `nodes/patch` verb. Confirmed by FR-14 + spec §3 non-goals. CI grep test (`! grep -rn "nodes/patch\\|kubectl cordon\\|kubectl drain" deployment/helm/nodemedic-agent/templates/`) fails the build if introduced. ✅ |
| I.3 Confidence gate is binding | `confidence ≥ 0.7`, ≥2 distinct sources, action ∈ {Cordon, DrainAndCordon} | Enforced **only** on the controller side per spec G5 + FR-8 step 2. The agent does NOT pre-reject low-evidence reports. Single-source reports are accepted (AC-6). The runbook prompt (AC-15 clauses 2 + 3) instructs the agent to lower confidence and choose `NoAction` when evidence is thin so the controller's gate routes to HumanInLoop. ✅ |
| I.4 Every tool call observable | `PreToolUse` hook emits structured-log line per call | Wired in FR-3 via `hooks=[("PreToolUse", tool_log_hook)]` on `ClaudeAgentOptions`. NFR-3 specifies the JSON shape. Stdout-only — durable JSONL on PVC is deferred per constitution "Production hardening" item 7. ✅ |
| I.5 Non-production clusters only | Fault injection + agent deployment on `test-*` / `cf1z` / `jc1z` / `sk1z` only | Helm chart's `_helpers.tpl` `requireTestCluster` accepts `cf1z` (literal), `jc1z` (literal), `sk1z` (literal), or any `test-*` prefix; rejects anything else (matches the controller chart's guard, with `jc1z`/`sk1z` added per the agent's spec §G10 surface). NR token scoped to staging account `1`. ✅ |
| I (hackathon simp.) No structured tool allow-list | Agent calls `Bash` directly | `ClaudeAgentOptions` enables `Bash` with no `can_use_tool` callback. Constitution-bound; tracked as deferred production-hardening item 1. ✅ |
| I (hackathon simp.) No SSH prefix-match | Free-form ssh against worker nodes | Runbook prompt instructs the agent on SSH usage (FR-16). No code-level prefix check. Deferred production-hardening item 2. ✅ |
| I (hackathon simp.) `permission_mode = "bypassPermissions"` | Loop not interrupted by per-tool prompts | Set on `ClaudeAgentOptions` per FR-3. Deferred production-hardening item 3. ✅ |
| I (hackathon simp.) No agent-side termination ceiling | No max_turns / max_budget / wall-clock cap | Loop runs until `emit_report` or model halts (FR-7). Controller's ~60–120 s deadline window is the user-visible bound; FR-12 governs the late-write race. Deferred production-hardening items 5–6. ✅ |
| I (hackathon simp.) Ephemeral audit log | Stdout structured logs only, no PVC JSONL | NFR-3 + NFR-7 explicit about this. `emptyDir` on the pod for any `Write`/`Edit` scratch. Deferred production-hardening item 7. ✅ |
| II.1 Two cross-scope contracts | NHD CRD + `POST /diagnose` only | The two cross-scope contracts in this plan's [`contracts/`](./contracts/) directory are `nhd-crd.yaml` (vendored copy of Spec 001's frozen v1alpha1 schema) and `post-diagnose.md` (agent-side receive contract). The third file, `emit-report-tool.md`, documents an **in-process** tool registered with the SDK — the controller never sees it directly, only the resulting `status.diagnosis` on the CR — so it is not a cross-scope channel. No shared FS, no shared DB, no other endpoint. ✅ |
| II.2 CRD owned by Scope 3 | Agent owns the schema | Schema authority lives in the Spec 001 `api/v1alpha1/nodehealthdiagnosisai_types.go` (Go) and the controller's `contracts/nhd-crd.yaml` (OpenAPI). For the hackathon shape both scopes live in the same repo, so "owned by Scope 3" is enforced by PR review — schema PRs are filed against `api/v1alpha1/` regardless of which scope drives them, and the agent + controller move in lockstep. The agent's runtime path uses `CustomObjectsApi` directly (no generated Python types) so a schema bump only needs the agent's `emit_report` payload validator updated. ✅ |
| II.3 Stub on Day 1 AM | Each scope ships a stub | Day 1 stub for the agent: `POST /diagnose` returns 202 immediately and a stub worker writes a hand-canned `status.diagnosis` with `confidence=0.85`, two evidence entries, `recommendation.action=Cordon` after a 30-second sleep. Lets the controller exercise its gate + cordon + Slack against a real CR before the SDK loop is wired. The controller already runs against this shape via `--stub-agent=true` on cf1z. ✅ |
| II.4 Agent does not call controller | One direction enforced by RBAC | Agent has no kube verbs that would let it call the controller (it only writes the NHD's `status` subresource); there is no controller HTTP endpoint to call. ✅ |
| II.5 Controller does not call MCP | Tool surface belongs to the agent | N/A for this plan, but cross-checked: the controller chart has no Anthropic / MCP env vars (verified at `deployment/helm/nodemedic-controller/templates/deployment.yaml`). ✅ |
| II.6 Cluster topology passed in | Controller writes `provider`/`region`/`instanceId` into `spec.case` | The agent reads `case.provider`/`case.region`/`case.instanceId` straight from the request body and the CR; it never queries `Node.spec.providerID`. ✅ |
| II.7 One binary, two clouds | Same image, AWS + Azure cases | One `Dockerfile.nodemedic-agent` produces one image. Per-cluster Helm `values-{azure,eks}.yaml` mounts only that cloud's credentials. Cross-cloud probing fails at the credential layer (AC-13). ✅ |
| III.1 Cite, don't claim | Verifiable references | This plan, research.md, data-model.md, contracts/, quickstart.md all link to file:line / spec section / scope.md anchors. ✅ |
| III.2 Stub before integrate | Day 1 stubs mandatory | See II.3. End-to-end on real components is Day 1 PM. ✅ |
| III.3 Out of scope stays out | No eval, drain, multi-region, etc. | Spec §3 non-goals list is comprehensive and binding. The plan introduces no new scope. ✅ |
| III.4 Demo flow drives priorities | cf1z `ContainerRuntimeUnhealthy` first, then `KubeletUnhealthy` | Tasks in Phase 2 will sequence AC-1..AC-3 (containerd path) before AC-3b (kubelet path) before AC-13/14. EKS parity is post-hackathon. ✅ |
| III.5 Ask before pivoting | Surface blockers, don't silently rearchitect | Open clarifications were resolved through three /speckit-clarify passes before /speckit-plan ran. Any new blocker discovered in Phase 2 goes back to a captain. ✅ |

**Gate result: PASS.** No violations beyond the constitution-permitted hackathon-scope simplifications, all of which are explicitly bound by the constitution itself. Complexity Tracking section below is empty.

## Project Structure

### Documentation (this feature)

```text
.specify/specs/002-nodemedic-agent/
├── plan.md              # this file
├── spec.md              # feature spec (already authored, plan-ready)
├── research.md          # Phase 0 output (this run)
├── data-model.md        # Phase 1 output (this run)
├── quickstart.md        # Phase 1 output (this run)
├── contracts/
│   ├── post-diagnose.md         # agent-side receive contract — golden 202/400/429 bodies
│   ├── emit-report-tool.md      # in-process terminal-tool schema
│   └── nhd-crd.yaml             # vendored copy of Spec 001's CRD (symlink-equivalent — cross-scope authoritative)
├── checklists/                  # already populated by /speckit-checklist
├── manifests/                   # per-cluster Secret + IRSA manifests (already authored)
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (in this repo — coexists with NPD + the Spec 001 controller)

The agent lives alongside the existing NPD tree (`pkg/`, `cmd/nodeproblemdetector`, etc.) and the Spec 001 controller (`cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/`, `deployment/helm/nodemedic-controller/`). All existing paths are untouched.

```text
node-medic-agent/                       # this repo (Go module: k8s.io/node-problem-detector)
├── cmd/
│   ├── nodeproblemdetector/            # existing NPD command (untouched)
│   ├── healthchecker/                  # existing (untouched)
│   ├── logcounter/                     # existing (untouched)
│   ├── nodemedic-controller/           # Spec 001 controller (untouched)
│   └── nodemedic-agent/                # NEW — Python entrypoint (a single .py module that
│       └── main.py                     #     imports nodemedic_agent.app and runs uvicorn)
├── api/                                # Spec 001 CRD types (untouched — schema authority)
│   └── v1alpha1/
│       └── nodehealthdiagnosisai_types.go
├── internal/                           # Spec 001 controller (untouched)
│   └── nodemedic/...
├── nodemedic_agent/                    # NEW — Python package (PEP 8 underscore name)
│   ├── __init__.py
│   ├── app.py                          # FastAPI app factory; wires routes, health, lifespan
│   ├── config.py                       # Pydantic Settings — env var schema (NFR-6)
│   ├── api/
│   │   ├── __init__.py
│   │   ├── diagnose.py                 # POST /diagnose handler (FR-1, FR-10)
│   │   └── health.py                   # GET /healthz, GET /readyz (FR-2)
│   ├── runner/
│   │   ├── __init__.py
│   │   ├── case_worker.py              # async worker per case — owns the SDK loop (FR-3)
│   │   ├── case_table.py               # in-memory case table, idempotency (FR-10)
│   │   ├── model_resolver.py           # startup model resolution (FR-3 fallback chain)
│   │   ├── prompt.py                   # load_runbook() + build_user_prompt(case)
│   │   └── hooks.py                    # PreToolUse hook → structured stdout log (NFR-3)
│   ├── tools/
│   │   ├── __init__.py
│   │   └── emit_report.py              # @tool emit_report; schema validation; CR write (FR-8)
│   ├── kube/
│   │   ├── __init__.py
│   │   ├── client.py                   # in-cluster config + CustomObjectsApi factory (FR-14)
│   │   └── nhd_writer.py               # Status().Update with FR-12 phase guard + FR-8 retry policy
│   ├── logging.py                      # structlog config (NFR-3)
│   └── metrics.py                      # OPTIONAL prometheus_client wiring (FR-2)
├── prompts/                            # NEW — runbook source-of-truth, baked into image
│   └── runbook.md                      # the seven AC-15 clauses live here
├── deployment/
│   ├── cf1z/                           # existing CF NPD deploy (untouched)
│   ├── helm/
│   │   ├── nodemedic-controller/       # Spec 001 chart (untouched)
│   │   └── nodemedic-agent/            # NEW — mirrors controller chart shape exactly
│   │       ├── Chart.yaml
│   │       ├── values.yaml             # defaults (cluster-name unset; image tag empty)
│   │       ├── values-azure.yaml       # cf1z install
│   │       ├── values-eks.yaml         # future test-* install (placeholder)
│   │       └── templates/
│   │           ├── _helpers.tpl        # requireTestCluster (cf1z + jc1z + sk1z + test-*)
│   │           ├── deployment.yaml     # NodeMedic agent Deployment
│   │           ├── service.yaml        # ClusterIP :8080 → :8080
│   │           ├── serviceaccount.yaml # with IRSA annotation when .Values.cloud.provider=aws
│   │           ├── clusterrole.yaml    # FR-14 minimal verbs
│   │           ├── clusterrolebinding.yaml
│   │           └── runbook-lint-job.yaml  # Helm pre-install Job runs check_runbook.sh
│   └── ...                             # existing NPD deploy files (untouched)
├── tests/                              # NEW — Python tests live alongside the package
│   └── nodemedic_agent/
│       ├── unit/
│       │   ├── test_emit_report_validate.py
│       │   ├── test_build_user_prompt.py
│       │   ├── test_model_resolver.py
│       │   └── test_should_overwrite.py     # FR-12 phase-guard truth table
│       ├── integration/
│       │   ├── test_post_diagnose.py        # FastAPI TestClient, golden bodies, 202/400/429
│       │   ├── test_idempotent.py           # FR-10 re-POST same caseId
│       │   └── test_health_ready.py         # FR-2 startup probe outcomes
│       ├── fixtures/
│       │   ├── case_aws.json                # golden POST /diagnose body (AWS)
│       │   ├── case_azure.json              # golden POST /diagnose body (Azure cf1z)
│       │   └── emit_report_payload.json     # golden emit_report tool payload
│       └── check_runbook.sh                 # AC-15 lint script (bash + grep)
├── pyproject.toml                      # NEW — Python deps + pytest config
├── poetry.lock or uv.lock              # NEW — lockfile (uv preferred per CLAUDE.md "Python")
├── Dockerfile                          # existing NPD image (untouched)
├── Dockerfile.nodemedic-controller     # Spec 001 image (untouched)
├── Dockerfile.nodemedic-agent          # NEW — multi-stage Python + CLI bundle, linux/amd64
├── Makefile                            # extended with `nodemedic-agent-*` targets;
│                                       #   NPD + nodemedic-controller targets unchanged
├── go.mod / go.sum / vendor/           # untouched (controller stays Go)
├── .github/workflows/
│   ├── nodemedic-ci.yml                # Spec 001 controller CI (untouched)
│   ├── nodemedic-agent-ci.yml          # NEW — pytest + helm lint + check_runbook.sh
│   └── ...                             # existing NPD workflows (untouched)
└── .specify/specs/002-nodemedic-agent/ # spec-kit artifacts (this plan)
```

**Structure Decision**: Python package at the **repo root** (`nodemedic_agent/`) rather than nested under `internal/` — Go's `internal/` convention does not apply to Python and a sibling tree keeps `pyproject.toml`'s package discovery simple (`packages = ["nodemedic_agent"]`). The Go module (`k8s.io/node-problem-detector`) and the Python package coexist at the repo root without import collisions; Go's package detection looks for `*.go` and Python's looks for `__init__.py`. The Python tests live under `tests/nodemedic_agent/` (not `tests/` directly) so the existing NPD `test/` Go tree is untouched and pytest only picks up agent tests by collection-root configuration in `pyproject.toml`.

The Helm chart shape mirrors the controller's exactly — same `_helpers.tpl` cluster-name guard pattern (extended to accept `jc1z`/`sk1z` per spec §G10's permitted surface), same per-cloud `values-{azure,eks}.yaml` split, same `Chart.yaml` minimum structure. A reviewer who has read the controller chart can read the agent chart.

The runbook lives at `prompts/runbook.md` at the repo root (not under the Python package) so it can be COPY'd into the image at `/app/prompts/runbook.md` without traversing Python package paths. The lint script `tests/nodemedic_agent/check_runbook.sh` greps that file for the seven AC-15 clauses; the Helm pre-install Job in `runbook-lint-job.yaml` mounts the same file from the rendered image and runs the same script — single source of truth.

**Build isolation**: Agent has its own Dockerfile (`Dockerfile.nodemedic-agent`) and Make target prefix (`make nodemedic-agent-*`). The controller's `Dockerfile.nodemedic-controller` and `make nodemedic-*` targets stay untouched. CI gets a separate `.github/workflows/nodemedic-agent-ci.yml` workflow that triggers only on agent paths.

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

(No violations beyond the constitution-permitted hackathon-scope simplifications, which the constitution itself binds. Section intentionally left empty.)
