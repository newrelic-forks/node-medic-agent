# Phase 0 Research — NodeMedic Agent

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-13

This document closes every plan-phase decision and pins the dependency / pattern choices the plan calls "as researched." The spec's own `## Clarifications` section already resolved the 10 product-level questions; the items below are the technology-layer decisions implied by those clarifications.

Format per the spec-kit research template:

> **Decision** — what was chosen.
> **Rationale** — why.
> **Alternatives** — what else was considered, and why rejected.

---

## R-1 Claude Agent SDK choice (closes plan PR-1)

**Decision** Use Anthropic's `claude-agent-sdk` (Python) — the package distributed on PyPI under that name, the same one `nova/k8s-agent-claude-sdk` uses. `ClaudeSDKClient` is the per-case session class; `ClaudeAgentOptions` carries `permission_mode`, `system_prompt`, `mcp_servers`, `hooks`, and the tool-allow knob. The `@tool` decorator + `create_sdk_mcp_server` API is the route for registering `emit_report` as an in-process terminal tool.

**Rationale**
- Spec FR-3 enumerates SDK features explicitly: `ClaudeSDKClient`, `ClaudeAgentOptions`, `permission_mode="bypassPermissions"`, `hooks=[("PreToolUse", …)]`, `mcp_servers={"nr": …}`. These are the SDK's first-class APIs, not hand-rolled wrappers.
- Identical wire protocol to the public Anthropic API — pointing `ANTHROPIC_BASE_URL` at the nerd-completion gateway requires zero SDK code changes (verified empirically by `nova/k8s-agent-claude-sdk`'s deployment shape).
- Prompt-cache `cache_control` on the system block (FR-3 / G2) is a one-flag SDK feature; rolling our own cache layer would be a substantial Day-1 cost.
- The SDK ships its own `Bash` tool implementation. The constitution's hackathon-scope simplification ("call `Bash` directly, no allow-list") aligns with the SDK's default behavior — we enable `Bash` and add no `can_use_tool` callback.

**Alternatives**
- **Hand-rolled HTTP client against the Anthropic Messages API.** Possible — the API surface is documented — but we'd write tool-call dispatch, hook execution, prompt-cache management, and message-loop ourselves. Constitution III.2 (stub before integrate) implies we want the smallest reliable runtime; reinventing the SDK's loop is the opposite.
- **`langgraph` / `langchain` agents with the Anthropic provider.** Adds a major framework + its dep tree on top of the SDK. The agent's loop shape is straightforward; LangGraph buys nothing for a single-loop autonomous diagnosis case.
- **Anthropic's TypeScript SDK + a Node.js runtime.** Possible but the team's Python familiarity (CLAUDE.md "Python: scripts, analysis, one-off tools") and `nova/k8s-agent-claude-sdk` precedent both point at Python.

**Citations** Spec FR-3 / FR-4; `nova/k8s-agent-claude-sdk` (referenced in spec §G2 and manifests/README.md); Anthropic Claude Agent SDK Python README at `github.com/anthropics/claude-agent-sdk-python`.

---

## R-2 Web framework: FastAPI + uvicorn (closes plan PR-2)

**Decision** FastAPI app served by uvicorn (`uvicorn[standard]`, `--workers 1`). `POST /diagnose` is an `async def` handler that does case-table lookup + `asyncio.create_task(run_case(case))` + returns 202. `GET /healthz` is trivial. `GET /readyz` calls the model-resolution check (FR-3) and the NR MCP probe (FR-2) once at startup, caches the result for 30 s.

**Rationale**
- FastAPI is async-native; the Claude Agent SDK runs on `asyncio`. Sharing one event loop across the HTTP server and the case workers means we don't bridge sync↔async unnecessarily.
- Pydantic v2 (FastAPI's transitive) is the same library we want for validating the `POST /diagnose` body (FR-1) and the `emit_report` payload (FR-8). One dep, two uses.
- Single uvicorn worker keeps the in-memory case table coherent (FR-10). Multi-worker would need leader-style coordination we don't want for a hackathon.
- TestClient drops into pytest cleanly for the integration tests in the plan's testing section.

**Alternatives**
- **Flask + gunicorn.** Synchronous; would force a thread-pool bridge into the Claude Agent SDK's asyncio loop. Strictly more glue.
- **`aiohttp`.** Lower-level async server. FastAPI gives us schema validation + OpenAPI for free; the OpenAPI artifact alone (`/docs`) is useful for the demo dry-run.
- **Starlette directly.** FastAPI is Starlette + Pydantic + a route decorator. Skipping FastAPI saves nothing meaningful.

**Citations** FastAPI docs; uvicorn docs; spec FR-1 / FR-2 / FR-10.

---

## R-3 Kubernetes client choice (closes plan PR-3)

**Decision** Official `kubernetes` Python client (PyPI package `kubernetes`). Use `kubernetes.config.load_incluster_config()` at startup; talk to NHD via `client.CustomObjectsApi`'s `get_namespaced_custom_object_status` and `replace_namespaced_custom_object_status` methods. No generated CRD types; no operator framework.

**Rationale**
- The agent's kube footprint is tiny: one `get` + one `replace_status` per case (with the FR-12 pre-write phase re-read, that's two `get`s + one `replace_status`). Generated types would buy strict typing the dynamic client lacks, but Pydantic models in `nodemedic_agent/tools/emit_report.py` already validate the payload before the kube call. Generating types from the CRD adds a build step (`openapi-python-client` or `kubernetes-asyncio`'s codegen) for marginal upside.
- The `replace_status` path hits the `status` subresource, which is what FR-8 + FR-14 require (the agent's RBAC has `update` on `nodehealthdiagnosisais/status` only).
- The `kubernetes` package is mature, well-documented, and the same library every Python operator example in the org uses.

**Alternatives**
- **`kopf`** (Kubernetes Operator Pythonic Framework). Brings a reconciler-style watcher loop, custom-handler decorators, and lots of opinions. The agent isn't a reconciler — it doesn't watch for events, it receives `POST /diagnose` and writes back. kopf's machinery is overhead.
- **`kr8s`** (newer, async-native client). Promising but less battle-tested. The official client has an async variant (`kubernetes-asyncio`) we can switch to if we hit blocking-IO issues, but the per-case kube call volume is so low that even the sync client's blocking call (run via `asyncio.to_thread(...)` if needed) is fine.
- **Raw HTTP via `httpx`.** Requires us to handle in-cluster serviceaccount-token rotation, CA verification, and the API server's content-type quirks ourselves. The official client does all of this for free.

**Citations** Spec FR-8 (`Status().Update`), FR-12 (pre-write phase re-read), FR-14 (RBAC scoping); `kubernetes-client/python` README.

---

## R-4 Concurrency model (closes plan PR-4)

**Decision** One `asyncio.Task` per case, gated by an `asyncio.Semaphore(MAX_CONCURRENT_CASES)` (default `32`). The `POST /diagnose` handler calls `case_table.try_register(case_id)` (FR-10), then if the slot is fresh, calls `asyncio.create_task(run_case(case))` and returns 202. The semaphore is acquired *inside* `run_case` before the SDK loop starts; `acquire()` is non-blocking via `asyncio.wait_for(..., timeout=0)` — if the cap is full the handler returns 429 before spawning the task.

**Rationale**
- Spec FR-1 makes 429 a real return code at the cap; spec FR-10 makes the case-table the source of truth for idempotency. These two interact: a duplicate `caseId` arriving when the cap is full should still return 202 (idempotent — the case is already running), not 429. Doing the case-table check first satisfies this.
- `asyncio.Task` keeps the worker on the same event loop as FastAPI. No threadpool, no process pool, no inter-task message bus.
- `Semaphore` is the canonical async cap primitive in Python; `MAX_CONCURRENT_CASES=32` is the spec's default.

**Alternatives**
- **`concurrent.futures.ThreadPoolExecutor`.** Forces async↔sync bridging at every SDK call. The SDK is async-native; threading buys nothing.
- **A separate worker process per case (`multiprocessing.Pool`).** Heavyweight; per-case startup would dominate the typical 30–50 s loop. Idempotency across processes would need shared state (Redis / file lock).
- **`anyio` task group.** `asyncio.TaskGroup` (Python 3.11+) covers the same pattern with stdlib only.

**Citations** Spec FR-1 (cap), FR-10 (idempotency), NFR-8 (load shape: 1 typical / 10 peak / 32 ceiling).

---

## R-5 `emit_report` registration via `@tool` (closes plan PR-5)

**Decision** Register `emit_report` as an in-process tool using the Claude Agent SDK's tool-decoration API: define `@tool("emit_report", description=…, input_schema=…)` on a closure that captures the per-case `Case` context. Register the resulting tool with `create_sdk_mcp_server(name="nodemedic", tools=[emit_report])` and pass it via `ClaudeAgentOptions.mcp_servers={"nodemedic": …, "nr": …}`. Allow the tool by name in the SDK's tool allowlist (`mcp__nodemedic__emit_report`).

**Rationale**
- The SDK's in-process MCP server pattern is the cleanest way to register a tool the runner intercepts: the tool function runs in the agent process, the runner can validate the args, write the CR, and return whatever it likes to the model — and the loop ends naturally because the runbook prompt instructs the agent to call no further tool after `emit_report`.
- The closure capturing the per-case `Case` object lets the tool implementation call `nhd_writer.update_status(case, payload)` directly. No global state. Each `ClaudeSDKClient` instance gets its own MCP server with its own closure (FR-3 fresh-session-per-case binding).
- Schema validation lives in the input schema declared on the `@tool` decorator. The SDK enforces argument shape before the function is invoked; our function only needs to validate semantic constraints (e.g. `confidence ∈ [0,1]`, `evidence` non-empty, `evidence[].result ≤ 4 KB`) per FR-8 step 1.

**Alternatives**
- **Sentinel last-message parse.** Parse the assistant's final message as JSON and treat that as the report. Spec FR-8 explicitly considers and rejects this for brittleness.
- **External MCP server hosting `emit_report`.** Adds a network hop and a separate process for what is fundamentally a callback into the runner. Defeats the whole point of "the runner intercepts the call."
- **`Bash kubectl patch` from the agent.** Spec FR-8 explicitly considers and rejects this — loses schema validation, FR-12 phase-conflict guards, and the case-complete log line ordering.

**Citations** Spec FR-8 (full rationale + schema); Anthropic Claude Agent SDK Python `@tool` / `create_sdk_mcp_server` reference.

---

## R-6 New Relic HTTP MCP wiring (closes plan PR-6)

**Decision** Configure NR MCP as a streaming-HTTP MCP server in `ClaudeAgentOptions.mcp_servers["nr"]`, pointing at the public NR HTTP MCP at the URL set on `NR_MCP_URL` (per spec NFR-6) with `Authorization: Bearer ${NR_MCP_TOKEN}` injected via the SDK's MCP server config `headers` field. The runbook prompt (AC-15 clause 5) carries the "set `account_id=1` on every call" instruction; we do not enforce this in code (deferred per the constitution's hackathon-scope simplifications — the NR token's own scope is the safety boundary).

**Rationale**
- The NR HTTP MCP at `docs.newrelic.com/docs/agentic-ai/mcp/` is publicly documented and speaks the standard MCP HTTP transport. The SDK's `mcp_servers` config supports HTTP-transport entries directly.
- Spec FR-4 §2 enumerates the tools the agent uses (`execute_nrql_query`, `analyze_entity_logs`, etc.). Wiring is one config block, not a per-tool wrapper.
- `account_id=1` is a runbook-prompt instruction, not a schema enforcement, per the constitution's hackathon-scope deferral. If the agent passes a different account, the NR token's scope (staging account `1` only) returns an authorization error and the agent moves on. Closed in spec §10 Q2.

**Alternatives**
- **In-proc Python wrapper around `nrql.execute(...)` + a Python NR API client.** The spec's earlier draft had this as a `nrql` SDK-MCP tool. Constitution Article I (hackathon-scope simplifications) explicitly rejected this; we use the public HTTP MCP server unmodified.
- **Direct NerdGraph HTTP POST from a `Bash curl` call.** Possible; the runbook could instruct that. The NR HTTP MCP gives us per-tool docs and arg validation server-side, which is more useful than raw NerdGraph at the model interface.

**Citations** Spec FR-4 §2; NR HTTP MCP server docs at `docs.newrelic.com/docs/agentic-ai/mcp/`.

---

## R-7 Runbook lint script (closes plan PR-7)

**Decision** `tests/nodemedic_agent/check_runbook.sh` — a single bash script that `grep -E`'s the canonical runbook at `prompts/runbook.md` for each of the seven AC-15 clauses, exits 0 if all match, exits non-zero with a per-missing-clause error message otherwise. Run by:
1. CI (`.github/workflows/nodemedic-agent-ci.yml` invokes it directly).
2. A Helm pre-install Job (`deployment/helm/nodemedic-agent/templates/runbook-lint-job.yaml`) that mounts the rendered image's `/app/prompts/runbook.md` and runs the script. The Helm install fails if the Job's exit code is non-zero (`hook-delete-policy: before-hook-creation`).

The seven clauses are encoded as `grep -P` patterns, one per AC-15 sub-bullet:
1. Cloud-dispatch sections — `^\s*##\s+AWS:` and `^\s*##\s+Azure:` headings present.
2. Evidence calibration paragraph — phrase `confidence below 0.7` or `lower the confidence` present.
3. Recommendation rubric — phrase `recommendation\.action\s*=\s*"?NoAction"?` and `do not recommend cordoning` present.
4. Credential-path forbid list — `\$SSH_KEY_PATH`, `/etc/nodemedic/ssh/`, `/var/run/secrets/` all present in a "do not read" context.
5. NRQL `account_id=1` instruction — `account_id\s*=\s*1` present.
6. `emit_report` calling discipline — phrase `call emit_report exactly once` present.
7. Decision-tree sections — headings `^\s*##\s+ContainerRuntimeUnhealthy` and `^\s*##\s+KubeletUnhealthy` present.

**Rationale**
- Bash + grep is right-sized for AC-15 (seven literal substring/regex checks against one file). pytest would force a Python test discoverable from `pyproject.toml` — useful but heavier than necessary.
- The Helm pre-install Job means a partial runbook ships fail at install time, not at first POST /diagnose. AC-15 explicitly requires "blocking AC-3 from being claimed if any clause is missing"; the install-time block is the strongest possible interpretation.
- Same script in two places (CI + Helm Job) means the contract is enforced both at PR review and at deployment.

**Alternatives**
- **pytest in a Job.** Pulls Python + pip + the entire test tree into the Helm pre-install image. Bash + alpine + grep is ~5 MB.
- **Hardcoded checks inside the agent's startup path (`/readyz`).** Pushes the failure to runtime, after image build + Helm install + pod start. AC-15 wants build/install-time.
- **A separate runbook-validator binary.** Same overhead as the pytest path.

**Citations** Spec AC-15 (the seven clauses); plan-section "Testing".

---

## R-8 Helm chart shape (closes plan PR-8)

**Decision** Mirror the controller chart's layout exactly. Files at `deployment/helm/nodemedic-agent/`:
- `Chart.yaml` (apiVersion v2, type application, version 0.1.0, kubeVersion `>=1.28.0-0` — same floor as controller).
- `values.yaml` (defaults; `clusterName: ""` deliberately unset so the helper-template guard fires).
- `values-azure.yaml` (cf1z install — sets `cloud.provider: azure`, references `nodemedic-azure-creds` Secret, sets image tag).
- `values-eks.yaml` (placeholder for the future test-* install — sets `cloud.provider: aws`, IRSA annotation, references no Azure Secret).
- `templates/_helpers.tpl` — reuses the `requireTestCluster` pattern from the controller chart, extended to accept `cf1z`, `jc1z`, `sk1z`, and any `test-*` prefix (per spec §G10).
- `templates/{deployment,service,serviceaccount,clusterrole,clusterrolebinding}.yaml` — one resource per file, parallel to controller.
- `templates/runbook-lint-job.yaml` — Helm `pre-install` hook Job that runs `check_runbook.sh` against the runbook baked into the agent image (R-7).

The Service is `ClusterIP` on port 8080 with selector `app.kubernetes.io/name: nodemedic-agent`, matching the controller's `agentURL: http://nodemedic-agent.cf-monitoring.svc:8080/diagnose` (verified empirically against `deployment/helm/nodemedic-controller/values-azure.yaml:21`).

**Rationale**
- Reviewer cognitive load: a captain who has reviewed the controller chart can review the agent chart in five minutes. Same template names, same helper macros, same per-cloud values split.
- The `requireTestCluster` guard is the constitution's Article I.5 enforcement at chart install time. Reusing the controller's pattern (already verified to work on cf1z) is strictly safer than rewriting it.
- Pre-install Job for the runbook lint is the strongest place to gate AC-15 (R-7).
- Service shape is dictated by the cross-scope contract — the controller's `agentURL` already names this Service path (`nodemedic-agent.cf-monitoring.svc:8080`).

**Alternatives**
- **Single chart serving both controller + agent.** Would couple their release cadences. The constitution Article II.1 wants independent lifecycles (`Restart of the controller does not restart the agent and vice versa` — spec FR-15); separate charts enforce this.
- **Library chart + thin wrappers.** Premature abstraction. Two charts with shared shape are reviewable; a library chart needs its own versioning + tests.

**Citations** Spec FR-15, AC-15, NFR-9; existing `deployment/helm/nodemedic-controller/` for shape reference.

---

## R-9 CI workflow (closes plan PR-9)

**Decision** New `.github/workflows/nodemedic-agent-ci.yml`, structured the same way as the controller's `nodemedic-ci.yml`:

```yaml
name: nodemedic-agent-ci

on:
  pull_request:
    paths:
      - 'cmd/nodemedic-agent/**'
      - 'nodemedic_agent/**'
      - 'prompts/**'
      - 'deployment/helm/nodemedic-agent/**'
      - 'tests/nodemedic_agent/**'
      - 'Dockerfile.nodemedic-agent'
      - 'pyproject.toml'
      - 'uv.lock'
      - '.github/workflows/nodemedic-agent-ci.yml'
  workflow_dispatch:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@... (pinned)
      - uses: actions/setup-python@... (pinned, python-version 3.12)
      - run: pip install uv && uv sync --all-extras --dev
      - run: uv run pytest tests/nodemedic_agent/

  helm-lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@...
      - uses: azure/setup-helm@...
      - run: helm lint deployment/helm/nodemedic-agent --values deployment/helm/nodemedic-agent/values-azure.yaml --set clusterName=cf1z

  runbook-lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@...
      - run: bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md
```

Image build/push is **manual** for the hackathon — the same shape the controller uses today (developer cf-registry creds, `make nodemedic-agent-docker-build && make nodemedic-agent-docker-push` from a laptop). No GitHub Actions push to cf-registry; that requires a service account we don't have time to set up before the demo.

**Rationale**
- Path-filter trigger keeps the agent CI out of the way of NPD's existing CI (CodeQL, Scorecards, dependency-review) and the controller's CI. Same precedent as the controller workflow.
- Three jobs, each tightly scoped, run in parallel. The `runbook-lint` job is the build-time half of AC-15.
- Manual image push is the path the controller already uses on `hackathon-2026/cf1z-baseline` — see the most recent commits `nodemedic(controller): migrate cf1z image to cf-registry` and `nodemedic(controller): pin builder-base to BUILDPLATFORM`. The agent can ride the same proven workflow.

**Alternatives**
- **Auto-push on tag.** Requires a registry service account. Post-hackathon work.
- **Build/push from a single CI job that does everything.** Couples lint failures to image build failures unhelpfully; separate jobs make breaks bisectable.

**Citations** Existing `.github/workflows/nodemedic-ci.yml` (controller CI shape); spec NFR-9 (image build conventions); recent commits `61865c62`, `3f9ee3af` (controller image pipeline lessons).

---

## R-10 Image build (closes plan PR-10)

**Decision** `Dockerfile.nodemedic-agent` at the repo root, multi-stage:

1. **Builder stage** (`--platform=$BUILDPLATFORM`, base `python:3.12-slim`) — installs `uv`, runs `uv sync --frozen --no-dev`, produces a `/app/.venv` virtualenv.
2. **CLI bundle stage** (`--platform=$BUILDPLATFORM`, base `python:3.12-slim`) — `pip install awscli` + downloads `kubectl` 1.33.x + downloads `az` CLI tarball. CLI tools are large (~450 MB combined); isolating them to a stage keeps the runtime stage cacheable.
3. **Runtime stage** (`linux/amd64`, base `python:3.12-slim`) — `COPY --from=builder /app/.venv /app/.venv`, `COPY --from=cli /usr/local/aws-cli /usr/local/aws-cli` (and similar for kubectl + az), `COPY prompts/runbook.md /app/prompts/runbook.md`, `COPY nodemedic_agent /app/nodemedic_agent`, `COPY cmd/nodemedic-agent /app/cmd/nodemedic-agent`. Final image runs as non-root user `1000:1000`. `CMD ["/app/.venv/bin/uvicorn", "nodemedic_agent.app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1"]`.

Target image size ≤ 1.5 GB per spec NFR-9. Tag convention `dev-cf1z-<short-sha>` per the Clarifications binding. Push: `docker push cf-registry.nr-ops.net/container-fabric/nodemedic-agent:dev-cf1z-<sha>` with developer cf-registry creds.

**Rationale**
- `--platform=$BUILDPLATFORM` on the build/CLI stages lets Apple Silicon dev hosts cross-compile cleanly via Colima — same lesson the controller image learned (commit `3f9ee3af`).
- Runtime stage stays `linux/amd64` because cf1z worker nodes are amd64-only. Building a multi-arch manifest is wasted bytes for the hackathon.
- `uv sync --frozen --no-dev` produces a deterministic virtualenv from the lockfile — same Python toolchain the user prefers (CLAUDE.md "Python: scripts, analysis, one-off tools. Keep simple; use `uv` / venv").
- Three stages keeps the image cache discriminating: source-only edits don't invalidate the CLI-bundle layer.

**Alternatives**
- **Single-stage Dockerfile.** Every Python source change reinstalls the AWS CLI. Slow.
- **`pip install` instead of `uv`.** uv is 10–100× faster on dependency resolution and matches the user's stated preference.
- **`distroless` runtime base.** Distroless images don't have `bash`, which the agent's primary tool surface (FR-4) requires. `python:3.12-slim` is the right floor.
- **Buildx multi-arch image.** No arm64 cf1z nodes; not worth the build minutes.

**Citations** Spec NFR-9 (image size + tag convention); commits `61865c62` (controller cf-registry migration) and `3f9ee3af` (BUILDPLATFORM pinning lesson); existing `Dockerfile.nodemedic-controller` for build-stage reference.

---

## R-11 Day-1-AM stub deliverable (Constitution II.3 / III.2)

**Decision** Day 1 AM stub for the agent:
1. Flask-equivalent FastAPI app with `POST /diagnose` returning 202 and `GET /healthz` returning 200. The handler validates the body against the Pydantic `DiagnoseRequest` schema and returns 400 on shape errors.
2. The "case worker" sleeps 30 s, then writes a hand-canned `status.diagnosis` to the NHD identified by `caseId`:
   - `confidence: 0.85`
   - `evidence: [{source: "kubectl", ref: "kubectl get node …", result: "stub", observedAt: …}, {source: "nrql", ref: "stub query", result: "stub", observedAt: …}]`
   - `recommendation: {action: "Cordon", reason: "stub diagnosis"}`
   - `rootCause: "stub root cause for Day-1 integration"`, `rcaCategory: "Unknown"`
   - `phase: Diagnosed`
3. No SDK loop, no model calls, no `Bash`. Just enough to let the controller (Spec 001) exercise its FR-6 confidence gate + FR-8 cordon path against a real CR.

The controller's `--stub-agent=true` flag (already deployed on cf1z) covers the inverse direction — the controller writes a hand-canned diagnosis when the agent is unavailable. Day 1 AM these two stubs eliminate the dependency arrow between scopes; both teams can develop in parallel against a real CR shape.

**Rationale**
- Constitution Article II.3 + III.2 are explicit: each scope ships a stub on Day 1 AM.
- The 30 s sleep emulates the typical demo loop duration (spec NFR-1 soft target ~30–50 s) so the controller's deadline path (Spec 001 FR-5) is exercised in the same time-window the real agent will fill.
- The hand-canned payload meets the controller's confidence-gate threshold (`confidence ≥ 0.7`, two distinct sources, action ∈ {Cordon, DrainAndCordon}) so the gate-pass path runs end-to-end against a real cordon. A second stub variant with `confidence: 0.4` covers the gate-fail path (matches spec AC-6).

**Alternatives**
- **Wait until the SDK loop is wired before Scope 2 can integrate.** Constitution III.2 explicitly forbids this pattern.
- **Stub controller-side only.** Doesn't unblock the agent's own deploy/install + RBAC / Helm rehearsal — those need a live agent pod.

**Citations** Constitution Article II.3 + III.2; controller's `--stub-agent=true` deployment on cf1z (verified live 2026-06-13/14); spec AC-6.

---

## R-12 Concurrent-load + memory sizing rebuild (verifies NFR-8)

**Decision** Accept the spec's NFR-8 sizing: request `1 Gi` / limit `3 Gi` memory, request `1` / limit `4` CPU. No empirical baseline yet — first measurement comes after Day 1 evening's first end-to-end demo run.

**Rationale**
- The spec's NFR-8 walks the memory math (idle ~400 MB CLI bundle + Python runtime, +75 MB per active case, peak rehearsal 10 cases ≈ 1.15 GB, hypothetical 32-case ceiling ≈ 2.8 GB). The 3 Gi limit covers the ceiling with margin; the 1 Gi request matches typical demo load.
- CPU is I/O-bound (waiting on Anthropic gateway, NR MCP, SSH, cloud APIs); the `1 / 4` request/limit shape gives bursting headroom on JSON parsing without overcommitting.
- Empirical revision after Day 1: if 10-case rehearsal drives the working set above 2 GB, raise the limit to 4 Gi before raising `MAX_CONCURRENT_CASES`. Cap is sized for safety, not ergonomics.

**Alternatives**
- **Smaller initial limits + auto-raise on the fly.** Risks first-demo OOM. Hackathon optimal: start generous, tighten after data.
- **Set HPA on agent.** Adds a deployment surface (HPA + metrics-server reach + scaling latency) for a load shape that doesn't need it.

**Citations** Spec NFR-8 (sizing rationale).

---

## R-13 NHD `Status().Update` failure-class handling (verifies FR-8 retry policy)

**Decision** Implement the spec's FR-8 retry policy verbatim in `nodemedic_agent/kube/nhd_writer.py`:

| Failure class | Action |
|---|---|
| `NotFound` (404) | Retry 3 times at 250 ms / 500 ms / 1 s (FR-8 + Clarifications binding). |
| Conflict (409) on status subresource | Re-read CR, retry once after 1 s. |
| Other 5xx / network | Retry once after 1 s. |
| 403 RBAC denied / 422 schema invalid | No retry — terminal. |

Detection:
- `kubernetes.client.exceptions.ApiException.status` integer for 404/409/403/422.
- Network errors raise `urllib3.exceptions.MaxRetryError` or `kubernetes.client.exceptions.ApiException(status=0)` — bucket as "transient" and use the `Other 5xx / network` path.

Before each `Status().Update` attempt the writer re-reads the CR and consults FR-12's phase-conflict matrix (`""`/`Pending`/`Diagnosing` proceed; `Diagnosed`/`Acted`/`Failed` defer with INFO log + return cleanly).

**Rationale**
- The FR-8 retry policy is bound by spec + Clarifications. This R-item just confirms the Python-side detection mechanics.
- The 250 ms / 500 ms / 1 s `NotFound` schedule absorbs the structural race between the controller's `Create` and the agent's first cache observation (per Clarifications). Without it, the demo flake rate on first POST after a fresh deploy would be unacceptable.
- The 409 path on status subresource fires when two writers race; FR-12's pre-write phase re-read is the merge step.

**Alternatives**
- **Indefinite retry on 5xx.** Would drag a flaky cluster into a multi-minute case duration. The spec's "retry once" budget is the right shape.
- **Use server-side apply.** Marginal benefit at the cost of additional client-side complexity (field manager string, conflict resolution mode). Spec FR-8 mentions SSA; implementing it is post-hackathon polish.

**Citations** Spec FR-8 (retry table); Clarifications session entry on NotFound retry timing.

---

## R-14 Open items not yet resolved (non-blocking)

None. All plan-phase decisions PR-1 through PR-10 are resolved (R-1 through R-10). Phase 1 may proceed.

**Items deliberately deferred to Phase 2 (`/speckit-tasks`)** — these aren't decisions but task-sequencing concerns:

- Sequencing AC-3 (containerd) vs AC-3b (kubelet) demos in the rehearsal block.
- Whether to author both `values-azure.yaml` (cf1z) and `values-eks.yaml` (placeholder) in the same task or stage them.
- Whether the `runbook-lint-job.yaml` Helm pre-install Job is built in Day 1 AM stub or Day 1 PM (the lint script itself ships Day 1 AM either way; only the in-cluster Job wiring is at issue).

These are scheduling questions, not technology questions, and belong in `tasks.md`.
