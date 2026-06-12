---
description: "Task list for NodeMedic Controller (Scope 2 of AFA 2026 hackathon)"
---

# Tasks: NodeMedic Controller

**Input**: Design documents from `.specify/specs/001-nodemedic-controller/`

**Prerequisites**: [`plan.md`](./plan.md), [`spec.md`](./spec.md), [`research.md`](./research.md), [`data-model.md`](./data-model.md), [`contracts/`](./contracts/), [`quickstart.md`](./quickstart.md), [`constitution.md`](../../memory/constitution.md)

**Tests**: Spec FR-2 mandates fixture-driven unit tests for the providerID parser, FR-6 mandates a unit-tested confidence gate, and §8 acceptance criteria require envtest-backed reconciler tests. Tests are in scope and called out per phase below.

**Organization**: Tasks are grouped by user story (mapped from spec §4 walkthroughs + §8 acceptance criteria). Each user story is an independently testable demo increment.

**Working tree**: All paths are repo-relative. The controller code lives **in this repo** alongside the NPD fork (decision 2026-06-12: no separate `nodemedic-controller` repo). Work happens on a new branch `hackathon-2026/scope2-controller` cut from `hackathon-2026/cf1z-baseline`. Spec-kit artifacts stay at `.specify/specs/001-nodemedic-controller/`. Controller code is namespaced under `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/...`, `config/nodemedic/...`, `deployment/helm/nodemedic-controller/...`, and `test/nodemedic/...` so it doesn't collide with NPD's existing tree.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: Maps task to a user story (US1, US2, US3, US4, US5)
- File paths are exact; `<controller-repo>` is shorthand for the controller repo root

## User stories (derived from spec §4 + §8)

| ID | Title | Priority | Acceptance criteria |
|---|---|---|---|
| US1 | Operator runs the happy-path demo: auto-cordon on high-confidence diagnosis | **P1 (MVP)** | AC-1, AC-2, AC-3, AC-6 (debounce), AC-9 (restart), AC-10 (metrics) |
| US2 | Operator sees HumanInLoop on gate failure (no cordon) | P2 | AC-4 |
| US3 | Operator sees Critical Slack on agent failure (no cordon, with retry) | P2 | AC-5 |
| US4 | Same controller runs unchanged on Azure kubeadm test cluster | P3 | AC-8 |
| US5 | Operator sees `ConditionCleared` Event on self-heal (no auto-uncordon) | P3 | AC-7 |

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Bootstrap the controller workspace **inside this repo**, scoped under `cmd/nodemedic-controller/`, `api/`, `internal/nodemedic/`, `config/nodemedic/`, `deployment/helm/nodemedic-controller/`, and `test/nodemedic/` so we don't disturb the NPD tree. No controller behavior yet.

- [x] T001 Create branch `hackathon-2026/scope2-controller` off `hackathon-2026/cf1z-baseline`; switch the working tree to it
- [x] T002 Hand-scaffold the directory tree (kubebuilder init refuses on a non-empty repo): `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/{controller,agentclient,notifier,metrics,providerid}/`, `config/nodemedic/{crd,rbac}/`, `deployment/helm/nodemedic-controller/templates/`, `test/nodemedic/{envtest,fixtures}/`. Each new package also got a `doc.go` placeholder so `go test ./...` exits 0 cleanly until Phase 2 lands real code.
- [x] T003 Defer `go get sigs.k8s.io/controller-runtime` and `controller-tools` to Phase 2 (T010) — Makefile uses `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5` and `go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19` so deps materialize on first use. `vendor/` will regrow on Phase 2 `go mod vendor`.
- [x] T004 [P] Verified existing `.golangci.yml` lints controller paths (no path filter beyond `vendor/`/`third_party/`); `exclusions.generated: lax` already auto-excludes `zz_generated.deepcopy.go` — no edit needed.
- [x] T005 [P] Added `Dockerfile.nodemedic-controller` — multi-stage `golang:1.25.9-bookworm` builder for `cmd/nodemedic-controller/`, distroless static runtime, multi-arch (`linux/amd64,linux/arm64`). NPD `Dockerfile` untouched.
- [x] T006 [P] Extended top-level `Makefile` with `nodemedic-help`, `nodemedic-build`, `nodemedic-generate`, `nodemedic-manifests`, `nodemedic-test`, `nodemedic-envtest`, `nodemedic-docker-build`, `nodemedic-helm-lint`, `nodemedic-helm-package`, `nodemedic-clean`. NPD's targets verified intact via `make -n test` and `make -n bin/node-problem-detector`.
- [x] T007 [P] Added `hack/boilerplate.go.txt` (Apache 2.0 header for `controller-gen`).
- [x] T008 [P] Added `.github/workflows/nodemedic-ci.yml` — path-filtered to controller files; runs `make nodemedic-test` + `make nodemedic-helm-lint` (helm-lint gracefully skips if `templates/` is still empty).
- [x] T009 Wired `setup-envtest` into `nodemedic-envtest` via `go run`; pinned `ENVTEST_K8S_VERSION=1.31.0`.

