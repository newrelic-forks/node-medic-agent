# NodeMedic — AFA 2026 hackathon page entry

**Team:** Caramelizing Frolickers (Craig Fullen · Sachin Shankar · Suhas Shah · Harrison Latimer)
**Source:** [hackathon.service.nr-ops.net/teams/caramelizing-frolickers](https://hackathon.service.nr-ops.net/teams/caramelizing-frolickers)

Paste-ready: each `##` block maps 1:1 to a textarea on the team page.

---

## Problem — *What problem are you solving?*

Container Fabric runs **30,000–40,000 worker nodes across AWS EKS and Azure kubeadm**, lifecycle-managed by a layered stack that obscures failure: CAPI with the AWS (CAPA) and Azure (CAPZ) providers, Karpenter and MachinePools for capacity, ArgoCD + argo-webhook for delivery, MLC (machine-lifecycle-controller) for cordon-driven rotation. Nodes go bad constantly across orthogonal modalities: ENI / subnet / IP exhaustion, conntrack saturation, PID/FD exhaustion, inode and disk fill, kubelet and containerd hangs (PLEG, healthz blackholes), kernel panics and OOM, IMDS throttling, clock drift, expired kubelet certs, AZ outages, noisy hypervisor neighbors, faulty AMI upgrades, scheduled cloud maintenance, spot/preemptible terminations.

The cruel part: **most failure modes break the telemetry path we'd normally use to detect them**. We run NR infra agents on every worker, but a node with broken DNS, saturated conntrack, a wedged container runtime, or a corrupted route table can't ship telemetry — so the alert that should have fired doesn't, and we learn from an application team's customer-impact ticket minutes-to-hours later.

Diagnosis is then slow and manual. Only CF has SSH, so the work can't be delegated. The on-call engineer notices, finds the right context across 300+ clusters, SSHes in *before the cloud reaper or MLC reclaims the box*, and walks dmesg / journald / kubelet / `/proc` / cloud APIs from memory. MTTR is hours; the node is often gone before we get there. CF is a single-point-of-knowledge bottleneck during every incident. This is a **DRI-class** (Do-Not-Interrupt) problem — every minute the node stays uncordoned, blast radius spreads.

---

## Why should we do this?

Every unplanned node failure cascades into customer-visible impact across every product on our K8s platform — APM, browser, infra, logs, synthetics. Each incident burns hours across multiple teams (response, RCA, customer comms) and erodes platform trust. Compressing root cause from hours to under a minute would:

1. **Shorten outage duration** — remediation begins while evidence is still on the box.
2. **Capture evidence pre-reclaim** — most failure modes get auto-erased the moment MLC or cloud auto-recovery rotates the VM.
3. **Eliminate the CF SSH bottleneck** — encode our incident playbook into a runbook + tool surface any operator can drive.
4. **Prove ROI on NR telemetry we already collect** — turn raw NRQL + logs + change events into structured, source-cited answers via the NR MCP server.
5. **Reduce blast radius automatically** — confidence-gated cordon stops the bleeding within seconds of a confident diagnosis.
6. **Keep the human in the loop deliberately, not by accident** — engineer reviews evidence and decides uncordon / drain / let MLC reclaim, all auditable, all one click.

---

## Solution — *What are we building?*

**NodeMedic is a fully implemented, end-to-end, cloud-agnostic node-health pipeline.** Deterministic detection + Kubernetes-native controller + multi-turn AI agent + automated blast-radius mitigation + human-in-the-loop on-call UI. All five pieces are spec-driven (`/speckit-specify` → `plan` → `tasks` → `implement`), code-complete, and live on the cf1z hackathon cluster.

**Pipeline (target: under 60 s from fault to cordon, then human review):**

1. **Detect (deterministic).** NPD DaemonSet + custom plugins (`check-containerd.sh`, `check-kubelet-healthz.sh`, plus the §4.2 catalog: conntrack, FD, PID, inode, disk-fill, DNS, IMDS) flip `Node.status.conditions[]` directly — no telemetry round-trip.
2. **Wake the controller (Spec 001 · Go · controller-runtime).** Watches Node + Event, debounces flapping (30 s per `(node, condition)`), resolves `provider`/`region`/`instanceId` from labels + `spec.providerID`, and creates a `NodeHealthDiagnosisAI` (NHD) custom resource — single auditable source of truth per case.
3. **Invoke the agent (async, idempotent).** Controller `POST /diagnose`; agent returns 202 immediately and queues a per-case asyncio worker. Duplicate `caseId` is a no-op.
4. **Diagnose (Spec 002 · Python · Claude Agent SDK · multi-turn).** Fresh `ClaudeSDKClient` per case (no cross-case state). Opus 4.7 primary / Sonnet 4.6 fallback via the internal `nerd-completion` gateway — no API-key plumbing. Tools: `Bash` (kubectl, ssh, aws, az, /proc) + the **public NR HTTP MCP server** (NRQL, log analysis, golden metrics, change events, entity lookup). Pre/PostToolUse hooks emit one structured stdout JSON line per tool call. The runbook is a prompt-cached system prefix; user prompts and tool results are case-local — evidence cannot leak across cases. Loop ends when the terminal `emit_report` tool fires.
5. **Hallucination guardrails (layered).** (a) `emit_report` schema-validates `confidence ∈ [0,1]`, `rcaCategory` enum, `recommendation.action` enum, every claim must cite an `evidence[]` row. (b) Controller's confidence gate enforces ≥0.7 confidence AND ≥2 distinct sources before any cordon. (c) **Read-only credentials** — agent IAM/RBAC/kube-SA cannot mutate cluster or cloud, even with raw `Bash`. (d) Raw evidence rendered on the UI for human audit.
6. **Act (controller).** Gate passes → patch `node.spec.unschedulable=true` (cordon, idempotent), stamp `status.action.decision=Applied`, post Slack. Gate fails → no cordon, `decision=HumanInLoop`. Agent timeout → `decision=Critical` after retry-once.
7. **Notify (Block Kit, severity-coded).** Severity emoji + node + cluster header, fields grid (Decision · Confidence · Trigger · rcaCategory), `attachment.color = danger | warning | grey`, primary `View full diagnosis` button deep-linking to the UI. No interactive Slack components — actions live in the UI.
8. **Human-in-the-loop (Spec 003 · Go stdlib).** Separate Deployment + Service in `cf-monitoring`. List view (24 h, auto-refreshing) + per-case detail (case metadata, diagnosis, collapsible evidence, merged action history). Three single-click buttons — **Uncordon** · **Clear MLC skipDeletion** · **Drain** — execute directly via the kube API (no extra HTTP contract). Drain streams per-pod progress over SSE. Every click is auditable in two places: structured stdout log + JSON entry on the NHD's `nodemedic.cf.newrelic.com/ui-action-history` annotation (FIFO ring buffer, last 20 entries, surface via `kubectl get nhd -o yaml`).
9. **Automated cleanup.** Once the engineer chooses Uncordon / Clear-skipDeletion, MLC reclaims the VM on its next reconcile. NodeMedic owns the *decision* and the *observability*; MLC owns disposal.

**Cloud-agnostic from the ground up.** Same image, chart, and runbook on EKS and Azure kubeadm. Only credentials change per install (AWS IRSA vs Azure workload identity). Demonstrated end-to-end on cf1z (Azure kubeadm).

---

## Scale and scope

**In scope (all implemented, live on cf1z):**

- **Spec 001 — Controller** (~5,300 LOC Go). controller-runtime manager, NHD CRD (`nodemedic.cf.newrelic.com/v1alpha1`), node + event watcher with 30 s debounce, phase machine `Pending → Diagnosing → Diagnosed → Acted` (`Failed` terminal), confidence gate, idempotent cordon, agent client (retry-once on `DeadlineExceeded` / `AgentUnreachable`), Block Kit Slack notifier (Applied / HumanInLoop / Critical). Helm chart with cluster-name guard (`cf1z` | `jc1z` | `sk1z` | `test-*`), least-privilege RBAC (no `delete`, no `pods/eviction`). envtest suite covers all three user stories.
- **Spec 002 — Agent** (~3,400 LOC Python). FastAPI on `:8080` (`/diagnose` async 202, `/healthz`, `/readyz` with model-availability fallback chain). `MAX_CONCURRENT_CASES=32`. Claude Agent SDK loop, `permission_mode="bypassPermissions"`, `Bash` + NR HTTP MCP, `emit_report` terminal tool with schema validation + `Status().Update` (NotFound retry). FR-12 deadline-race handling — agent never overwrites a controller-stamped `Failed`. Helm chart with workload-identity / IRSA mount, runbook content-hash linter (pre-install Job).
- **Spec 003 — On-Call UI + Slack Block Kit** (~600 LOC Go + ~40 LOC vanilla JS). Stdlib `net/http` + `html/template` (no framework, no node_modules). Routes: `GET /` · `GET /cases/<nhd>` · three POST action endpoints · `GET /healthz`. Drain uses `fetch()` + `ReadableStream` + hand-rolled SSE-frame parser (native `EventSource` is GET-only). Direct kube-API actuator with exactly four ClusterRole rules: `nodes get/list/watch/patch`, `pods get/list/watch`, `pods/eviction create`, NHD `get/list/watch/patch`. Anonymous behind `kubectl port-forward` (no SSO / Ingress / TLS — port-forward is the access boundary, deliberate hackathon-scope simplification).
- **Detection plane.** NPD on cf1z runs `check-containerd.sh` + `check-kubelet-healthz.sh` (30 s probe interval) flipping `ContainerRuntimeUnhealthy` and `KubeletUnhealthy`. The §4.2 catalog (conntrack, FD, PID, inode, disk-fill, DNS, IMDS) is plugin-ready for Day 2.
- **Fault-injection harness.** `chaos-containerd-unhealthy` and `chaos-kubelet-unhealthy` cronjobs verified end-to-end on cf1z (NPD detects + recovers, no side effects). Both Day-1 demo paths reproducible via `kubectl create job --from=cronjob/...`.

**Out of scope (explicit):** predictive failure scoring; multi-cluster federation in the UI; rollout to `stg-*` / `us-*` / `eu-*` (Constitution Article I.5 — non-prod only); SSO / public ingress / per-user attribution; eval-agent integration (`status.evaluation.*` is in the schema but unused); automated remediation beyond cordon (drain is human-initiated only); historical-incident training corpus.

---

## AI approach — *How does AI fit in?*

The hard part of node diagnosis isn't running probes — it's **picking which probe to run next based on what the last one returned**, **correlating across modalities** in one reasoning pass, and **producing a defensible answer with citations**. That judgment is exactly what a long-context model with tool use is good at, and exactly what classical automation can't do.

NodeMedic's agent does five things classical automation can't:

1. **Multi-turn evidence-driven probing.** The Claude Agent SDK loop picks the next probe based on the last result. Containerd socket missing → `ls -la /run/containerd/...` + `journalctl -u containerd`; kubelet healthz blackhole → `iptables -L OUTPUT -n` + `ss -tnlp | grep 10248`; node looks isolated → IMDS reachability before kubelet logs. The runbook prompt encodes our playbook; the agent applies it adaptively.
2. **Cross-modal correlation.** One reasoning pass synthesizes log lines, NRQL telemetry, K8s events, cloud-side events, `/proc` reads, SSH probe outputs — citing which signals support the conclusion. Output is structured `status.diagnosis` with `rootCause` (every claim cited), `rcaCategory` (enum), `confidence`, `evidence[]` (each pinned to a source modality), and `recommendation`.
3. **Hallucination resistance, layered.** Models fabricate. We assume that:
   - **`emit_report` schema validation** rejects malformed reports (out-of-range confidence, unknown enums, missing fields).
   - **Controller confidence gate** — ≥0.7 AND ≥2 distinct sources AND `recommendation ∈ {Cordon, DrainAndCordon}` required for cordon. Single-source diagnoses still surface for review but cannot trigger automation.
   - **Credential-layer least privilege** — agent IAM/RBAC/kube-SA are read-only. Even with `Bash`, no mutation. The mutation surface is the controller (cordon) and the UI (uncordon / drain / clear-skip-deletion, human-confirmed) — three independent gates between "model said something" and "the cluster changed."
4. **Operator-expertise capture.** Our incident playbook used to live in two engineers' heads. Now it's `prompts/runbook.md` — a build-time artifact, content-hash-linted at Helm install, no per-cluster overrides.
5. **Confirmation as a first-class concern.** Agent never acts on the cluster — it diagnoses. Controller acts (cordon) only when the gate passes. Human acts (uncordon / drain) only after reviewing the diagnosis.

---

## AI tools and technologies

- **Claude via internal `nerd-completion` gateway.** Opus 4.7 primary, Sonnet 4.6 fallback. Standard `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` env protocol — no SDK fork. Token from Vault (Nova's `nova-service` path). Gateway-availability fallback chain at `/readyz`: requested ID → best-available Opus → best-available Sonnet → fail-readyz.
- **Claude Agent SDK (Python).** `ClaudeSDKClient` per case, `permission_mode="bypassPermissions"`, `Bash` enabled, NR MCP wired as HTTP server, custom in-process `emit_report` terminal tool registered via `create_sdk_mcp_server` + `@tool` decorator.
- **New Relic MCP server (HTTP).** Public NR-managed server. Tools: `execute_nrql_query`, `analyze_entity_logs`, `analyze_golden_metrics`, `list_change_events`, `get_entity`, `lookup_entity`. NRQL forced to `account_id=1` (staging) per Constitution Article I.5.
- **Prompt caching (Anthropic `cache_control`).** Static system-prompt prefix (the runbook) is cached on the gateway; user prompts + tool results are case-local. Token cost flat across cases, no evidence leak.
- **Kubernetes-native primitives.** controller-runtime, kubebuilder-style CRDs, server-side apply with field manager `nodemedic-agent`, Helm 3.

---

## Execution — *How will we judge success?*

**Demo finale (90 seconds, end-to-end on cf1z, rehearsed):**

1. **0:00** — `kubectl create job --from=cronjob/chaos-kubelet-unhealthy …` on canary `cf1z-general-nodes-2000002`. Cronjob installs iptables REJECT on `127.0.0.1:10248` for 90 s.
2. **0:10** — NPD's `check-kubelet-healthz.sh` flips `KubeletUnhealthy=True`.
3. **0:11** — Controller debounces, resolves cloud metadata, creates NHD `phase=Pending`.
4. **0:12** — Controller `POST /diagnose`. Agent returns 202, queues worker. NHD `phase=Diagnosing`.
5. **0:12–0:55** — Agent loop. Live tool-call log streams: kubectl pod listing, NRQL on `K8sNodeSample` + `Log`, ssh `ss/iptables/journalctl`, `az vm get-instance-view`.
6. **0:55** — `emit_report`: `rcaCategory=Kubelet`, `confidence=0.92`, evidence `[ssh:iptables-rule, log:kubelet-healthz, cloud:az-vm-instance-view]` (3 distinct sources), `recommendation.action=Cordon`. `phase=Diagnosed`.
7. **0:56** — Controller's gate passes. Patches `node.spec.unschedulable=true`. `phase=Acted`, `decision=Applied`. Block Kit Slack: `🚨 Cordoned: cf1z-general-nodes-2000002 (cf1z) · Decision: Applied · Confidence: 0.92 · Trigger: KubeletUnhealthy · rcaCategory: Kubelet`.
8. **0:58** — Engineer clicks **View full diagnosis**. Browser opens `localhost:8080/cases/<nhd-name>`. Page renders metadata, rootCause, 6 collapsible evidence entries (engineer expands the iptables ssh probe), action history with `Cordon (controller, T+0:56)`.
9. **1:10** — Engineer clicks **Uncordon Node** → confirm. UI patches the node, page refreshes, audit annotation grows (`{ts, action:"uncordon", actor:"demo-anonymous", result:"ok"}`).
10. **1:30** — `kubectl get nhd <name> -o yaml` shows the `ui-action-history` ring buffer. Demo lands.

**Spec-bound success gates** (each rehearsable on cf1z):

1. **Correct failure class.** `rcaCategory` matches the injected fault (verified across both chaos cronjobs).
2. **Cited evidence — no hand-wavy answers.** Every claim in `rootCause` traces to an `evidence[]` entry; UI surfaces the raw result for human audit.
3. **Recommendation matches what oncall would have chosen.** Validated against runbook + CF kubelet/containerd playbook.
4. **Confidence-gated cordon.** Auto-cordon fires only when `confidence ≥ 0.7` AND ≥2 distinct sources AND `recommendation ∈ {Cordon, DrainAndCordon}`; gate failures route to `HumanInLoop` Slack.
5. **Cordon happens before cascade.** End-to-end median well under 60 s; FR-5 controller deadline 5 m, one-shot retry on agent timeout.
6. **Cloud parity.** Same image / chart / runbook on EKS and Azure kubeadm; demonstrated on cf1z, identical code path on EKS.
7. **Human-in-the-loop closure.** Engineer reviews diagnosis, clicks Uncordon (idempotent) / Drain (SSE, PDB-aware) / Clear-skipDeletion, all auditable on the NHD itself.
8. **Stretch — fault-class generalization.** Same agent, no prompt changes, handles Day-2 fault classes (conntrack, FD, PID, inode, disk-fill, DNS, IMDS) the moment Scope 1 ships matching plugins.

**Composite acceptance** (Spec 003 AC-14): chaos-kubelet on cf1z → Slack message → engineer click → per-case page → Uncordon → confirm → page refreshes, annotation grows. **≤ 60 s Slack-ping to action.**

---

## Dependencies and potential blockers

**Already retired during the build:**

- Public NR HTTP MCP server wired and authenticated; prompt caching keeps token cost flat.
- SSH path on cf1z verified (hackathon key + workload-identity Azure RBAC).
- Cluster-name guard discipline — both Helm `_helpers.tpl` macro and binary `validateClusterName` reject anything outside `{cf1z, jc1z, sk1z, test-*}`.
- Two real chaos cronjobs verified end-to-end on cf1z; no hand-crafted CR fallback needed.
- CRD + RBAC review — UI ClusterRole has exactly four rules; controller chart cannot acquire `pods/eviction`; agent chart cannot acquire `nodes/patch`. Constitution Articles I.1 + I.2 enforced at chart-render.
- Three images (controller / agent / on-call UI) build on Apple Silicon (Colima) and push to `cf-registry.nr-ops.net/container-fabric/` with developer creds.

**Live blockers (small, mitigated):**

- **Anthropic gateway model availability.** `claude-opus-4-7` may not be in the catalog. Mitigation: `/readyz` resolves at startup with a fallback chain and logs the resolved IDs at INFO.
- **cf1z apiserver health.** UI is only as available as cf1z. Mitigation: pre-flight check (`kubectl get nodes`) before chaos trigger; backup is a recorded session.
- **Port-forward discipline.** UI is anonymous behind port-forward (deliberate hackathon-scope simplification). Mitigation: pre-canned demo script.
- **Runbook drift.** Pre-install Helm Job runs the runbook content-hash linter against the seven required clauses (cloud dispatch, evidence calibration, recommendation rubric, credential forbid-list, NRQL `account_id=1`, `emit_report` discipline, cf1z-deployed Condition decision trees).

**Out-of-scope cleanly:** prod rollout (`stg-*` / `us-*` / `eu-*`); multi-cluster UI federation; predictive failure scoring; durable JSONL audit; programmatic credential-verification Job — all on the production hardening track.
