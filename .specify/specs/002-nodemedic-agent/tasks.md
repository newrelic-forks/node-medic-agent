---
description: "Task list for NodeMedic Agent (Scope 3 of AFA 2026 hackathon)"
---

# Tasks: NodeMedic Agent

**Input**: Design documents from `.specify/specs/002-nodemedic-agent/`

**Prerequisites**: [`plan.md`](./plan.md), [`spec.md`](./spec.md), [`research.md`](./research.md), [`data-model.md`](./data-model.md), [`contracts/`](./contracts/), [`quickstart.md`](./quickstart.md), [`constitution.md`](../../memory/constitution.md), [`manifests/`](./manifests/)

**Tests**: Tests are mandatory per user instruction. Every implementation task is preceded by a unit-test task and followed by a cf1z end-to-end gate at meaningful boundaries. No task is "done" until its unit tests are green and the next-up E2E gate has run successfully against real components on cf1z.

**Organization**: Tasks are grouped by user story (mapped from spec §8 acceptance criteria). Each story is an independently demonstrable increment on cf1z.

**Working tree**: All paths are repo-relative. Agent code lands directly on `hackathon-2026/cf1z-baseline` (per the plan — the controller landed on this branch via PR #2, the agent follows the same pattern). Spec-kit artifacts stay at `.specify/specs/002-nodemedic-agent/`. Agent code is namespaced under `nodemedic_agent/`, `cmd/nodemedic-agent/`, `prompts/`, `deployment/helm/nodemedic-agent/`, and `tests/nodemedic_agent/` so it doesn't collide with NPD's existing tree or the Spec 001 controller.

**Cluster context**: cf1z (Azure kubeadm, k8s 1.33.8). Controller `nodemedic-controller` is already deployed in `cf-monitoring` running `dev-cf1z-9e029aa9` with `--stub-agent=true`. Canary node `cf1z-general-nodes-2000002` labeled `canary-chaos-test=true`. Chaos cronjobs `chaos-containerd-unhealthy` and `chaos-kubelet-unhealthy` deployed in `default` namespace and verified end-to-end 2026-06-14. Controller flips off stub mode (`config.stubAgent=false`) only after US1 lands.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: Maps task to a user story (US1, US2, US3, US4, US5). Phase 1/2/Polish tasks have no story tag.
- File paths are exact; the agent repo root is `/Users/smandyashankar/Downloads/node-medic-agent` (or `~/Downloads/node-medic-agent`).

## User stories (derived from spec §4 + §8)

| ID | Title | Priority | Acceptance criteria |
|---|---|---|---|
| US1 | **Day-1 AM stub: deployed agent answers `POST /diagnose` and lets the controller exercise its gate against a real CR** | **P1 (MVP)** | AC-1, AC-2, AC-8, AC-10, AC-11, AC-15 (build-time + install-time runbook lint) |
| US2 | **Containerd happy-path diagnosis (the demo's headline win)** — real SDK loop, runbook v1, NR MCP, Bash, `emit_report`, end-to-end on cf1z's `chaos-containerd-unhealthy` injector | P1 | AC-3, AC-9 |
| US3 | **Kubelet path + case isolation** — second deployed NodeCondition exercises the same loop with a different runbook decision-tree section | P2 | AC-3b, AC-14 |
| US4 | **Failure modes** — single-source acceptance routes to HumanInLoop; late-write race is suppressed by FR-12 | P2 | AC-6, AC-7 |
| US5 | **Cross-cloud safety** — credential layer prevents `aws …` calls on the cf1z (Azure) install (and symmetrically) | P3 | AC-13 |

**Constitution checkpoints sprinkled through the plan** (mirrors the controller's tasks.md pattern):
- **Article I.1 credential-layer least privilege** — T023 (`ClusterRole` template), T039 (per-cluster Secret apply), T083 (captain chart review). Programmatic verification deferred per FR-13.
- **Article I.2 cordon-only** — T023 (`ClusterRole` template explicitly omits `nodes/patch`) + T077 (CI grep test).
- **Article I.3 confidence gate binding** — T053 (`emit_report` schema accepts single-evidence) + T070 (unit test confirms thin acceptance) + T073 (cf1z gate confirms controller-side routing). Controller-side gate is the sole enforcer (US4 / AC-6).
- **Article I.4 every tool call observable** — T056 (`PreToolUse` + `PostToolUse` hooks) + T036/T057 (case-complete log line in stub and real worker) + T077 (CI grep test that the hook is wired).
- **Article I.5 non-production clusters only** — T010 (config validator) + T022 (Helm `_helpers.tpl` cluster-name guard accepting `cf1z`/`jc1z`/`sk1z`/`test-*`) + T026 (chart render test).
- **Article II.1 two cross-scope contracts only** — T028 (`POST /diagnose` golden bodies match controller's `DiagnoseRequest` byte-for-byte) + T048 (`emit_report` golden matches `contracts/emit-report-tool.md`).
- **Article II.2 CRD owned by Scope 3** — process-level (PR review against `api/v1alpha1/`); programmatic drift check at T085 (CRD-payload sync test against the vendored OpenAPI schema).
- **Article II.3 stub before integrate** — US1 entire phase = the Day-1 AM stub. T047 closes the milestone (controller flips off `--stub-agent=true` and exercises its gate against the agent's real CR).
- **Article II.7 one binary, two clouds** — T024 (per-cloud values) + T075 (chart credential isolation) + T076 (cf1z cross-cloud probe gate).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Bootstrap the Python workspace inside this repo without disturbing the NPD tree, the Spec 001 controller tree, or any existing CI workflows. No agent behavior yet.

- [X] T001 Confirm working tree is on `hackathon-2026/cf1z-baseline` and clean (`git status` reports nothing) before starting Phase 1; if a `hackathon-2026/scope3-agent` branch is wanted instead, cut it now from `cf1z-baseline` per plan.md and update tasks.md's working-tree note in the same commit.
- [X] T002 Hand-scaffold the directory tree: `cmd/nodemedic-agent/`, `nodemedic_agent/{api,runner,tools,kube}/`, `prompts/`, `deployment/helm/nodemedic-agent/{templates,}/`, `tests/nodemedic_agent/{unit,integration,fixtures,helpers}/`. Each new Python package gets an empty `__init__.py` so `pytest --collect-only` exits 0 cleanly until Phase 2 lands real code.
- [X] T003 Author `pyproject.toml` at repo root: `[project]` with name `nodemedic-agent`, Python 3.12 floor, deps from plan §Primary Dependencies (`claude-agent-sdk`, `fastapi`, `uvicorn[standard]`, `kubernetes`, `pydantic`, `pydantic-settings`, `httpx`, `structlog`, optional extra `metrics` for `prometheus-client` + `prometheus-fastapi-instrumentator`). `[tool.uv]` block with `dev-dependencies = ["pytest", "pytest-asyncio", "respx"]`. `[tool.pytest.ini_options]` with `testpaths = ["tests/nodemedic_agent"]` so pytest only picks up agent tests, leaving any future Go test discovery untouched. `[tool.setuptools.packages.find]` scoped to `["nodemedic_agent*"]`.
- [X] T004 Run `uv lock` once to produce `uv.lock` at repo root (deterministic build input for `Dockerfile.nodemedic-agent`); commit both `pyproject.toml` and `uv.lock` in the same commit. Verify `uv sync --all-extras --dev` succeeds in a clean checkout.
- [X] T005 Author `cmd/nodemedic-agent/main.py` as a thin uvicorn entrypoint that imports `nodemedic_agent.app:create_app` and calls `uvicorn.run(...)` with `--workers 1` baked in. Module is intentionally <30 lines — all wiring lives inside `nodemedic_agent.app`.
- [X] T006 [P] Author `Dockerfile.nodemedic-agent` at repo root per research R-10: three-stage build (Python builder with `--platform=$BUILDPLATFORM`, CLI bundle stage with `--platform=$BUILDPLATFORM` for `aws` CLI v2 + `az` CLI 2.x + `kubectl` 1.33.x, runtime stage pinned to `linux/amd64` because cf1z is amd64-only). `COPY prompts/runbook.md /app/prompts/runbook.md` (image SHA pins the runbook version per Clarifications). `CMD ["/app/.venv/bin/uvicorn", "nodemedic_agent.app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1"]`. Image label `org.opencontainers.image.source` points at the agent repo URL. Reproducibility per NFR-9: no random UUIDs in layers, no per-build timestamps in non-OCI labels, pin all `apt`/`pip` package versions, `--no-cache-dir` on `pip install`. The `Dockerfile.nodemedic-agent` MUST also `COPY tests/nodemedic_agent/check_runbook.sh /app/tests/check_runbook.sh && chmod +x /app/tests/check_runbook.sh` so the Helm pre-install Job (T025) can invoke it from the image.
- [X] T007 [P] Extend top-level `Makefile` with `nodemedic-agent-help`, `nodemedic-agent-lint`, `nodemedic-agent-test`, `nodemedic-agent-runbook-lint`, `nodemedic-agent-helm-lint`, `nodemedic-agent-docker-build`, `nodemedic-agent-docker-push`, `nodemedic-agent-clean`. Image tag convention `dev-cf1z-$(shell git rev-parse --short=8 HEAD)` matching the controller's tagging. Verify NPD's `make test` and the controller's `make nodemedic-*` targets are unchanged via `make -n test` and `make -n nodemedic-build` diff against the previous commit.
- [X] T008 [P] Author `.github/workflows/nodemedic-agent-ci.yml` per research R-9: path-filtered to agent paths only; three parallel jobs (`test` → `uv run pytest tests/nodemedic_agent/`; `helm-lint` → `helm lint deployment/helm/nodemedic-agent --set clusterName=cf1z`; `runbook-lint` → `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md`). Pin all action SHAs. Image build/push stays manual for the hackathon (Colima → cf-registry with developer creds, per research R-9).
- [X] T009 [P] Author `prompts/runbook.md` v0 — a placeholder containing only the seven AC-15 clause anchor markers (cloud-dispatch headings `## AWS:` / `## Azure:`, evidence calibration sentence with `confidence below 0.7`, recommendation rubric line with `recommendation.action = "NoAction"`, credential-path forbid block listing `$SSH_KEY_PATH`, NRQL `account_id=1` instruction, `emit_report` calling discipline phrase, decision-tree headings `## ContainerRuntimeUnhealthy` and `## KubeletUnhealthy`). v0 makes the lint script pass at build time but does NOT yet contain the runbook content needed for AC-3 — that lands in T052 once US2's runbook decision tree is authored. The lint exists as a structural gate from Phase 1; behavioral runbook content lands in US2.

**Checkpoint**: `uv sync` + `pytest --collect-only` + `helm lint deployment/helm/nodemedic-agent --set clusterName=cf1z` (the chart is empty but `helm lint` should not crash on the empty templates dir — gracefully skips) + `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md` all exit 0. NPD + controller test/build targets unchanged.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Config, structured logging, in-cluster kube client, case table, runbook lint script, and Helm chart skeleton with the I.5 cluster-name guard. Everything below blocks every user story. **No US-tagged task may begin until this phase is complete and its E2E gate (T027) has been verified on cf1z.**

### Config + logging (NFR-6 / NFR-3)

- [X] T010 Author `nodemedic_agent/config.py` per data-model.md §7: `Settings(BaseSettings)` with every env in NFR-6's table, validators on required fields, `Literal` enums for `log_level` and `cloud_provider`. Includes a `_validate_cluster_allowlist` post-init check that mirrors the Helm guard in T015 — refuses to start with `cluster_name` outside `{cf1z, jc1z, sk1z}` ∪ `test-*` (Constitution Article I.5 belt-and-suspenders).
- [X] T011 [P] Author `nodemedic_agent/logging.py` per data-model.md §5: structlog config emitting JSON-per-line on stdout, processor chain that injects `ts` (RFC3339 UTC), `level`, and binds `case_id` from a `contextvars.ContextVar` so every log line in a worker task carries it without explicit threading. INFO baseline; WARN/ERROR per NFR-3.

### Unit tests for config + logging

- [X] T012 [P] Author `tests/nodemedic_agent/unit/test_config.py` — table-driven Pydantic validation: missing required env → `ValidationError`; bad cluster name (e.g. `stg-going-plaid`) → rejected; `cloud_provider` accepts only `aws|azure`; `log_level` accepts only the four documented values. Run via `uv run pytest tests/nodemedic_agent/unit/test_config.py -v`. **Must pass before T014.**
- [X] T013 [P] Author `tests/nodemedic_agent/unit/test_logging.py` — captures stdout via `capsys`, logs a sample line, asserts JSON shape carries `ts`, `level`, `case_id` (when bound), and that `case_id` propagates across `await` boundaries via `contextvars`. **Must pass before T014.**

### Kube client + NHD writer (FR-12, FR-14, R-13)

- [X] T014 Author `nodemedic_agent/kube/client.py`: `load_in_cluster_config()` at process start; expose a single `CustomObjectsApi` instance + a `get_namespaced_custom_object_status` / `replace_namespaced_custom_object_status` helper pair. Pure function `should_overwrite(observed_phase: str) -> bool` (FR-12 phase-conflict matrix) lives here as a stateless helper.
- [X] T015 [P] Author `nodemedic_agent/kube/nhd_writer.py` per data-model.md §8: `NHDWriter.get`, `NHDWriter.update_status` with the FR-8 retry policy (404 NotFound → 250ms/500ms/1s 3 retries; 409 Conflict → re-read + 1s retry once; 5xx/network → 1s retry once; 403/422 → terminal). Return `WriteOutcome` enum. Includes the `find_by_case_id(namespace, case_id)` list+filter fallback per data-model.md §1.4 if `nhd_name` is not provided in the request body.

### Unit tests for kube client + NHD writer

- [X] T016 [P] Author `tests/nodemedic_agent/unit/test_should_overwrite.py` — exhaustive truth table for FR-12 phases: `""`, `Pending`, `Diagnosing` → `should_overwrite` returns True; `Diagnosed`, `Acted`, `Failed` → False. **Must pass before T018.**
- [X] T017 [P] Author `tests/nodemedic_agent/unit/test_nhd_writer.py` — uses `respx` (already a dev dep) to mock the apiserver responses; covers each FR-8 retry case (200, 404×3 then success, 404×4 then exhaustion, 409 then 200, 5xx then 200, 403 terminal, 422 terminal). Asserts elapsed time roughly matches the 250+500+1000 ms backoff schedule. **Must pass before T018.**

### Case table (FR-10)

- [X] T018 Author `nodemedic_agent/runner/case_table.py` per data-model.md §2: `Case` Pydantic model, `CaseTable` class wrapping a `dict[str, Case]` guarded by an `asyncio.Lock`, methods `try_register(case_id, case) -> CaseStatus|None`, `mark_running`, `mark_complete`, `lookup`, plus a 30-min retention sweep on a background `asyncio.Task` that wakes every 5 min.
- [X] T019 [P] Author `tests/nodemedic_agent/unit/test_case_table.py` — table-driven idempotency test: register-new → QUEUED; re-register-same-id while QUEUED/RUNNING → returns existing entry, no second worker; re-register-same-id while COMPLETE within 30 min → returns existing; re-register after retention sweep at 30:01 → fresh QUEUED. Uses `freezegun` (add as dev dep in this task — update `pyproject.toml` and re-run `uv lock`). **Must pass before T026.**

### Runbook lint script (R-7, AC-15 build-time half)

- [X] T020 Author `tests/nodemedic_agent/check_runbook.sh` per research R-7: bash script taking `$1 = path-to-runbook.md`, runs seven `grep -E` patterns (one per AC-15 clause), exits 0 only if all match, prints per-missing-clause error otherwise. Make it executable (`chmod +x`). Verify via `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md` (exits 0 — v0 placeholder runbook from T009 has all seven anchors) and `bash tests/nodemedic_agent/check_runbook.sh /dev/null` (exits non-zero, lists all seven clauses missing).

### Helm chart skeleton (R-8)

- [X] T021 [P] Author `deployment/helm/nodemedic-agent/Chart.yaml` (apiVersion v2, type application, version 0.1.0, kubeVersion `>=1.28.0-0` matching the controller chart's floor) and `values.yaml` (defaults: `clusterName: ""` deliberately unset so the helper guard fires; `image.repository: cf-registry.nr-ops.net/container-fabric/nodemedic-agent`, `image.tag: ""`; `cloud.provider: ""`; `replicaCount: 1` with a YAML comment `# DO NOT raise without addressing FR-10 case-table coherence + NFR-2 leader coordination — multi-replica is out of scope for v1`; resource requests `1Gi`/`1` and limits `3Gi`/`4` per NFR-8; `config.maxConcurrentCases: 32`; `config.stubAgent: false` (production path; flipped to `true` only for unit-test isolation); secret refs to `nodemedic-anthropic-token`, `nodemedic-nr-token`, `nodemedic-ssh-key`, `nodemedic-azure-creds`).
- [X] T022 Author `deployment/helm/nodemedic-agent/templates/_helpers.tpl` per Constitution Article I.5: `nodemedic-agent.requireTestCluster` named template that calls `fail` if `.Values.clusterName` is empty or doesn't match `{cf1z, jc1z, sk1z}` ∪ a `test-*` prefix. Mirrors the controller chart's helper, extended per spec §G10.
- [X] T023 [P] Author `deployment/helm/nodemedic-agent/templates/serviceaccount.yaml` (with IRSA annotation `eks.amazonaws.com/role-arn` only when `.Values.cloud.provider=aws`), `clusterrole.yaml` (FR-14 minimal verbs: `nhd` get/list/watch/update/patch + status update; `nodes` get/list/watch — **explicitly NO patch** per Constitution Article I.2; `pods` get/list/watch; `events` get/list/watch), `clusterrolebinding.yaml`, `service.yaml` (ClusterIP :8080 → :8080, selector matching the controller's `agentURL` per the controller's `values-azure.yaml:21`), `deployment.yaml` (single replica, `runAsNonRoot: true`, `readOnlyRootFilesystem: true` with `emptyDir` for `/tmp` per NFR-8, drop ALL caps, healthz/readyz probes, env vars wired from `Settings`, secret mounts at the paths in NFR-6). Required env wiring (Helm value → env var): `clusterName → CLUSTER_NAME`, `cloud.provider → CLOUD_PROVIDER`, `config.maxConcurrentCases → MAX_CONCURRENT_CASES`, `config.stubAgent → STUB_AGENT`, plus the secret-sourced tokens per NFR-6 (anthropic, NR MCP, Azure tuple, SSH path).
- [X] T024 [P] Author `deployment/helm/nodemedic-agent/values-azure.yaml` (cf1z install: `cloud.provider: azure`, references `nodemedic-azure-creds` Secret, no IRSA annotation) and `deployment/helm/nodemedic-agent/values-eks.yaml` (placeholder for future test-* install: `cloud.provider: aws`, IRSA annotation, no Azure Secret). Both reference the same image tag set at install time.
- [X] T025 [P] Author `deployment/helm/nodemedic-agent/templates/runbook-lint-job.yaml` — Helm `pre-install,pre-upgrade` hook Job per research R-7 that runs the rendered image's `/app/tests/check_runbook.sh /app/prompts/runbook.md` and exits non-zero if any clause is missing (`hook-delete-policy: before-hook-creation,hook-succeeded`). The script is COPY'd into the image at the same path (update `Dockerfile.nodemedic-agent` in T006 if not already there). This is the install-time half of AC-15.

### Unit + chart-render tests for foundational

- [X] T026 Author `tests/nodemedic_agent/integration/test_helm_render.py` — invokes `helm template ./deployment/helm/nodemedic-agent --set clusterName=cf1z` via `subprocess`, asserts: `ClusterRole` lacks `nodes/patch` and any `*` verb on `nodes`; `Deployment` mounts the four expected secrets; `requireTestCluster` rejects `clusterName=stg-going-plaid` (chart render fails with the expected error string). Sister test asserts `cf1z`, `jc1z`, `sk1z`, `test-foo` all render successfully. **Must pass before T027.**

### Phase 2 cf1z E2E gate

- [X] T027 **cf1z gate (Phase 2 → Phase 3)**: deploy nothing yet — verify only that the chart renders, the runbook lint passes, and `kubectl --context=cf1z get ns cf-monitoring` confirms the target namespace exists with the controller already running on `--stub-agent=true`. Run: `helm template nodemedic-agent ./deployment/helm/nodemedic-agent --values deployment/helm/nodemedic-agent/values-azure.yaml --set clusterName=cf1z --set image.tag=foo | kubectl --context=cf1z apply --dry-run=server -f -`. Expected: zero errors, zero warnings besides the missing image (image isn't built yet). Confirms RBAC + service shape are accepted by cf1z's apiserver. **Phase 3 cannot start until this gate is green.**

**Checkpoint**: All Phase 1+2 unit tests pass. Helm chart renders with the cf1z guard active. Runbook v0 lint passes. Day-1 AM milestone for Constitution II.3 is structurally ready — US1 layers the stub worker on top.

---

## Phase 3: User Story 1 — Day-1 AM stub: deployed agent answers `POST /diagnose` (Priority: P1) MVP

**Goal**: Build, push, install the agent on cf1z. `POST /diagnose` validates the body and returns 202; a stub worker sleeps 30 s and writes a hand-canned `status.diagnosis` (confidence 0.85, two evidence entries, `recommendation.action=Cordon`, `phase=Diagnosed`) to the NHD CR. Lets the controller flip off `--stub-agent=true` and exercise its FR-6 gate + FR-8 cordon path against a real CR before the SDK loop is wired.

**Independent test (E2E gate at end of phase)**: Run [`quickstart.md`](./quickstart.md) Scenarios A (AC-2), F (AC-8 idempotency), H (AC-10 concurrency cap), I (AC-11 RBAC), and L (AC-15 install-time lint) on cf1z. Verify AC-1 by `kubectl --context=cf1z -n cf-monitoring get deploy nodemedic-agent` shows 1/1.

### Tests for User Story 1 (write FIRST, expect to FAIL before implementation)

- [ ] T028 [P] [US1] Author `tests/nodemedic_agent/fixtures/case_aws.json` (golden body from contracts/post-diagnose.md §Golden request body) and `tests/nodemedic_agent/fixtures/case_azure.json` (cf1z Azure golden body). Byte-stable — these are the contract reference fixtures.
- [ ] T029 [P] [US1] Author `tests/nodemedic_agent/integration/test_post_diagnose.py` — FastAPI `TestClient` covering: valid AWS body → 202 with `{caseId, status: "queued"}`; valid cf1z Azure body → 202; missing `caseId` → 400; bad `provider` enum → 400; bad UUID → 400; invalid `maxBudgetUSD` regex → 400; `Authorization: Bearer junk` header on a valid body → 202 (header ignored, FR-1). Uses a fake worker (`async def fake_run_case(case): await asyncio.sleep(0)`) so tests don't talk to the gateway. **Expect to FAIL until T037 (the `app.py` factory wires the diagnose router from T033) lands.**
- [ ] T030 [P] [US1] Author `tests/nodemedic_agent/integration/test_idempotent.py` — POST same body twice rapidly while a fake worker holds the case in RUNNING; assert second POST returns 202 without spawning a second worker (counts `case_started` log lines via `caplog`). **Expect to FAIL until T037 lands.**
- [ ] T031 [P] [US1] Author `tests/nodemedic_agent/integration/test_concurrency_cap.py` — set `MAX_CONCURRENT_CASES=2`, post 3 distinct caseIds simultaneously; assert exactly one returns 429 with `{caseId, status: "rejected", reason: "concurrency_cap"}`. **Expect to FAIL until T037 lands.**
- [ ] T032 [P] [US1] Author `tests/nodemedic_agent/integration/test_health_ready.py` — `GET /healthz` always returns 200; `GET /readyz` returns 200 only after a successful model-resolution probe (mock the gateway catalog endpoint via `respx`); `GET /readyz` returns 503 if the gateway returns no usable Claude model. **Expect to FAIL until T037 lands** (depends on `app.py` wiring the health router from T034 + the model resolver from T035).

### Implementation for User Story 1

- [ ] T033 [US1] Author `nodemedic_agent/api/diagnose.py`: `DiagnoseRequest` and `DiagnoseResponse` Pydantic models per data-model.md §3 (with the optional `nhdName` field per §1.4); `router = APIRouter()` with `POST /diagnose` handler that does `case_table.try_register(...)` first, then concurrency-cap acquire, then `asyncio.create_task(run_case(...))`, then return 202. The handler is small (≤30 lines) — all real work lives downstream.
- [ ] T034 [P] [US1] Author `nodemedic_agent/api/health.py`: `GET /healthz` (always 200 if process is up); `GET /readyz` calls `model_resolver.resolve()` (T035) + an HTTP HEAD against `NR_MCP_URL` with `READYZ_PROBE_TIMEOUT_SEC` budget; caches the result for 30 s.
- [ ] T035 [P] [US1] Author `nodemedic_agent/runner/model_resolver.py` per data-model.md §6 + FR-3 fallback chain: `ResolvedModels` Pydantic type, `resolve(settings, http_client) -> ResolvedModels` async function that hits the gateway's catalog endpoint, walks the exact → best-Opus → best-Sonnet → fail chain. Uses `httpx.AsyncClient(timeout=settings.readyz_probe_timeout_sec)` so a sick gateway can't hang the readiness probe (NFR-6). Logs `event=model_resolved primary=… fallback=… resolution_path=…` at INFO at startup.
- [ ] T036 [US1] Author `nodemedic_agent/runner/case_worker.py` **stub variant** per research R-11: `async def run_case_stub(case: Case, nhd_writer: NHDWriter)` sleeps 30 s, then builds a hand-canned `status.diagnosis` (confidence 0.85, two evidence entries citing kubectl + nrql sources, `rcaCategory: Unknown`, `recommendation.action: Cordon`, `rootCause: "Day-1 stub diagnosis for integration testing"`), calls `nhd_writer.update_status(... phase="Diagnosed" ...)`. Set the worker behind a feature flag `Settings.stub_agent: bool = True` (default True for US1; flipped to False in T058 once the real SDK loop in T057 is wired). Stub also emits a `case_started` and `case_complete` log line so AC-8 paths are observable from Day-1 AM (AC-9 — full per-tool-call observability — gets exercised in US2 once the real `PreToolUse` hook is wired).
- [ ] T037 [US1] Author `nodemedic_agent/app.py`: `create_app(settings: Settings) -> FastAPI` factory. `lifespan` context: load in-cluster config (T014), instantiate `CaseTable` + `NHDWriter` + `model_resolver`, register the diagnose router (T033) and health router (T034), kick off the case-table retention sweeper. On shutdown, cancel any in-flight worker tasks gracefully.

### Build + push + deploy (US1 cf1z gate)

- [ ] T038 [US1] Run `make nodemedic-agent-test` (all unit + integration tests green). Build the image: `colima start ...` if not running; `make nodemedic-agent-docker-build TAG=dev-cf1z-$(git rev-parse --short=8 HEAD)`; verify image size ≤ 1.5 GB per NFR-9.
- [ ] T039 [US1] Apply per-cluster Secret manifests: `kubectl --context=cf1z apply -f .specify/specs/002-nodemedic-agent/manifests/secret-anthropic-token.yaml` (after Vault retrieval per `manifests/README.md`), `secret-nr-token.yaml`, `secret-ssh-key.yaml`, `secret-azure-creds.yaml`. Skip `serviceaccount-irsa.yaml` (cf1z is Azure). Verify `kubectl --context=cf1z -n cf-monitoring get secrets` shows all four.
- [ ] T040 [US1] `make nodemedic-agent-docker-push TAG=dev-cf1z-$SHA`. Confirm push completes without auth prompts (developer cf-registry creds, verified empirically 2026-06-14).
- [ ] T041 [US1] `helm --kube-context=cf1z upgrade --install nodemedic-agent ./deployment/helm/nodemedic-agent -n cf-monitoring -f deployment/helm/nodemedic-agent/values-azure.yaml --set clusterName=cf1z --set image.tag=dev-cf1z-$SHA --wait`. The runbook-lint pre-install Job (T025) gates the install — passes because runbook v0 has all seven anchor markers. Verify `kubectl --context=cf1z -n cf-monitoring get deploy nodemedic-agent` shows 1/1 ready (AC-1).

### US1 cf1z E2E gate — quickstart Scenarios A, F, H, I, L

- [ ] T042 [US1] **AC-2 gate** — quickstart Scenario A: `kubectl --context=cf1z -n cf-monitoring port-forward svc/nodemedic-agent 8080:8080 &`; valid POST → 202; missing-field → 400; wrong-enum → 400; auth header → 202 (ignored). All four pass.
- [ ] T043 [US1] [P] **AC-8 gate** — quickstart Scenario F: post the cf1z Azure golden body twice rapidly; verify exactly one `case_started` log line via `kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --tail=500 | jq 'select(.case_id == "1a2b3c4d-5e6f-4789-9abc-def012345678" and .event == "case_started")' | wc -l`.
- [ ] T044 [US1] [P] **AC-10 gate** — quickstart Scenario H: `helm upgrade … --set config.maxConcurrentCases=2`; post 3 distinct caseIds; expect exactly one 429. Restore `MAX_CONCURRENT_CASES=32` after via another `helm upgrade`.
- [ ] T045 [US1] [P] **AC-11 gate** — quickstart Scenario I: `kubectl --context=cf1z -n cf-monitoring auth can-i patch nodes --as=system:serviceaccount:cf-monitoring:nodemedic-agent` returns `no`; `auth can-i update nodehealthdiagnosisais/status …` returns `yes`; `auth can-i create pods …` returns `no`. All as expected per FR-14.
- [ ] T046 [US1] [P] **AC-15 install-time gate** — quickstart Scenario L: `kubectl --context=cf1z -n cf-monitoring logs job/nodemedic-agent-runbook-lint` confirms exit 0 and all seven clauses passed. (Build-time half from T020 is already enforced by CI.)
- [ ] T047 [US1] **End-to-end stub roundtrip on cf1z**: `helm --kube-context=cf1z upgrade nodemedic-controller … --set config.stubAgent=false` (controller now talks to the real agent). Trigger the `chaos-containerd-unhealthy` cronjob; expect the controller to `POST /diagnose`, the agent stub worker to sleep 30 s and write `status.diagnosis` with confidence 0.85, the controller's gate to pass, the controller to cordon `cf1z-general-nodes-2000002`, Slack `Applied` message to land. Verify `kubectl --context=cf1z get node cf1z-general-nodes-2000002 -o jsonpath='{.spec.unschedulable}'` returns `true`. Manually `kubectl uncordon` after — auto-uncordon is intentionally absent (controller territory). **This is the Day-1 AM milestone for Constitution II.3 / III.2.**

**Checkpoint (Day-1 AM done)**: AC-1, AC-2, AC-8, AC-10, AC-11, AC-15 (build + install) green on cf1z. Controller has been flipped to talk to the real agent for the first time. Stub diagnosis exercises the full controller gate + cordon + Slack pipeline against a real CR. **STOP and DEMO if Day-1 AM is the demo target.**

---

## Phase 4: User Story 2 — Containerd happy-path diagnosis (Priority: P1)

**Goal**: Replace the stub worker with the real Claude Agent SDK loop. `emit_report` registered as in-process MCP tool. Runbook v1 with the `## ContainerRuntimeUnhealthy` decision tree authored. `Bash`, NR HTTP MCP, and SDK built-ins enabled. `PreToolUse` hook emits structured stdout log lines. End-to-end on cf1z's `chaos-containerd-unhealthy` injector — the demo's headline win.

**Independent test (E2E gate at end of phase)**: quickstart Scenarios B (AC-3) + G (AC-9) on cf1z. The agent's diagnosis on `ContainerRuntimeUnhealthy` cites at least 2 distinct evidence sources, confidence ≥ 0.7, recommendation Cordon, phase Diagnosed. Every tool call appears in `kubectl logs` with its command/args.

### Tests for User Story 2 (write FIRST, expect to FAIL)

- [ ] T048 [P] [US2] Author `tests/nodemedic_agent/fixtures/emit_report_payload.json` (cf1z containerd happy-path golden, three evidence sources) and `tests/nodemedic_agent/fixtures/emit_report_payload_thin.json` (single-source gate-fail golden) per contracts/emit-report-tool.md §Golden payload.
- [ ] T049 [P] [US2] Author `tests/nodemedic_agent/unit/test_emit_report_validate.py` — table-driven Pydantic validation of `EmitReportPayload`: happy-path golden passes; thin golden passes (single-evidence accepted, FR-8 has no count gate, AC-6); empty `evidence` → ValidationError; `confidence=1.5` → ValidationError; `confidence=-0.1` → ValidationError; `rcaCategory="WrongEnum"` → ValidationError; `recommendation.action="DropNode"` → ValidationError; `rootCause` >4 KB → ValidationError; `evidence[].result` >4 KB → truncated to 4 KB with WARN log (not rejected).
- [ ] T050 [P] [US2] Author `tests/nodemedic_agent/unit/test_build_user_prompt.py` — verifies `build_user_prompt(case)` injects `nodeName`, `clusterName`, `provider`, `region`, `instanceId`, `trigger.{type,reason,message,observedAt}`, `caseId` exactly once each. Snapshot test against a frozen golden so prompt changes are reviewable as diffs.
- [ ] T051 [P] [US2] Author `tests/nodemedic_agent/unit/test_hooks.py` — `PreToolUse` hook emits a JSON log line per tool call with `case_id`, `turn`, `tool`, and either `command` (Bash) or `args` (non-Bash). Asserts truncation at 2 KB. Verifies the `case_id` propagation via `contextvars` from T011.

### Runbook v1 (the build-time AC-15 gate now must pass with real content)

- [ ] T052 [US2] Author `prompts/runbook.md` v1 — replace v0 placeholders with the real runbook content. Required sections (per AC-15):
  1. **Cloud-dispatch sections** (`## AWS:` and `## Azure:`) — explicit `aws …` and `az …` command guidance keyed off `case.provider`.
  2. **Evidence calibration paragraph** — explicit "lower confidence below 0.7 when evidence is single-source or contradictory" text.
  3. **Recommendation rubric** — explicit `recommendation.action="NoAction"` guidance for thin evidence; calls out controller's `GateSkip` interpretation.
  4. **Credential-path forbid block** — `$SSH_KEY_PATH`, `/etc/nodemedic/ssh/*`, `/var/run/secrets/**` listed under "do not read".
  5. **NRQL `account_id=1` instruction** — every NR MCP call.
  6. **`emit_report` calling discipline** — "call exactly once per case as the FINAL tool call; no further tools after `emit_report`".
  7. **`## ContainerRuntimeUnhealthy` decision tree** — which probes to run first (NPD socket check, kubectl describe pod, ssh to node, `aws ec2 describe-instance-status` / `az vm get-instance-view`, NRQL on `K8sNodeSample`), what's a slam-dunk vs ambiguous, which `rcaCategory` to emit (`Kernel` for socket-unreachable patterns).
  8. `## KubeletUnhealthy` decision tree as a stub heading only — full content in US3 (T066).
  Verify locally: `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md` exits 0.

### `emit_report` tool implementation (FR-8)

- [ ] T053 [US2] Author `nodemedic_agent/tools/emit_report.py` per data-model.md §4 + research R-5: `make_emit_report(case, nhd_writer)` factory closing over the per-case `Case`. Inside: define `@tool("emit_report", description=…, input_schema=EMIT_REPORT_INPUT_SCHEMA)` with the JSON Schema from contracts/emit-report-tool.md verbatim. Tool function: Pydantic-validates args via `EmitReportPayload.model_validate`, truncates `evidence[].result` to 4 KB if needed, calls `nhd_writer.update_status(... phase="Diagnosed" ...)`, returns the structured success/deferred/error envelope per contracts/emit-report-tool.md §Tool return value.
- [ ] T054 [US2] Author `tests/nodemedic_agent/integration/test_emit_report_writes_cr.py` — uses `respx` to mock the apiserver; happy-path golden → `WriteOutcome.WRITTEN`, returns `{ok: true, phase: "Diagnosed"}`; thin golden → still `WriteOutcome.WRITTEN`, single-evidence accepted (AC-6 unit-level coverage). Pre-existing `phase=Failed` on the CR → `WriteOutcome.DEFERRED_PHASE_CONFLICT`, returns `{ok: false, deferred: true, observedPhase: "Failed"}` (FR-12 unit-level coverage; full E2E in US4). **Plus the FR-11 failure-reason mapping**: 403 RBAC denied on the writer → `case_worker` writes a `Failed` CR with `reason=CRWriteFailed`; mocked `ClaudeSDKClient` exception → `Failed{reason=ModelError}`; tool-runtime exception during the loop → `Failed{reason=ToolError}`; model halt without `emit_report` → `Failed{reason=ModelHalted}`. **Must pass before T057.**

### SDK runner + hooks + prompt (FR-3, NFR-3)

- [ ] T055 [US2] Author `nodemedic_agent/runner/prompt.py`: `load_runbook(path)` reads `/app/prompts/runbook.md` once at startup and caches; `build_user_prompt(case: Case) -> str` produces the per-case user prompt with all the `case.*` fields injected. Both functions are pure.
- [ ] T056 [P] [US2] Author `nodemedic_agent/runner/hooks.py` per Article I.4 + spec FR-3: a single `tool_log_hook(...)` callable registered for **both** `PreToolUse` and `PostToolUse`. On `PreToolUse` it emits a `ToolCallLog` line with `tool` + `command`/`args` (truncated to 2 KB each). On `PostToolUse` it emits a paired line with the same `case_id`/`turn` plus `latency_ms`, and for `Bash` tools also `exit_code` + `stdout_bytes` + `stderr_bytes`. Both fires use the structlog config from T011. Author `nodemedic_agent/runner/nr_mcp.py` in the same task: `nr_mcp_config(settings) -> dict` factory returning the streaming-HTTP MCP config block per research R-6, with `Authorization: Bearer ${NR_MCP_TOKEN}` injected via the SDK's MCP server `headers` field.
- [ ] T057 [US2] Author the **real** `nodemedic_agent/runner/case_worker.py` — replaces the stub worker (T036). `async def run_case(case: Case, nhd_writer: NHDWriter, settings: Settings)`:
  1. Build per-case `mcp_server = create_sdk_mcp_server(name="nodemedic", tools=[make_emit_report(case, nhd_writer)])`.
  2. Load runbook **once at module init** (not per case) and wrap the system block with `cache_control={"type": "ephemeral"}` per spec FR-3 / G2 so Anthropic's prompt cache keeps the static runbook prefix warm across cases.
  3. Build `ClaudeAgentOptions(system_prompt=<cached runbook block>, permission_mode="bypassPermissions", hooks=[("PreToolUse", tool_log_hook), ("PostToolUse", tool_log_hook)], mcp_servers={"nodemedic": mcp_server, "nr": nr_mcp_config(settings)}, allowed_tools=["mcp__nodemedic__emit_report", "mcp__nr__*", "Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch"])` with `model=resolved.primary_resolved`, `fallback_model=resolved.fallback_resolved`.
  4. `async with ClaudeSDKClient(options=options) as client: await client.query(build_user_prompt(case)); async for msg in client.receive_response(): ...`.
  5. Loop ends naturally on `emit_report` tool call (the tool function does the CR write and returns) or model halt. Map terminal outcomes to FR-11 reasons:
     - `emit_report` returned `WriteOutcome.WRITTEN` → `case_complete{write_outcome="written", final_phase="Diagnosed"}`.
     - `emit_report` returned `WriteOutcome.DEFERRED_PHASE_CONFLICT` → `case_complete{write_outcome="deferred_phase_conflict", observed_phase=<phase>}` (FR-12; agent does NOT self-Failed).
     - `emit_report` returned `WriteOutcome.WRITE_FAILED` → write `Failed` CR (best-effort retry-once via the writer) with `reason=CRWriteFailed`; emit `case_complete{write_outcome="write_failed", failure_reason="CRWriteFailed"}`.
     - Model halts without calling `emit_report` → write `Failed` CR with `reason=ModelHalted`; emit matching `case_complete`.
     - Anthropic API exception (5xx, transport) → write `Failed` CR with `reason=ModelError`.
     - SDK tool-runtime exception (Bash crash, MCP transport) → write `Failed` CR with `reason=ToolError`.
  6. Emit the `case_complete` log line last per data-model.md §5 (three-shape: `written` / `deferred_phase_conflict` / `write_failed`).
  7. **No agent-side termination ceiling** (FR-7) — no `max_turns`, no `max_budget_usd`, no wall-clock cap.
- [ ] T058 [US2] Wire `Settings.stub_agent: bool` (set default to False — production path). When True, `app.py` selects `run_case_stub` from T036; when False, `run_case` from T057. Helm `values-azure.yaml` pins `config.stubAgent: false` for cf1z; the stub stays available for unit-test isolation.

### Image rebuild + deploy + cf1z gate

- [ ] T059 [US2] Run `make nodemedic-agent-test` (all unit + integration including `test_emit_report_validate.py`, `test_build_user_prompt.py`, `test_hooks.py`, `test_emit_report_writes_cr.py` green). Run `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md` (v1 lint passes). `make nodemedic-agent-docker-build TAG=dev-cf1z-$SHA` + `nodemedic-agent-docker-push TAG=dev-cf1z-$SHA`.
- [ ] T060 [US2] `helm --kube-context=cf1z upgrade nodemedic-agent ./deployment/helm/nodemedic-agent -n cf-monitoring -f deployment/helm/nodemedic-agent/values-azure.yaml --set clusterName=cf1z --set image.tag=dev-cf1z-$SHA --wait`. The pre-install Job re-validates runbook v1 (passes). `kubectl --context=cf1z -n cf-monitoring rollout status deploy/nodemedic-agent` confirms 1/1 ready. Startup logs show `event=model_resolved primary=… fallback=… resolution_path=exact|best-opus|best-sonnet`.

### US2 cf1z E2E gate — quickstart Scenarios B + G

- [ ] T061 [US2] **AC-3 gate** — quickstart Scenario B: `kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy chaos-containerd-test-manual -n default`. Watch NPD flip `ContainerRuntimeUnhealthy=True` within ~30 s. Watch the controller `POST /diagnose`. Watch the agent loop run via `kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --tail=500 -f | jq 'select(.case_id != null)'`. Verify the eventual NHD `status.diagnosis` has `confidence ≥ 0.7`, `evidence` length ≥ 2 with distinct sources (typically `kubectl` + `ssh` + `nrql` + `cloud`), `recommendation.action = Cordon`, `phase = Diagnosed`. Diagnosis lands within ~120 s (~60 s first deadline + ~60 s extension); if it lands after, US4 covers the late-write race.
- [ ] T062 [US2] [P] **AC-9 gate** — quickstart Scenario G: after T061 completes, `kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --since=10m | jq 'select(.case_id == "<the-case-id>")'`. Verify every `Bash` invocation has its `command`, every NR MCP call has its `args`, every SDK built-in call (Read/Write/Glob/etc.) is logged, and the `case_complete` line appears last. AC-9 passes.
- [ ] T063 [US2] **Empirical resource sizing measurement (NFR-8 / R-12)**: during T061, `kubectl --context=cf1z -n cf-monitoring top pod -l app.kubernetes.io/name=nodemedic-agent --containers`. Record peak memory and CPU. If peak >2 GB or CPU >2 cores, log a follow-up to revisit the limits in Polish (T082). Spec NFR-8's 1 case ≈ 475 MB target should hold; this task pins the empirical baseline.

**Checkpoint**: AC-3, AC-9 green on cf1z. Real diagnosis cited from real evidence, real Anthropic gateway, real NR MCP, real SSH to a real Azure VM. The demo's headline win is reproducible. **STOP and DEMO if Day-1 PM target is the AC-3 happy path alone.**

---

## Phase 5: User Story 3 — Kubelet path + case isolation (Priority: P2)

**Goal**: Extend runbook v1 with the `## KubeletUnhealthy` decision-tree section. Verify the `chaos-kubelet-unhealthy` cronjob path produces a clean diagnosis under the same code path. Verify case isolation between A (containerd) and B (kubelet) — the agent's per-case `ClaudeSDKClient` does not leak evidence across cases (G2 / FR-3).

**Independent test (E2E gate at end of phase)**: quickstart Scenarios C (AC-3b) + K (AC-14).

### Tests for User Story 3

- [ ] T064 [P] [US3] Author `tests/nodemedic_agent/unit/test_runbook_kubelet_section.py` — asserts `prompts/runbook.md` contains a `## KubeletUnhealthy` section with non-trivial content (≥30 lines under that heading; mentions `journalctl -u kubelet`, `kubectl get events`, `127.0.0.1:10248`, `iptables`). Pure structural check; the build-time lint (T020) only checks the heading exists.
- [ ] T065 [P] [US3] Author `tests/nodemedic_agent/integration/test_case_isolation.py` — drives two consecutive cases through fake workers with distinct caseIds; asserts logs filtered by case_id are disjoint trees, no shared message-history; the second case's `ClaudeSDKClient` is a fresh instance (introspect via mock).

### Runbook expansion + redeploy

- [ ] T066 [US3] Author the `## KubeletUnhealthy` decision-tree section in `prompts/runbook.md`: which probes to run first (`kubectl get node <name> -o yaml` confirming the condition; `ssh` to the node for `journalctl -u kubelet --since "5 min ago"`; `kubectl get events --field-selector involvedObject.name=<node>`; `iptables -L -n` on the node to detect REJECT rules on `127.0.0.1:10248`; NRQL on `K8sNodeSample` for kubelet metric drops), what's slam-dunk (kubelet healthz blocked by iptables → `rcaCategory: Kubelet`) vs ambiguous (kubelet up but health probe flaking → request manual review), which `rcaCategory` to emit. Verify `bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md` still exits 0.
- [ ] T067 [US3] `make nodemedic-agent-docker-build TAG=dev-cf1z-$SHA` + push + `helm upgrade` on cf1z (image SHA pins runbook v1.1).

### US3 cf1z E2E gate — quickstart Scenarios C + K

- [ ] T068 [US3] **AC-3b gate** — quickstart Scenario C: `kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy chaos-kubelet-test-manual -n default`. Wait ~30 s for NPD to flip `KubeletUnhealthy=True` with `reason=KubeletHealthzFailed`. Watch the agent's diagnosis land — must cite at least one NRQL row, one SSH probe (typically `journalctl -u kubelet`), and one kube-side or cloud-side probe (`kubectl get events …` or `az vm get-instance-view …`). After ~90 s the iptables REJECT is removed, NPD recovers `KubeletUnhealthy` to `False` with `reason=KubeletIsHealthy`.
- [ ] T069 [US3] **AC-14 gate** — quickstart Scenario K: run the containerd path to completion, wait 60 s, run the kubelet path on the same canary node. `kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --tail=2000 | jq -s 'group_by(.case_id) | map({case_id: .[0].case_id, calls: length, tools: [.[].tool] | unique})'`. Assert: case A and case B have distinct `case_id`s; the trees are disjoint (no log line of case B references case A's evidence). Inspect both NHD CRs — case B's `status.diagnosis.evidence[].result` strings reference only kubelet-fault content, no containerd content from case A.

**Checkpoint**: AC-3b, AC-14 green on cf1z. Both Day-1 NodeConditions exercise the same agent loop with the right runbook decision tree firing. Case isolation verified end-to-end against real chaos.

---

## Phase 6: User Story 4 — Failure modes (low-confidence + late-write race) (Priority: P2)

**Goal**: Verify the agent accepts single-evidence reports (FR-8 no count gate, AC-6) and the controller's confidence gate routes them to `HumanInLoop`. Verify FR-12's late-write suppression — when the controller marks `phase=Failed` mid-loop, the agent's eventual `emit_report` is suppressed and the structured stdout logs are the only record of the late completion.

**Independent test (E2E gate at end of phase)**: quickstart Scenarios D (AC-6) + E (AC-7).

### Tests for User Story 4

- [ ] T070 [P] [US4] Author `tests/nodemedic_agent/unit/test_emit_report_thin_accepted.py` — calls `EmitReportPayload.model_validate(thin_golden)` directly; asserts no validation error raised; asserts the runner's `emit_report` tool function returns `{ok: true, phase: "Diagnosed"}` even with `evidence` length 1 and `confidence: 0.4`. Confirms the agent-side gate has been removed (FR-8 step 2).
- [ ] T071 [P] [US4] Author `tests/nodemedic_agent/integration/test_late_write_suppression.py` — drive the writer with a mock apiserver that returns `phase=Failed` on the pre-write read; assert `WriteOutcome.DEFERRED_PHASE_CONFLICT`; assert the deferred-write log line carries `case_id`, `observed_phase=Failed`, and the intended payload; assert the case-complete log line per data-model.md §5 carries `write_outcome="deferred_phase_conflict"`, `observed_phase="Failed"`, and `final_phase=None` (the three-shape `CaseCompleteLog`). Sister assertions for `observed_phase ∈ {"Diagnosed", "Acted"}` to round out the FR-12 matrix.
- [ ] T072 [P] [US4] Author `tests/nodemedic_agent/helpers/emit_thin_report.py` per quickstart Scenario D Option 2 — a small CLI helper that posts a hand-canned thin `emit_report` payload directly into the agent process for AC-6 demo rehearsal (uses uvicorn TestClient internally; useful for quickstart Scenario D's repeatable demo path).

### US4 cf1z E2E gate — quickstart Scenarios D + E

- [ ] T073 [US4] **AC-6 gate** — quickstart Scenario D: post a real case via Scenario B's chaos trigger, hand-craft a thin emit_report payload via `uv run python -m tests.nodemedic_agent.helpers.emit_thin_report --case-id <id> --nhd-name <name>`. Verify NHD `status.diagnosis` lands with `evidence` length 1, `confidence: 0.4`, `recommendation.action: NoAction`. Verify controller's confidence gate routes to `HumanInLoop` (no cordon; node remains schedulable). Verify Slack message has the "needs human review" framing.
- [ ] T074 [US4] **AC-7 gate** — quickstart Scenario E: trigger `chaos-containerd-unhealthy`. As soon as the agent logs `case_started`, hand-patch the CR's `status.phase=Failed` via `kubectl --context=cf1z -n cf-monitoring patch nhd $NHD_NAME --subresource=status --type=merge -p '{"status":{"phase":"Failed",...}}'`. Wait for the agent's loop to finish; observe the deferred-write log line via `kubectl logs … | jq 'select(.event == "deferred_write_phase_conflict")'`. Verify CR retains `phase=Failed` after the agent's eventual `emit_report` (AC-7 pass). No agent-side metric reports a failure.

**Checkpoint**: AC-6, AC-7 green on cf1z. Both failure modes (gate-fail demo path + controller-deadline race) reproducible from quickstart.

---

## Phase 7: User Story 5 — Cross-cloud probing prevented (Priority: P3)

**Goal**: Verify FR-6's credential-layer-only enforcement of cloud dispatch — on cf1z (Azure install), `aws` CLI calls fail at the credential layer. Symmetric AWS-side verification is deferred until a `test-*` EKS cluster has a chaos injector (spec AC-4 footnote — out of scope for hackathon kickoff).

**Independent test (E2E gate at end of phase)**: quickstart Scenario J (AC-13).

### Tests for User Story 5

- [ ] T075 [P] [US5] Author `tests/nodemedic_agent/integration/test_chart_credential_isolation.py` — `helm template … --set cloud.provider=azure …` produces a Deployment that mounts `nodemedic-azure-creds` Secret and does NOT mount any `nodemedic-aws-*` Secret or set IRSA annotation; `helm template … --set cloud.provider=aws …` symmetrically. Confirms NFR-5 chart-side enforcement.

### US5 cf1z E2E gate — quickstart Scenario J

- [ ] T076 [US5] **AC-13 gate** — quickstart Scenario J: `kubectl --context=cf1z -n cf-monitoring exec deploy/nodemedic-agent -- aws ec2 describe-instance-status --region us-east-2`. Expected: command fails with "Unable to locate credentials" or equivalent. Confirms the cf1z (Azure) install carries no AWS credentials, so even if the agent's runbook prompt drifted toward an `aws …` call, it would fail at the credential layer (FR-6). The symmetric AWS-side check (`az` calls fail on a `test-*` EKS install) is deferred — log a follow-up issue when an EKS test cluster has a chaos injector.

**Checkpoint**: AC-13 green on cf1z. The credential layer is the cloud-dispatch enforcement boundary; no code-level guard in the agent.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: CI invariants, optional metrics, image-size verification, README, follow-ups from the empirical sizing measurement, and a captain review pass before the demo.

- [ ] T077 [P] Author `tests/nodemedic_agent/unit/test_constitution_invariants.py` — CI grep tests covering the constitution checkpoints listed at the top of this file. (a) `! grep -rn 'nodes/patch\|nodes\s*\*\|kubectl cordon\|kubectl drain' deployment/helm/nodemedic-agent/templates/ nodemedic_agent/` (Article I.2 — agent never patches nodes). (b) `! grep -rn 'mcp_servers\|claude-agent-sdk\|anthropic' deployment/helm/nodemedic-controller/templates/` (Article II.5 — controller never wires MCP). (c) `grep -q 'PreToolUse' nodemedic_agent/runner/case_worker.py` (Article I.4 — the hook is wired). Pure-bash assertions inside pytest via `subprocess.check_output`.
- [ ] T078 [P] Author optional `nodemedic_agent/metrics.py` per FR-2 and NFR-3 — wire `prometheus-fastapi-instrumentator` if `metrics` extra is installed; export the seven recommended metrics (`nodemedic_agent_cases_total`, `_failure_reason_total`, `_latency_seconds`, `_tool_calls_total`, `_inflight_cases`, `_queue_depth`, `_build_info`). Exposed at `/metrics`. AC-12 — N/A if not implemented; pin its inclusion based on whether it falls out cheaply during this task.
- [ ] T079 [P] Author `deployment/helm/nodemedic-agent/README.md` — values reference, install / upgrade / uninstall commands for cf1z, RBAC review checklist (FR-13's deferred verification covered by captain chart-review per the constitution), credential-source pointers (Vault paths from manifests/README.md).
- [ ] T080 Verify image size with `docker image ls cf-registry.nr-ops.net/container-fabric/nodemedic-agent:dev-cf1z-$SHA --format '{{.Size}}'` ≤ 1.5 GB per NFR-9. If over: profile layers via `docker history`, prune unused stages, target the `az` CLI tarball as the most likely overspend.
- [ ] T081 Run the full quickstart.md A→L on cf1z one more time end-to-end. Capture the structured stdout logs from the live demo, save to `docs/cf/demos/nodemedic-agent-day1.json` for the demo deck. (The constitution defers durable per-case JSONL; this task captures a one-off snapshot manually.)
- [ ] T082 Empirical resource-sizing follow-up from T063: if peak memory exceeded 2 GB or CPU exceeded 2 cores during AC-3, raise the limits in `values-azure.yaml` before raising `MAX_CONCURRENT_CASES`. If within budget, document the actual peak in T079's README so future operators know the realistic ceiling.
- [ ] T083 Captain review pass: have a captain outside Scope 3 walk the rendered Helm chart (`helm template … --set clusterName=cf1z`) confirming the FR-14 RBAC verbs, the NFR-5 single-cloud-credential mount, and the NFR-9 image tag/path. Constitution Article I.1 is verified by chart review per FR-13's hackathon-scope deferral.
- [ ] T084 Run all CI workflows on a fresh PR: `nodemedic-agent-ci.yml` test/helm-lint/runbook-lint jobs all green. Existing `nodemedic-ci.yml` and NPD CI unaffected (path filter verification via touching agent paths only).
- [ ] T085 [P] Author `tests/nodemedic_agent/integration/test_crd_payload_sync.py` per Constitution Article II.2 — load the controller-authoritative CRD schema from `.specify/specs/002-nodemedic-agent/contracts/nhd-crd.yaml` (vendored from Spec 001's `api/v1alpha1/nodehealthdiagnosisai_types.go`); use `jsonschema` to validate that an `EmitReportPayload` happy-path golden serialized through `nhd_writer.update_status`'s payload builder conforms to the OpenAPI schema's `status.diagnosis` block. Catches silent drift if the controller's Go types evolve and the agent's Pydantic model lags. CI-only test (no kube needed). Add `jsonschema` to dev deps in this task.

**Checkpoint**: All ACs that have a test surface (AC-1 through AC-15 except AC-4, AC-5, AC-12 N/A footnotes) are green on cf1z. Constitution invariants enforced by CI. README + demo log captured. Day-2 demo rehearsal can run the full quickstart.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup, T001–T009)**: No dependencies — can start immediately on `hackathon-2026/cf1z-baseline`.
- **Phase 2 (Foundational, T010–T027)**: Depends on Phase 1 completion. **Blocks all user stories.** T027 (cf1z gate) is the hard go/no-go for Phase 3.
- **Phase 3 (US1, T028–T047)**: Depends on Phase 2. The Day-1 AM milestone for Constitution II.3.
- **Phase 4 (US2, T048–T063)**: Depends on Phase 3 (replaces the stub worker; needs the controller already pointing at the real agent). Also depends on prompts/runbook.md v1 (T052) being authored before T060's redeploy because the runbook-lint pre-install Job will refuse a placeholder runbook in production once the lint patterns are tightened.
- **Phase 5 (US3, T064–T069)**: Depends on Phase 4. Same code path; runbook expansion only.
- **Phase 6 (US4, T070–T074)**: Depends on Phase 4 (needs the real SDK loop running so the late-write race is reproducible). Independent of Phase 5.
- **Phase 7 (US5, T075–T076)**: Depends on Phase 3 (chart credential isolation is verifiable from US1 onward). Independent of Phases 4/5/6.
- **Phase 8 (Polish, T077–T085)**: Depends on all desired user stories.

### User Story Dependencies

- **US1 (P1, MVP — Day-1 AM stub)**: Foundational only. Ships the deployable agent with a stub worker. Lets the controller flip off `--stub-agent=true`.
- **US2 (P1 — containerd demo)**: Foundational + US1 (needs the agent already deployed and the controller talking to the real agent). Replaces the stub worker with the real SDK loop. **The demo's headline win.**
- **US3 (P2 — kubelet + case isolation)**: Foundational + US2 (same code path, runbook-only addition).
- **US4 (P2 — failure modes)**: Foundational + US2 (needs real SDK loop for late-write race; AC-6 has a unit-level test in US2's T054 already, but the cf1z E2E lives here).
- **US5 (P3 — cross-cloud safety)**: Foundational + US1 (chart shape only — verifiable from the moment the chart deploys).

### Within Each User Story

- Tests written first; expected to FAIL at first run, then PASS after implementation. Per user instruction, every implementation task is followed by an E2E gate at the user-story boundary on cf1z.
- Pure functions before side-effecting ones (`should_overwrite` before `nhd_writer.update_status`; `build_user_prompt` before `case_worker.run_case`).
- Helm chart pieces lift before the Deployment template references them.
- Wire `app.py` last (T037).

### Parallel Opportunities

- **Phase 1 setup**: T006–T009 run in parallel after T002 lands (Dockerfile, Makefile, CI workflow, runbook v0 are independent files).
- **Phase 2 foundational**: T011 (logging) and T013 (logging tests) can parallel T010+T012 (config). T015 (NHD writer) and T017 (its tests) can parallel T014 (kube client) once T014's signatures are stable. T019 (case-table tests), T020 (runbook lint), T021 (Chart.yaml + values), T023 (RBAC + Deployment templates), T024 (per-cloud values), T025 (lint Job) all parallelize after T018 (case table) and T022 (helpers.tpl) land.
- **Phase 3 US1 tests**: T028–T032 are five different fixtures/tests in five different files — fully parallelizable. T033 (diagnose handler), T034 (health), T035 (model resolver) can land in parallel (different files); T036 (stub worker) and T037 (app factory) need T033–T035 done.
- **Phase 3 US1 cf1z gates**: T042–T046 are independent quickstart scenarios — can run in parallel by different operators (or sequentially by one).
- **Phase 4 US2 tests**: T048–T051 four different test files — fully parallelizable.
- **Phase 5 US3, Phase 6 US4, Phase 7 US5**: All three can run in parallel after Phase 4 lands (different files, independent ACs).
- **Phase 8 polish**: T077, T078, T079 are three different files; T080 needs an image build present; T081 (full quickstart) runs once everything else is done.

---

## Parallel Example: User Story 1 implementation

```text
# Day 1 AM, after Foundational lands and US1 tests are written and red:

# Developer A — pure / library code:
T035 nodemedic_agent/runner/model_resolver.py
T036 nodemedic_agent/runner/case_worker.py (stub variant)

# Developer B — HTTP routing:
T033 nodemedic_agent/api/diagnose.py
T034 nodemedic_agent/api/health.py

# Both converge on:
T037 nodemedic_agent/app.py (wiring)

# Then sequential build/deploy:
T038 build image (Colima)
T039 apply Secrets on cf1z
T040 push image to cf-registry
T041 helm install on cf1z

# Then parallel cf1z gates:
T042 AC-2 gate    T043 AC-8 gate    T044 AC-10 gate    T045 AC-11 gate    T046 AC-15 gate

# Final convergence:
T047 controller flip off --stub-agent=true and end-to-end stub roundtrip
```

---

## Implementation Strategy

### MVP First (US1 only) → Day 1 AM checkpoint (Constitution II.3 milestone)

1. **Phase 1 (Setup)** — `pyproject.toml` + Dockerfile + Makefile + CI + runbook v0. Time-box to 2 h.
2. **Phase 2 (Foundational)** — config, logging, kube client, NHD writer, case table, runbook lint, Helm chart skeleton. Time-box to 4 h.
3. **Phase 3 (US1 — Day-1 AM stub)** — diagnose handler + health + model resolver + stub worker + app factory + cf1z deploy + 5 quickstart gates + flip controller off `--stub-agent`. Time-box to 4 h.
4. **STOP and VALIDATE.** AC-1, AC-2, AC-8, AC-10, AC-11, AC-15 green on cf1z. Controller is exercising its gate against a real CR shape produced by a stub worker. Day-1 AM Constitution II.3 milestone hit.

### Incremental Delivery → Day 1 PM (the demo's headline win)

5. **Phase 4 (US2 — containerd demo)** — emit_report + runbook v1 + real SDK loop. Replace the stub worker, redeploy, run the chaos cronjob, watch a real diagnosis land. Time-box to a full Day-1 PM. AC-3 + AC-9 green. **STOP and DEMO** if AC-3 alone is the demo target.

### Day 2 Polish (the rest of the gate matrix)

6. **Phase 5 (US3 — kubelet + case isolation)** in parallel with **Phase 6 (US4 — failure modes)** in parallel with **Phase 7 (US5 — cross-cloud safety)** — three captains, three branches, three independent E2E gates. Time-box to half a day total.
7. **Phase 8 (Polish)** — CI invariants, optional metrics, README, sizing follow-up, captain review pass, final quickstart rehearsal. Time-box to half a day.

### Constitution checkpoints (one-line cross-references)

- **Article I.1 credential-layer least privilege** — T023 (`ClusterRole` template), T039 (per-cluster Secret apply), T083 (captain chart review). Programmatic verification deferred per FR-13.
- **Article I.2 cordon-only** — T023 (no `nodes/patch`), T077 (CI grep test).
- **Article I.3 confidence gate binding** — T053 (`emit_report` schema accepts single-evidence), T070 (unit test confirms thin acceptance), T073 (cf1z gate confirms controller-side routing).
- **Article I.4 every tool call observable** — T056 (hook), T077 (CI grep test).
- **Article I.5 non-production clusters only** — T010 (config validator), T022 (Helm helper guard), T026 (chart render test).
- **Article II.1 two cross-scope contracts only** — T028 (POST /diagnose golden bodies match controller's), T048 (emit_report golden matches contract).
- **Article II.3 stub before integrate** — Phase 3 entire user story is the stub deliverable. T047 (controller flip-off gate) closes the milestone.
- **Article II.7 one binary, two clouds** — T024 (per-cloud values), T075 (chart credential isolation), T076 (cf1z cross-cloud probe gate).

---

## Notes

- [P] tasks = different files, no dependencies on incomplete tasks.
- [Story] tag traces every implementation task back to a user story and an AC in spec §8.
- Per user instruction: **every task carries unit tests adjacent to (or before) the implementation, and every user story closes with cf1z E2E gates against real components before the next phase begins.** Don't batch tests across phases.
- Verify tests fail before implementing (TDD spirit; the spec has hard-fact expectations on FR-1 / FR-8 / FR-10 / FR-12).
- Commit after each task or logical group; squash inside a PR if the chain is dense.
- Do not begin US-tagged tasks until Phase 2 + T027 (the cf1z dry-run gate) is green.
- **NHD lookup path** (data-model.md §1.4): tasks above default to receiving optional `nhdName` in the `POST /diagnose` body AND implementing the list+filter fallback in `nhd_writer.find_by_case_id` (T015) so the agent works whether or not Spec 001 sends `nhdName`. Coordinate with Spec 001's captain in T028 (golden body authorship) — if Spec 001 commits to sending `nhdName`, drop the fallback before US2 lands; otherwise leave both paths in.
- **Empirical resource sizing**: T063 captures the first measurement after Day-1 PM's containerd demo. T082 acts on the data — don't tune NFR-8 limits before that data point exists.
- **Runbook authoring sequencing**: v0 placeholder ships in T009 (Phase 1) so the lint passes structurally; v1 with the `## ContainerRuntimeUnhealthy` decision tree ships in T052 (US2) before the redeploy in T060; `## KubeletUnhealthy` content lands in T066 (US3). The build-time lint is enforced from T020 onward and the install-time lint is enforced from T025 onward.
- **Image build is manual** for the hackathon. CI verifies tests + lint; pushing to cf-registry happens from a developer laptop with Colima + email-as-username creds (verified empirically 2026-06-14 on the controller image).
- If a captain pivots away from this plan mid-hackathon, raise it (Constitution III.5) — don't silently re-architect.