**Checkpoint**: `make nodemedic-build` is wired (will fail until Phase 2 puts source under `cmd/nodemedic-controller/`); `make nodemedic-test` runs (no tests yet — exits 0); existing `make test` and NPD build targets still pass.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Manager scaffolding, CRD types, RBAC, Helm skeleton, and shared fixtures. **No user-story behavior yet.** Everything below blocks every user story.

**⚠️ CRITICAL**: No US-tagged task may begin until this phase is complete.

### CRD types (vendored from contracts/nhd-crd.yaml)

- [x] T010 Authored `api/v1alpha1/groupversion_info.go` — uses `runtime.NewSchemeBuilder` (apimachinery) rather than controller-runtime's `scheme.Builder` so the package compiles standalone.
- [x] T011 Authored `api/v1alpha1/nodehealthdiagnosisai_types.go` matching [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml): `CaseSpec` / `TriggerSpec` / `BudgetsSpec`, `NodeHealthDiagnosisAIStatus` with `Phase` / `Conditions` / `Diagnosis` / `Evaluation` / `Action`, plus enum types for `Provider`/`EvidenceSource`/`RecommendationAction`/`RCACategory`/`ActionDecision`/`ActionOperation`. Compile-time `_ runtime.Object` assertions break the build if deepcopy drifts.
- [x] T012 `make nodemedic-generate` → `api/v1alpha1/zz_generated.deepcopy.go` (358 lines, checked in).
- [x] T013 `make nodemedic-manifests` → `config/nodemedic/crd/nodemedic.cf.newrelic.com_nodehealthdiagnosisais.yaml`. Required `crd:allowDangerousTypes=true` because `confidence` is `type: number` (the contract is the authority — Constitution II.2). Schema verified structurally: enums, ranges (`confidence` 0–1), patterns (UUIDv4, decimal-as-string), required fields, status subresource, all 6 printer columns. Defaults made it through (`maxTurns: 15`, `maxBudgetUSD: "0.50"`). The Makefile target now also copies the CRD into `deployment/helm/nodemedic-controller/files/crd/` so `helm install` ships exactly what `controller-gen` produced.

### Manager scaffolding

- [x] T014 Authored `cmd/nodemedic-controller/main.go` — flag set per spec NFR-6 with defaults (`--watched-conditions` covers all 7 fault classes, `--min-confidence=0.7`, `--min-evidence-sources=2`, `--debounce-window=30s`), zap-logr setup, `ctrl.NewManager` on `:9443` (metrics) + `:8081` (probes), **no leader election** (research R-7). Cluster-name guard rejects empty + non-`test-*` (Constitution I.9, binary-side belt + suspenders). Smoke-tested both rejection paths via `/tmp/nmc-test`. Reconciler wiring deferred to Phase 3 T044.
- [x] T015 [P] Authored `internal/nodemedic/metrics/metrics.go` — all 5 NFR-3 collectors (`nodemedic_cases_total`, `nodemedic_phase_duration_seconds`, `nodemedic_agent_post_total`, `nodemedic_cordon_total`, `nodemedic_slack_post_total`) registered against `ctrlmetrics.Registry`. Label-value constants exported so reconciler code in Phase 3 doesn't have to hard-code strings.
- [x] T016 [P] Authored `internal/nodemedic/controller/types.go` — `Case`, `Decisions`, `GateOutcome`/`GateResult`, `CordonResult`, `SlackResult`, `DebounceKey` per [`data-model.md`](./data-model.md) §4–§6. Time injected via `Case.Now` so tests can drive deadlines deterministically.

### RBAC

- [x] T017 Authored `config/nodemedic/rbac/role.yaml` per spec NFR-4 — exactly `nhd` get/list/watch/create/update/patch (+ status get/update/patch); `nodes` get/list/watch/patch; `events` create/patch. Hand-written rather than controller-gen-marker-driven so the verb surface is one explicit file (easier to audit, harder to widen accidentally).
- [x] T018 [P] Authored `config/nodemedic/rbac/role_binding.yaml` + `service_account.yaml` (namespace `cf-monitoring`).

### Helm chart skeleton

- [x] T019 Authored `deployment/helm/nodemedic-controller/Chart.yaml` (apiVersion v2, `version: 0.1.0`, kubeVersion `>=1.28.0-0`).
- [x] T020 Authored `deployment/helm/nodemedic-controller/values.yaml` — flag defaults mirror `cmd/main.go` defaults; `installCRD: true`; secret refs (`nodemedic-agent-token`, `nodemedic-slack`); `replicaCount: 1`; resource limits.
- [x] T021 [P] Authored `templates/_helpers.tpl` with the `nodemedic.requireTestCluster` helper that calls `fail` if `.Values.clusterName` is missing or doesn't start with `test-`. Every other template includes it; helm-lint test confirmed both rejection paths fire (Constitution I.9 chart-side hard guard).
- [x] T022 [P] Authored `templates/serviceaccount.yaml`, `clusterrole.yaml` (mirrors `config/nodemedic/rbac/role.yaml` line-for-line), `clusterrolebinding.yaml`, `deployment.yaml` (one replica, distroless `runAsNonRoot:true`, `readOnlyRootFilesystem:true`, drop ALL caps, healthz/readyz probes), `service.yaml`.
- [x] T023 Authored `templates/crd.yaml` gated by `.Values.installCRD`. Loaded via `.Files.Get "files/crd/..."` rather than the special Helm `crds/` directory so the toggle works (Helm's `crds/` is auto-installed unconditionally). `helm.sh/resource-policy: keep` annotation already present in the generated CRD via the api type comments.

### Shared test fixtures

- [x] T024 Authored a **representative** `test/nodemedic/fixtures/node-eks.yaml`. Header comment flags it for replacement with a real `kubectl get node` capture on Day 1 AM. Fields cover the FR-2 resolution paths: `cf.newrelic.com/cloud-provider=aws`, `topology.kubernetes.io/region=us-east-2`, `spec.providerID=aws:///us-east-2a/i-0abc1234deadbeef`, plus a `ConntrackSaturated` condition pre-seeded at False so unit tests can flip it.
- [x] T025 [P] Authored a **representative** `test/nodemedic/fixtures/node-azure.yaml` matching cloud-provider-azure's URI shape: `spec.providerID=azure:///subscriptions/.../virtualMachines/aks-cf-test-1-vmss000004`, `topology.kubernetes.io/region=eastus2`, `cf.newrelic.com/cloud-provider=azure`. Same Day-1-AM replacement note.
- [x] T026 [P] Authored `test/nodemedic/fixtures/nhd-applied.yaml` — canned NHD with `phase=Diagnosed`, `confidence=0.85`, three distinct evidence sources (`nrql`, `ssh`, `kubectl`), `recommendation.action=Cordon`. Drives the US1 happy-path through gate-pass + cordon + Slack `Applied` without the agent being live (Constitution II.3 / research R-14 stub deliverable).

**Checkpoint**: CRD installs cleanly. Manager binary starts, registers no controllers, exposes empty `/metrics`. Helm chart renders against `--set clusterName=test-foo` and refuses with `clusterName=stg-foo`. Day-1-AM milestone for Constitution II.3 is **structurally** ready (we still need at least US1's reconciler scaffolding to be a meaningful stub — completes inside US1).

---

## Phase 3: User Story 1 — Auto-cordon on high-confidence diagnosis (Priority: P1) 🎯 MVP

**Goal**: NPD flips a watched `NodeCondition` to `True` → controller creates an NHD with cluster metadata → calls the agent → on `Diagnosed` runs the confidence gate → on pass cordons the Node and posts an `Applied` Slack message. End-to-end on `test-odd-wire` (EKS).

**Independent Test**: Run [`quickstart.md`](./quickstart.md) Scenario A on `test-odd-wire` with the canned-diagnosis stub from T026. Verify AC-1, AC-2, AC-3, AC-6, AC-9, AC-10. The whole demo's win moment lives here.

### Tests for User Story 1 (FR-2 + FR-6 + AC-1..3 mandate them)

- [ ] T027 [P] [US1] Author `internal/nodemedic/providerid/parse_test.go` — table-driven against `test/nodemedic/fixtures/node-eks.yaml`; cases: valid AWS providerID (extracts `i-...`), missing providerID (returns error), unknown scheme, malformed segments (FR-2 row 3)
- [ ] T028 [P] [US1] Author `internal/nodemedic/controller/confidence_gate_test.go` — table-driven covering each clause of FR-6 / Constitution I.3 independently: `confidence < 0.7` alone, `< 2 distinct sources` alone, `action=NoAction` alone, all pass, malformed confidence
- [ ] T029 [P] [US1] Author `internal/nodemedic/controller/nameformat_test.go` — `nameForCase(node, observedAt)` produces `<node-truncated-50>-<unix-ts>`, deterministic for same inputs, sanitizes invalid chars (data-model §1.5)
- [ ] T030 [P] [US1] Author `internal/nodemedic/agentclient/client_test.go` — golden-file test against [`contracts/post-diagnose.md`](./contracts/post-diagnose.md) "Golden request body"; `httptest.Server` returning 202 → expects phase Diagnosing; 429 retried 3× then surfaces error; per-attempt timeout 1.5s honored
- [ ] T031 [P] [US1] Author `internal/nodemedic/notifier/slack_test.go` — golden JSON for `BuildApplied(...)` matches data-model §8 envelope; `httptest.Server` retry test for 1/2/4s backoff; final-failure path returns error without panic
- [ ] T032 [US1] Author `test/nodemedic/envtest/reconciler_us1_test.go` — full happy-path: create Node (from fixture), apply NHD with `phase=Diagnosed` and canned diagnosis, run reconciler, assert `Node.spec.unschedulable=true`, NHD `phase=Acted`, `decision=Applied`, mocked Slack got one POST

### Implementation for User Story 1

- [ ] T033 [P] [US1] Implement `internal/nodemedic/providerid/parse.go` — `ParseAWS(providerID string) (instanceId string, err error)` only (Azure deferred to US4); URI scheme dispatch returns error for `azure://`
- [ ] T034 [P] [US1] Implement `internal/nodemedic/controller/confidence_gate.go` — pure `ConfidenceGate(d, minConfidence, minSources) (GateResult, error)` per data-model §5; no I/O, no clock
- [ ] T035 [P] [US1] Implement `internal/nodemedic/controller/nameformat.go` — `NameForCase(node, observedAt time.Time) string`
- [ ] T036 [P] [US1] Implement `internal/nodemedic/controller/debounce.go` — `DebounceMap` per data-model §6; no goroutines (lazy GC on `Allow`)
- [ ] T037 [P] [US1] Implement `internal/nodemedic/agentclient/types.go` + `internal/nodemedic/agentclient/client.go` — `Client.Diagnose(ctx, req) (*DiagnoseResponse, error)`; retry 1/2/4s on 429+5xx+timeout, no retry on 400/401, per-attempt 1.5s timeout, bearer token from env
- [ ] T038 [P] [US1] Implement `internal/nodemedic/notifier/slack.go` — `Client.Post(ctx, blocks []byte) error` with 1/2/4s backoff and 5s per-attempt; webhook URL from env
- [ ] T039 [P] [US1] Implement `internal/nodemedic/notifier/messages.go` — `BuildApplied(nhd) []byte` only (HumanInLoop + Critical deferred to US2/US3); fenced `kubectl get nhd` block per research R-6
- [ ] T040 [US1] Implement `internal/nodemedic/controller/cordon.go` — `Cordon(ctx, c client.Client, node *corev1.Node) error` patches `spec.unschedulable=true` via `client.MergeFrom`, idempotent if already true; refuses if `node.Name != nhd.Spec.Case.NodeName` (data-model §2.4)
- [ ] T041 [US1] Implement `internal/nodemedic/controller/nodewatcher.go` — controller-runtime `source.Kind` on `corev1.Node`; predicate filters per FR-1 (allowlisted Condition type, transition to True, `--debounce-window` not exceeded for the (node, type) key); on match, enqueue a `Trigger` event for the case_creator
- [ ] T042 [US1] Implement `internal/nodemedic/controller/case_creator.go` — `CreateCaseFor(ctx, trigger Trigger) error`: resolves provider/region/instanceId per FR-2 (using providerid.ParseAWS), builds NHD with `caseId=uuid.NewString()`, computes deterministic name, `Create` with `IgnoreAlreadyExists`, emits `MetadataResolutionFailed` Event on failure (FR-2 last paragraph)
- [ ] T043 [US1] Implement `internal/nodemedic/controller/nhd_reconciler.go` — controller-runtime reconciler keyed on NHD; phase-machine arms for `""→Diagnosing` (calls agent client) and `Diagnosed→Acted` (calls gate, on pass calls cordon + Applied Slack); other arms TODO until US2/US3; emits `PhaseTransition` events; updates `status.conditions[]` per data-model §1.4
- [ ] T044 [US1] Wire reconciler + nodewatcher into manager from T014 (`main.go`); register metrics counters at startup
- [ ] T045 [P] [US1] Author `deployment/helm/nodemedic-controller/values-eks.yaml` — `clusterName: test-odd-wire`, `agentUrl: http://nodemedic-agent.cf-monitoring.svc:8080/diagnose`, watchedConditions list per FR-1
- [ ] T046 [US1] Update `README.md` quickstart section — link to [`quickstart.md`](./quickstart.md), document `make docker-build` + `helm install` flow per research R-3
- [ ] T047 [US1] Run [`quickstart.md`](./quickstart.md) Scenario A end-to-end on `test-odd-wire` against the T026 canned NHD; capture `kubectl get nhd -o yaml` output and `kubectl describe node` showing `SchedulingDisabled` as artifacts; verify AC-1, AC-2, AC-3
- [ ] T048 [US1] Run quickstart Scenario D (debounce, AC-6) and Scenario G (restart, AC-9) on the same cluster
- [ ] T049 [US1] Verify quickstart Scenario H (metrics, AC-10) — `nodemedic_cases_total{outcome="Applied"} >= 1`

**Checkpoint**: AC-1, AC-2, AC-3, AC-6, AC-9, AC-10 all green on `test-odd-wire`. Demo's win moment is reproducible. The Day-1-AM stub commitment (Constitution II.3 / research R-14) is satisfied because anyone can `kubectl apply -f nhd-applied.yaml` and watch the controller cordon + Slack.

---

## Phase 4: User Story 2 — HumanInLoop on gate failure (Priority: P2)

**Goal**: When the diagnosis comes back below the confidence gate (or with `action=NoAction`), the controller does NOT cordon, sets `decision=HumanInLoop`, and posts a Slack message with "needs human review" framing. AC-4.

**Independent Test**: [`quickstart.md`](./quickstart.md) Scenario B — hand-edit a stub NHD's `status.diagnosis.confidence=0.5`, watch the controller route to HumanInLoop without touching the Node.

### Tests for User Story 2

- [ ] T050 [P] [US2] Extend `internal/nodemedic/notifier/slack_test.go` — golden JSON for `BuildHumanInLoop(...)` matches data-model §8 (red/critical framing, "needs human review" header)
- [ ] T051 [P] [US2] Author `test/nodemedic/envtest/reconciler_us2_test.go` — apply NHD with `phase=Diagnosed`, `confidence=0.5`, `evidence=[nrql, ssh]`, `recommendation.action=Cordon`. Assert `Node.spec.unschedulable` is **never** patched, NHD `decision=HumanInLoop`, mocked Slack received one POST whose payload matches the HumanInLoop golden

### Implementation for User Story 2

- [ ] T052 [US2] Author `test/nodemedic/fixtures/nhd-humaninloop.yaml` — canned NHD: `confidence=0.5, evidence=[{source:nrql},{source:ssh}], recommendation.action=Cordon`
- [ ] T053 [US2] Add `BuildHumanInLoop(nhd, gateReason) []byte` to `internal/nodemedic/notifier/messages.go` — distinct color/severity, mirrors Applied body otherwise
- [ ] T054 [US2] Extend `internal/nodemedic/controller/nhd_reconciler.go` `Diagnosed→Acted` arm — on gate fail: do NOT call cordon, set `decision=HumanInLoop`, call `Slack.Post(BuildHumanInLoop(...))`, emit `ActionApplied` Condition with `reason=HumanInLoop`
- [ ] T055 [US2] Run [`quickstart.md`](./quickstart.md) Scenario B on `test-odd-wire` against T052 fixture; verify AC-4

**Checkpoint**: AC-4 green. Gate-fail demo path works.

---

## Phase 5: User Story 3 — Critical Slack on agent failure (Priority: P2)

**Goal**: When the agent never writes `phase=Diagnosed` before `spec.budgets.deadline`, the controller marks `Failed{reason=DeadlineExceeded}`, retries `POST /diagnose` once with the same `caseId` (FR-7), and on second timeout posts a `Critical` Slack message. The Node is never cordoned. AC-5.

**Independent Test**: [`quickstart.md`](./quickstart.md) Scenario C — point `--agent-url` at a black-hole endpoint, inject a fault, observe terminal `Failed` and one Critical Slack message.

### Tests for User Story 3

- [ ] T056 [P] [US3] Extend `internal/nodemedic/notifier/slack_test.go` — golden JSON for `BuildCritical(...)` includes `reason` and "AGENT FAILED" header
- [ ] T057 [P] [US3] Author `test/nodemedic/envtest/reconciler_us3_test.go` — agent stub returns 202 then never updates the CR; envtest fast-forwards `time.Now` past deadline; assert: first `Failed{DeadlineExceeded}`, retry, second `Failed`, exactly one Critical Slack POST, `Node.spec.unschedulable` never set

### Implementation for User Story 3

- [ ] T058 [US3] Add `BuildCritical(nhd, failureReason) []byte` to `internal/nodemedic/notifier/messages.go`
- [ ] T059 [US3] Extend `internal/nodemedic/controller/nhd_reconciler.go` `Diagnosing` arm — when `now > spec.budgets.deadline`: set `Failed{reason=DeadlineExceeded}`, increment a per-NHD retry annotation (`nodemedic.cf.newrelic.com/retry-count`); if annotation < 1, re-issue `POST /diagnose` with the same `caseId` and bump annotation; if = 1, set terminal `Failed` and call `Slack.Post(BuildCritical(...))`
- [ ] T060 [US3] Add per-NHD `retry-count` annotation handling to status updater so it survives controller restarts (FR-11 idempotency edge)
- [ ] T061 [US3] Add a small black-hole HTTP service definition to `test/nodemedic/fixtures/blackhole-deployment.yaml` (returns `202` then sleeps); referenced by quickstart Scenario C
- [ ] T062 [US3] Run [`quickstart.md`](./quickstart.md) Scenario C on `test-odd-wire`; verify AC-5

**Checkpoint**: AC-5 green. Agent-failure demo path works.

---

## Phase 6: User Story 4 — Azure kubeadm parity (Priority: P3)

**Goal**: Same Helm chart, same controller binary, same CRD; only `--cluster-name`, kubeconfig context, and Slack webhook differ. Constitution Article II.7. AC-8.

**Independent Test**: [`quickstart.md`](./quickstart.md) Scenario F — repeat Scenarios A, B, C on the Azure kubeadm test cluster.

### Tests for User Story 4

- [ ] T063 [P] [US4] Extend `internal/nodemedic/providerid/parse_test.go` — fixture-driven cases against `test/nodemedic/fixtures/node-azure.yaml`: valid Azure providerID extracts VM name, malformed Azure URI errors out, multi-segment paths handled correctly
- [ ] T064 [P] [US4] Add envtest case in `test/nodemedic/envtest/reconciler_us1_test.go` parameterized over (eks, azure) fixtures — same assertions, different Node yaml

### Implementation for User Story 4

- [ ] T065 [US4] Implement `ParseAzure(providerID string) (vmName string, err error)` in `internal/nodemedic/providerid/parse.go`; update the URI-scheme dispatcher to route to ParseAzure for `azure://`
- [ ] T066 [US4] [P] Author `deployment/helm/nodemedic-controller/values-azure.yaml` — `clusterName: cf1z` (the legacy CF Azure kubeadm test cluster, per Constitution Article I.9), watchedConditions list, agentUrl
- [ ] T067 [US4] Run [`quickstart.md`](./quickstart.md) Scenario F end-to-end on the Azure test cluster; capture `kubectl get nhd -o yaml` showing `provider=azure`, region resolved from Azure cloud-controller-manager labels, instanceId = VM name; verify AC-8

**Checkpoint**: AC-8 green. One binary, two clouds (Constitution II.7) is demonstrated.

---

## Phase 7: User Story 5 — Self-heal observability (Priority: P3)

**Goal**: When a watched NodeCondition flips True→False on a node with an `Acted` NHD, the controller emits a `ConditionCleared` Event on the Node. It does **not** uncordon, does **not** mutate the NHD. FR-10. AC-7.

**Independent Test**: [`quickstart.md`](./quickstart.md) Scenario E.

### Tests for User Story 5

- [ ] T068 [P] [US5] Author `test/nodemedic/envtest/reconciler_us5_test.go` — Node already cordoned, Acted NHD exists; flip Condition True→False; assert: ConditionCleared Event on Node, `Node.spec.unschedulable` STILL true, NHD unchanged

### Implementation for User Story 5

- [ ] T069 [US5] Extend `internal/nodemedic/controller/nodewatcher.go` predicate — on True→False transition for a watched condition: look up matching `Acted` NHD by `(node, type)`; if found, emit `ConditionCleared` Event on Node referencing the NHD name; do NOT enqueue any reconcile work
- [ ] T070 [US5] Run [`quickstart.md`](./quickstart.md) Scenario E on `test-odd-wire`; verify AC-7

**Checkpoint**: AC-7 green. Operator visibility into self-heal is in place; auto-uncordon is correctly NOT happening.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Hardening, audit-friendly invariants, demo-ready packaging. Most of these don't add features; they protect the constitution.

- [ ] T071 [P] Add CI grep test in `.github/workflows/ci.yml` — fails if `internal/` ever contains `claude_agent`, `anthropic`, `mcp_servers` (Constitution II.5 invariant)
- [ ] T072 [P] Add CI grep test — fails if any `client.Update` or `client.Patch` outside `cordon.go` references `corev1.Node` (Constitution I.2 invariant)
- [ ] T073 [P] Helm-lint matrix in CI: `helm template --set clusterName=test-foo` succeeds; `helm template --set clusterName=stg-foo` fails with the cluster-name guard
- [ ] T074 [P] Run `Makefile` `make nodemedic-helm-package` to produce `deployment/helm/nodemedic-controller-0.1.0.tgz`; commit alongside the chart (research R-3)
- [ ] T075 [P] Verify NFR-3 metrics coverage by running [`quickstart.md`](./quickstart.md) Scenarios A+B+C and asserting all 5 metric series have at least one non-zero label combination
- [ ] T076 [P] Sweep all `phase` transitions in `nhd_reconciler.go` to ensure every transition adds a `status.conditions[]` entry with `lastTransitionTime`, `reason`, and a non-empty `message` (NFR-7 audit trail)
- [ ] T077 [P] Author a 1-page Slack/PR message summarizing the Day-1 AM stub deliverable for Scope 1 + Scope 3 captains (Constitution II.3 / III.2) — link to `nhd-applied.yaml` + `nhd-humaninloop.yaml`
- [ ] T078 Run the **full** [`quickstart.md`](./quickstart.md) (Scenarios A→H) on `test-odd-wire` and on the Azure test cluster as the final pre-demo rehearsal; record screencaps for the captain review
- [ ] T079 Update [`spec.md`](./spec.md) §10 — close the five open questions (already resolved in [`research.md`](./research.md)); replace the question list with a "Decisions" pointer to research

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: No deps; start immediately.
- **Phase 2 (Foundational)**: Depends on Phase 1 complete. **Blocks every user story.**
- **Phase 3 (US1)**: Depends on Phase 2. P1 / MVP — start first.
- **Phase 4 (US2)**: Depends on Phase 2. Adds the gate-fail arm + HumanInLoop builder; touches `nhd_reconciler.go` (conflict with US1) and `messages.go` (conflict with US1's `BuildApplied`). **Sequence after US1's reconciler skeleton lands**, or coordinate via small PR per US1 → US2 chain.
- **Phase 5 (US3)**: Depends on Phase 2. Touches `nhd_reconciler.go` and `messages.go` like US2. **Can run in parallel with US2 by different developers if PRs are coordinated** (separate arms of the phase machine).
- **Phase 6 (US4)**: Depends on Phase 3 (US1). Adds Azure providerID branch — file conflicts with US1's `parse.go`. **Sequence after US1.**
- **Phase 7 (US5)**: Depends on Phase 3 (US1). Touches `nodewatcher.go`. Can run in parallel with US4.
- **Phase 8 (Polish)**: Depends on **all** desired user stories.

### User Story Dependencies

- **US1 (P1, MVP)**: Foundational only. The reconciler skeleton lands in this phase; US2/US3 extend it.
- **US2 (P2)**: Foundational + US1 reconciler skeleton (extends `Diagnosed→Acted` arm). Independently testable: a P=0.5 NHD doesn't need US3 or US4 to exist.
- **US3 (P2)**: Foundational + US1 reconciler skeleton (extends `Diagnosing` arm). Independently testable: a black-hole agent doesn't need US2 to exist.
- **US4 (P3)**: Foundational + US1 (extends providerID dispatcher). Independently testable on its own cluster.
- **US5 (P3)**: Foundational + US1 (extends nodewatcher). Independently testable: cordon + flap.

### Within Each User Story

- Tests written first; expected to FAIL at first run, then PASS after implementation.
- Pure functions before side-effecting ones (`confidence_gate.go` before `nhd_reconciler.go`).
- Wire into `main.go` last (T044).

### Parallel opportunities

- **Phase 1 setup**: T004–T008 run in parallel after T002 lands (Dockerfile, Makefile, lint, CI, boilerplate are independent files).
- **Phase 2 foundational**: T015 (metrics), T016 (types), T018 (rolebinding/SA), T021 (helpers.tpl), T022 (deployment templates), T024–T026 (fixtures) can run in parallel after CRD types (T010–T013) land.
- **Phase 3 US1 tests**: T027–T031 run in parallel; T032 needs the implementations.
- **Phase 3 US1 implementations**: T033–T039 are 7 different files with no inter-deps — fully parallelizable; T040 (cordon) depends on T033 for providerID parsing; T041–T043 (watcher / case_creator / reconciler) depend on T033–T039; T044 wires everything; T045 is independent.
- **Phase 4 + Phase 5**: Two developers can work US2 and US3 in parallel; coordinate around `messages.go` and `nhd_reconciler.go` via small atomic PRs.
- **Phase 6 + Phase 7**: Independent of each other; can run in parallel by two developers.
- **Phase 8 Polish**: T071–T077 are all independent files; T078 sequences last.

---

## Parallel Example: User Story 1 implementation

```text
# Day 1 PM, after Foundational lands and US1 tests are written and red:

# Developer A — pure / library code (no kube I/O):
T033 internal/nodemedic/providerid/parse.go
T034 internal/nodemedic/controller/confidence_gate.go
T035 internal/nodemedic/controller/nameformat.go
T036 internal/nodemedic/controller/debounce.go

# Developer B — HTTP clients (no kube I/O):
T037 internal/nodemedic/agentclient/{client.go,types.go}
T038 internal/nodemedic/notifier/slack.go
T039 internal/nodemedic/notifier/messages.go (BuildApplied only)

# Developer C — controller wiring (after A+B):
T040 internal/nodemedic/controller/cordon.go
T041 internal/nodemedic/controller/nodewatcher.go
T042 internal/nodemedic/controller/case_creator.go
T043 internal/nodemedic/controller/nhd_reconciler.go
T044 cmd/nodemedic-controller/main.go wiring

# Developer A or B (in parallel with C):
T045 deployment/helm/nodemedic-controller/values-eks.yaml
```

---

## Implementation Strategy

### MVP First (US1 only) → Day 1 PM checkpoint

1. **Phase 1 (Setup)** — kubebuilder bootstrap, Makefile, Dockerfile. Time-box to 2 h.
2. **Phase 2 (Foundational)** — CRD types, manager scaffolding, Helm skeleton, fixtures. Time-box to 4 h.
3. **Phase 3 (US1)** — happy-path demo. Tests first, then 7-way parallel implementation, then wire + run quickstart Scenario A on `test-odd-wire`. Time-box to ~1 day with 2 devs.
4. **STOP. Validate.** AC-1, AC-2, AC-3, AC-6, AC-9, AC-10 must be green. Demo this even if no other story ships.

### Incremental delivery → Day 2 AM/PM

5. **Phase 4 (US2)** + **Phase 5 (US3)** in parallel — two devs, two atomic PRs, ~half a day each. Adds the gate-fail and agent-fail demo arms. AC-4, AC-5 green.
6. **Phase 6 (US4)** Azure parity — half a day; needs the Azure test cluster reachable. AC-8 green.
7. **Phase 7 (US5)** Self-heal observability — small, ~2 h. AC-7 green.
8. **Phase 8 (Polish)** — CI invariants, helm package, full quickstart rehearsal, captain review. Half a day.

### Parallel Team Strategy (2 captains)

- **Harrison + Shabeeb** complete Phase 1 + Phase 2 together. After foundational lands:
  - Harrison: US1 reconciler + cordon (T040–T044) + US3 (FR-7 retry path)
  - Shabeeb: US1 client/notifier/gate (T033–T039) + US2 (HumanInLoop)
  - Whoever finishes first picks up US4 (Azure) → US5 (self-heal) → Polish

### Constitution checkpoints sprinkled through the plan

- **Article I.9 hard guard** — T021 (Helm `_helpers.tpl` cluster-name guard), T014 (controller startup guard).
- **Article II.3 stub before integrate** — T026 + T052 + the US1 checkpoint give Scopes 1/3 something to integrate against by Day 1 noon.
- **Article II.5 controller-no-MCP** — T071 CI grep test.
- **Article I.2 cordon-only** — T072 CI grep test.
- **Article I.3 confidence gate binding** — T028 (table-driven gate test) + T034 (pure function).

---

## Notes

- [P] tasks = different files, no dependencies on incomplete tasks.
- [Story] tag traces every implementation task back to a user story and an AC in spec §8.
- Verify tests fail before implementing (TDD spirit; the spec has hard-fact expectations on FR-2 / FR-6).
- Commit after each task or logical group; squash inside a PR if the chain is dense.
- Do not begin US-tagged tasks until Phase 2 is fully checked.
- If a captain pivots away from this plan mid-hackathon, raise it (Constitution III.5) — don't silently re-architect.
