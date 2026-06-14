# Spec 002 — NodeMedic Agent

**Status:** Draft · 2026-06-12
**Scope:** Scope 3 of the NodeMedic hackathon (Captain: Sachin)
**Constitution:** [`.specify/memory/constitution.md`](../../memory/constitution.md)
**Companion docs:** [`docs/cf/nodemedic-scope.md`](../../../docs/cf/nodemedic-scope.md) §6; [`docs/cf/nodemedic.md`](../../../docs/cf/nodemedic.md); diagrams in [`docs/cf/diagrams/`](../../../docs/cf/diagrams/)
**Sister spec:** [`../001-nodemedic-controller/spec.md`](../001-nodemedic-controller/spec.md) (Scope 2)

This spec defines **what the agent does**, not how each line of Python is written. It binds the two cross-scope contracts (the `NodeHealthDiagnosisAI` CRD and `POST /diagnose`) on the agent's side. Implementation choices that don't change observable behavior or evidence are out of scope here and belong in the plan.

> **Hackathon-scope deviations from `docs/cf/nodemedic-scope.md` §6** — bound by the constitution:
> - Tool surface is raw `Bash` + the public NR HTTP MCP server (`docs.newrelic.com/docs/agentic-ai/mcp/`) + SDK built-in tools; no in-proc `node_ops` / `cloud_info` SDK MCP servers, no `can_use_tool` allow-list, no SSH prefix-match.
> - `permission_mode = "bypassPermissions"`.
> - **No agent-side termination ceiling** — no `max_turns`, no `max_budget_usd`, no wall-clock `deadline`. The agent loop runs until `emit_report` is called or the model halts. The controller's own ~60 s deadline (Spec 001 FR-5) bounds *user-visible* case duration and routes to HumanInLoop, but does NOT cancel the agent's loop. The Anthropic key's account-level budget cap is the only spend ceiling.
> - Tool-call observability via stdout logs only — no durable audit JSONL.
>
> Credential-layer least privilege (read-only IRSA / Azure RBAC / NR token / kube ServiceAccount) is the primary safety boundary. The structured allow-list, budget-tracking, and durable-audit surface from `nodemedic-scope.md` §6.3 / §6.5 are deferred to the post-hackathon production hardening track per the constitution.

---

## Clarifications

### Session 2026-06-12

- Q: Where does the runbook (system prompt) live, who owns it, and how does it get updated? → A: Single canonical `prompts/runbook.md` in the agent repo, baked into the image at `/app/prompts/runbook.md` at build time. Owned by Scope 3 (Sachin). Changes flow through the agent's image build — no per-cluster overrides, no runtime ConfigMap.
- Q: What does the agent do if `POST /diagnose` arrives before the controller's NHD `Create` is visible to the agent's k8s client cache? → A: Bounded retry on `NotFound` during `Status().Update` — 3 attempts at 250 ms / 500 ms / 1 s. On exhaustion, fall through to the existing `Failed/CRWriteFailed` terminal state. Documented in FR-8.
- Q: What happens at startup if the requested Claude model IDs (`claude-opus-4-7` / `claude-sonnet-4-6`) aren't in the nerd-completion gateway's catalog? → A: Runner resolves at startup as part of `/readyz` — try requested ID first, fall back to best-available Opus → best-available Sonnet → fail `/readyz` if nothing matches. Resolved primary + fallback IDs logged at INFO at startup. Override via `CLAUDE_MODEL` / `CLAUDE_FALLBACK_MODEL` per-cluster Helm value. Documented in FR-3 / NFR-6.
- Q: What's the expected concurrent-load shape for the demo and rehearsal? → A: Typical demo = 1 concurrent case (operator-driven). Peak rehearsal = 10 concurrent cases (hand-crafted fault storm). `MAX_CONCURRENT_CASES=32` keeps 22 cases of headroom above peak — sized as a safety margin, not a target. NFR-8 memory sizing rebuilt around this load shape.
- Q: Which fault classes does the Day-1 runbook cover? → A: Bound to **what's actually deployed on cf1z**, not the aspirational §4.2 list. cf1z runs `hack-node-problem-detector` (NPD v0.8.24, in `cf-monitoring`) with two custom plugins flipping NodeConditions: `ContainerRuntimeUnhealthy` (via `check-containerd.sh`, 30 s probe) and `KubeletUnhealthy` (via `check-kubelet-healthz.sh`, 30 s probe). Day-1 runbook covers these two. Day-2+ adds the §4.2 fault classes (Conntrack, FD, PID, Inode, Disk-fill, DNS, IMDS) only as Scope 1 ships corresponding NPD plugins. Demo target = `ContainerRuntimeUnhealthy` on cf1z (Harrison's `chaos-containerd-unhealthy` cronjob is the deployed fault injector; canary node `cf1z-general-nodes-2000002` already labeled `canary-chaos-test=true`).
- Q: Is disk I/O pressure part of the agent demo? → A: **No** for v1. The deployed `chaos-disk-io-stress` cronjob produces `system-stats-monitor` Prometheus metrics (`disk/avg_queue_len`, `disk/io_time`, `disk/weighted_io`) — not NodeConditions. The agent's trigger contract requires a NodeCondition (`spec.case.trigger.type`); Prometheus metrics don't fire `POST /diagnose`. Bringing disk I/O into agent scope requires either (a) a CustomPluginMonitor that thresholds the metrics and flips a Condition like `DiskIOPressure=True`, or (b) expanding the agent's trigger model to accept Prometheus-metric thresholds. Both deferred. Documented in §3 non-goals.
- Q: How is "runbook v1 ready" verified before AC-3 (the cf1z chaos demo)? → A: New **AC-15** enumerates the seven required runbook clauses (cloud dispatch sections, evidence calibration, recommendation rubric, credential-path forbid list, NRQL `account_id=1` instruction, `emit_report` calling discipline, and decision-tree sections for the two cf1z-deployed NodeConditions — `ContainerRuntimeUnhealthy` and `KubeletUnhealthy`). Runbook is gated against the checklist at build time (lint script in the agent repo + pre-install Helm Job, exact wiring is plan-phase). Catches partial-runbook ship deterministically before the agent's behavioral ACs run.
- Q: Day-1 runbook covers `KubeletUnhealthy` — but the chaos cronjob set on cf1z previously only had a `ContainerRuntimeUnhealthy` injector. Is `KubeletUnhealthy` a real demo path or a runbook-only entry? → A: Real demo path. Built and shipped a `chaos-kubelet-unhealthy` cronjob (PR #129 commit `9c9382f`) that uses `iptables REJECT` on `127.0.0.1:10248` for 90 s — same shape as the containerd cronjob, lowest-risk mechanism since kubelet keeps running and only NPD's probe path is blocked. Verified end-to-end on cf1z 2026-06-14: NPD detected `KubeletUnhealthy=True` 10 s after rule install, `False` 9 s after rule removal, no side effects on the canary node. Both Day-1 demo paths now have rehearsable chaos injectors. AC-3 (containerd) and a sibling AC-3b (kubelet) both run end-to-end. AC-14's isolation test uses both real injectors — no hand-crafted CR fallback needed.

---

## 1. Problem statement

When an NPD-watched `NodeCondition` flips `True`, an engineer's job is: form a hypothesis, gather evidence, decide whether to cordon, and document why. NodeMedic does that in 60 s.

The **agent** is the part of NodeMedic that:

1. Receives a case via `POST /diagnose` (controller-driven).
2. Runs a bounded, audited Claude Agent SDK loop with read-only investigation tools (`kubectl`, SSH, `/proc`, NRQL, cloud-info on AWS or Azure).
3. Cites evidence from at least 2 distinct source modalities.
4. Writes the diagnosis (`rootCause`, `confidence`, `evidence[]`, `recommendation`) to the `NodeHealthDiagnosisAI` CR's `status.diagnosis`.
5. Lets the controller (Scope 2) decide whether to act on it — the agent never cordons.

Without the agent, the controller has no diagnosis. Without the controller, the agent has no trigger and no actuator. Together they close the gap between "node has problem X" and "here's why, here's the cordon decision, here's the evidence trail."

---

## 2. Goals (in scope for v1)

| # | Goal | Why |
|---|---|---|
| G1 | Accept `POST /diagnose` per the contract in `nodemedic-scope.md` §3.1 and return `202 Accepted` promptly. Diagnosis itself is async — the controller does not block on it, and the CR informer surfaces the result. No hard latency contract on diagnosis duration. The agent has no termination ceiling (FR-7); the controller's own deadline (Spec 001 FR-5) caps the user-visible case duration. | Cross-scope contract on the HTTP shape; latency is observability, not a SLO. |
| G2 | Run an isolated Claude Agent SDK loop **per case** using `claude-opus-4-7` (fallback `claude-sonnet-4-6`). Provider is the **internal nerd-completion gateway** — the SDK reads `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_BASE_URL` env vars, the latter pointed at `https://nerd-completion.staging-service.nr-ops.net`. Same pattern as `nova/k8s-agent-claude-sdk`. Every accepted `caseId` produces a brand-new diagnosis: fresh `ClaudeSDKClient` session, no message history carried over, no in-loop tool result cache shared across cases. The only cross-case state permitted is the prompt cache on the system block (a token-cost optimization on the static runbook prefix — not conversation memory). | Every CR's `status.diagnosis` must reflect what was true for **this** node at **this** moment. Cross-case state would leak prior nodes' evidence into the current diagnosis and dissolve the audit trail. nerd-completion is the established team pattern — token from Vault, no Bedrock IRSA wiring, no separate billing setup. |
| G3 | Give the agent a CLI-based investigation surface via `Bash`: `kubectl` (in-cluster ServiceAccount), `aws` (IRSA), `az` (workload identity), `ssh` (hackathon key), plus the public New Relic HTTP MCP server as the NRQL/logs/metrics tool. No in-proc tool wrappers, no `can_use_tool` allow-list. | Constitution Article I, hackathon-scope simplifications. Credential-layer least privilege bounds blast radius; the agent cannot do what its credentials cannot do. |
| G4 | Dispatch cloud-CLI calls to the right cloud based on `case.provider`. The runbook prompt instructs the agent to use `aws …` when `provider=aws` and `az …` when `provider=azure`; the agent pod has both CLIs and credentials for the matching cloud only on each per-cluster install. | Constitution Article II.7 (one binary, two clouds — different credential set per install). |
| G5 | Write whatever evidence the agent gathered into `status.diagnosis.evidence[]` — even a single entry. The controller's confidence gate (Constitution Article I.3) is the single layer that enforces "≥ 2 distinct sources" for auto-cordon. The agent does not pre-reject low-evidence reports. | A single-source diagnosis is still useful operator-visible information; the gate downstream prevents it from triggering an automatic cordon. Two enforcement layers were redundant. |
| G6 | Write the diagnosis via `Update` on `status.diagnosis` of the existing NHD CR (the controller created it; the agent does not `Create`) | Per controller spec FR-3 + sequence diagram |
| G7 | Treat repeat `caseId` from the controller as idempotent — return 202 if already queued/running; echo final outcome if already complete | Closes Scope 2's open question §10.3 |
| G8 | Observe every tool call (every `Bash` invocation, every NR MCP call, every SDK built-in call) via `PreToolUse` hook → structured stdout logs (JSON-formatted, one line per call, with `caseId`/`tool`/`command-or-args`). | Constitution Article I.4. The PVC-backed JSONL is a deferred production-hardening item per the constitution. |
| G9 | The agent loop ends when `emit_report` is called or the model declines to continue. **No agent-side termination ceiling** — no turn cap, no cost cap, no wall-clock deadline. The controller's deadline (Spec 001 FR-5) still bounds the user-visible case but does not cancel the agent. | Constitution Article I, hackathon-scope simplifications. Hackathon focus is effective analysis; cutting off a deep investigation at 60 s loses more value than it saves. Spend is bounded by the API key's account-level cap (§10.1). |
| G10 | Run on any non-production target cluster (`test-*`, designated Azure kubeadm test clusters, or legacy dev clusters like `cf1z`/`jc1z`/`sk1z`) with no code changes — same image, same Helm chart, switched only by per-cluster Helm values (which cloud's credentials are mounted, kubeconfig context, etc.). Constitution Article I.5 bounds the permitted surface. | Constitution Article II.7 + I.5. |
| G11 | Run as an **independent Deployment + Service** in the same cluster as the controller (not as a sidecar in the same pod, not on `interlinked`). The controller reaches the agent at `nodemedic-agent.<ns>.svc:8080`; the agent reads/updates the NHD CR via the in-cluster apiserver. Agent and controller have independent lifecycles, RBAC, and resource limits. | Decision bound in §3. Separate deployments mean either side can be restarted, scaled, or rolled without disturbing the other. |

## 3. Non-goals (explicitly out of v1)

Per Constitution Article III.3 and `nodemedic-scope.md` §9:

- **No mutating credentials.** The agent's IAM/RBAC/kube-SA/NR token are read-only. Even with `Bash` access to `kubectl apply` / `aws ec2 terminate-instances` / `az vm delete`, the credentials reject the call. This is the safety boundary.
- **No drain, no eviction, no pod deletion, no node termination, no cordon.** Even read-only credentials would permit `kubectl cordon` for the mounted SA — so the agent's kube ServiceAccount explicitly does NOT have `nodes/patch`. The controller holds that verb (per its FR-14 / NFR-4); the agent does not.
- **No production cluster access.** The agent runs only against non-production cluster apiservers (`test-*`, designated Azure kubeadm test clusters, or legacy dev clusters like `cf1z`/`jc1z`/`sk1z` per Constitution Article I.5). NR queries always go to account `1` (staging). Production clusters (`stg-*`, `us-*`, `eu-*`) are off-limits.
- **No Anthropic Files API, no batch API, no message memory persistence.** Each case is a fresh `ClaudeSDKClient` session; the system-prompt cache is the only cross-case state.
- **No durable audit log.** The constitution defers the per-case JSONL on PVC to the production hardening track. Tool-call records land on structured stdout logs only — durable for the lifetime of the pod / log-rotation window, gone after that. `kubectl logs --since=… | grep <caseId>` is how operators retrieve them during the demo. The Slack "View audit log" button is dropped (no stable URL to link to). Restoring the durable JSONL is on the constitution's production-hardening list.
- **No eval agent integration.** `status.evaluation.*` is never written by the agent.
- **No live agent on `interlinked` hub.** Centralized deployment is a post-hackathon concern.
- **No SSM, no Azure Bastion.** Hackathon-only direct SSH key. Test clusters only.
- **No retry across model errors mid-loop.** A 5xx from the Anthropic API kills the case as `Failed` with `reason=ModelError`. Resume-from-checkpoint is out of scope.
- **No streaming results.** The HTTP response is `202 Accepted`; final results land on the CR. No SSE, no WebSocket.
- **No agent-side termination ceiling.** The constitution defers `max_turns`, `max_budget_usd`, and the wall-clock `deadline` to the production hardening track. The agent loop ends only when `emit_report` is called or the model halts. The controller's deadline (Spec 001 FR-5) marks the NHD `Failed` after ~60 s but does not cancel the agent — see FR-12 for the resulting status-write race. Global cost is bounded by the Anthropic key's account-level budget cap (§10.1).
- **No structured tool allow-list, no `can_use_tool` callback, no SSH command prefix-match.** The constitution defers these. The agent uses `Bash` directly. The deviation from `nodemedic-scope.md` §6.3 is bound by the constitution and tracked as a deferred production-hardening item.
- **Disk I/O pressure is NOT a v1 demo target.** cf1z's `chaos-disk-io-stress` cronjob produces `system-stats-monitor` Prometheus metrics (`disk/avg_queue_len`, `disk/io_time`, `disk/weighted_io`) — not NodeConditions. The agent's trigger contract requires a NodeCondition (`spec.case.trigger.type` per `nodemedic-scope.md` §3.1); Prometheus metrics don't fire `POST /diagnose`. Bringing disk I/O into agent scope requires either (a) a CustomPluginMonitor that thresholds the metrics and flips a Condition like `DiskIOPressure=True`, or (b) expanding the agent's trigger model to accept Prometheus-metric thresholds. Both deferred. (Per Clarifications binding.)
- **Day-1 runbook covers only the NodeConditions actually deployed on cf1z** — `ContainerRuntimeUnhealthy` (via `check-containerd.sh` plugin, primary demo target) and `KubeletUnhealthy` (via `check-kubelet-healthz.sh` plugin, secondary). The §4.2 fault-class list (Conntrack, FD, PID, Inode, Disk-fill, DNS, IMDS) is aspirational against current cf1z reality — no plugins for those classes ship today. Day-2+ runbook expansion lands as Scope 1's plugins land. (Per Clarifications binding.)

If something here moves into scope mid-hackathon, it requires an amendment per the constitution.

---

## 4. Personas & demo scenarios

**Operator (demo driver).** Triggers the deployed chaos cronjob on cf1z (`kubectl create job --from=cronjob/chaos-containerd-unhealthy chaos-containerd-unhealthy-manual -n default`), watches Slack and the live tool-call log streaming from the agent pod. The cronjob bind-mounts a regular file over `/host/run/containerd/containerd.sock` inside the `hack-node-problem-detector` pod's container view on canary node `cf1z-general-nodes-2000002` for 90 s, NPD's `check-containerd.sh` probe (30 s interval) detects the missing socket and flips `Node.status.conditions[ContainerRuntimeUnhealthy]=True` with `reason=ContainerdUnreachable`. Expects: agent picks up the case soon after NHD creation; the final report on the CR cites at least one NRQL row, one SSH probe, and one cloud-side call. The exact timing is not a contract — async means whenever the agent finishes is when the controller acts. The controller will mark the NHD `Failed` after ~60 s if the agent hasn't reported yet, but the agent keeps working in the background (FR-7 / FR-12). After the 90 s shadow window, the cronjob umounts the bind, the probe recovers, and `ContainerRuntimeUnhealthy` flips back to `False` with `reason=ContainerRuntimeIsHealthy`.

**CF on-call (post-hackathon shape).** Reads the diagnosis on the CR. Expects: every claim in `rootCause` is supported by at least one entry in `evidence[]`. The agent's reasoning chain is reconstructable from the live structured stdout logs while the pod is up; durable replay across pod restarts is deferred to the production rollout (durable audit JSONL on the constitution's production-hardening list).

**Captains reviewing the design.** Expect that auto-cordon is defensible because (a) the agent's credentials cannot mutate the cluster or cloud (Constitution Article I.1 — verified by chart review during hackathon, by a programmatic post-install Job in production per the "Production hardening" list), (b) evidence count is gated by the controller (Spec 001 FR-6), and (c) every tool call left a structured-log line with `{tool, command-or-args}` (NFR-3). The structured allow-list, durable audit JSONL, programmatic credential verification, and several other production controls are deferred per the constitution's hackathon-scope simplifications; credential-layer least privilege (Article I.1) is the primary remaining safety boundary.

### Walkthrough — happy path
1. Controller `POST /diagnose` with `case.provider=azure`, `trigger.type=ContainerRuntimeUnhealthy` (cf1z's deployed Day-1 demo path; see Clarifications).
2. Agent returns `202 Accepted` promptly; queues a worker task. (Async — the controller is not waiting on a synchronous diagnosis result here.)
3. Worker boots a `ClaudeSDKClient` with the cached runbook prompt, `permission_mode="bypassPermissions"`, `PreToolUse` hook registered, `mcp_servers={"nr": <NR HTTP MCP>}`, `Bash` tool enabled.
4. Loop runs — agent calls (illustrative order, agent picks):
   - `Bash`: `kubectl --context=$KUBE_CONTEXT get pods --field-selector spec.nodeName=ip-10-1-2-3.ec2.internal -A`
   - `nr.execute_nrql_query`: `K8sNodeSample` / `Log` on the node, last 15m, `account_id=1`
   - `Bash`: `ssh -i $SSH_KEY_PATH -o StrictHostKeyChecking=no <user>@<node-ip> 'ls -la /run/containerd/containerd.sock; pgrep -fa containerd | head -3; journalctl -u containerd --since "5 min ago" --no-pager | tail -20'`
   - `Bash`: `aws ec2 describe-instance-status --instance-ids i-0abc1234 --region us-east-2`
   - `Bash`: `aws health describe-events --filter "services=EC2,regions=us-east-2" --max-results 5`
5. Agent calls `emit_report` (an in-process tool registered via the SDK) with `rootCause`, `rcaCategory=Conntrack`, `confidence=0.85`, `evidence=[nrql:…, ssh:…, cloud:…]` (3 distinct sources, well-supported), `recommendation.action=Cordon`.
6. Terminal-tool handler validates schema only (required fields, enum values, `confidence ∈ [0.0, 1.0]`), builds the `NodeHealthDiagnosisAI` status payload, `Update`s the CR via `Status().Update`. Loop ends. Whatever evidence the agent provided — one entry, two, or many — is written verbatim to the CR. The controller's confidence gate decides downstream whether to cordon.
7. Per-tool-call structured-log lines (one per `Bash` / NR MCP / SDK built-in call) have already been emitted to stdout during the loop; the runner does NOT need a separate "flush" step. The case-complete log line is emitted last.

### Walkthrough — Azure parity
Same flow, `case.provider=azure`. The runbook prompt instructs the agent to use `az` instead of `aws`:
- `Bash`: `az vm get-instance-view --ids /subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<vm>`
- `Bash`: `az rest --method get --uri 'https://management.azure.com/<vm-id>/providers/Microsoft.ResourceHealth/availabilityStatuses/current?api-version=2022-10-01'`
- (or via the `azure-mgmt-compute` Python wheel if the agent prefers; both are available)

Same agent image, same runbook. The cluster-specific Helm install determines which credentials are mounted (AWS IRSA vs Azure workload identity). The final CR has `provider=azure` echoed and at least one `evidence[].source=cloud` entry citing an Azure-side response.

### Walkthrough — single-source evidence (low confidence)
Agent finishes the loop with only one evidence source (e.g. NR MCP failed mid-case, agent gave up early). The agent's runbook prompt instructs it to lower its `confidence` value when evidence is thin, and to choose `recommendation.action="NoAction"` if it isn't sure.
- Agent calls `emit_report` with `evidence=[nrql:…]` (1 source), `confidence=0.4`, `recommendation.action=NoAction`.
- Terminal-tool handler validates schema (passes — schema doesn't require ≥2 sources), writes the CR, ends the loop. `phase=Diagnosed`.
- Controller's reconciler sees the CR. Confidence gate fails (0.4 < 0.7 AND only 1 distinct source). Controller marks `action.decision=HumanInLoop`, posts the "needs review" Slack message. Node is not cordoned.

This is the gate-fail demo path — the agent producing a low-confidence single-source report is *useful* output (it tells on-call what the agent saw and why it gave up), and the controller-side gate is what keeps it from triggering an automatic cordon.

### Walkthrough — controller deadline beats the agent (no agent-side timeout)
Agent is still investigating at the 60 s mark; no `emit_report` yet.
- Controller (Spec 001 FR-5) marks the NHD `Failed`, posts a "needs human review" Slack message, moves on. From the controller's perspective, the case is closed.
- Agent keeps running. No agent-side cancellation (FR-7).
- Two outcomes possible:
  1. *Agent finishes successfully later (e.g. 90 s):* Agent attempts `Status().Update` with the diagnosis. FR-12 detects the existing `phase=Failed` and refuses to overwrite. The CR retains the controller's `Failed` decision. Structured stdout logs captured the late tool calls — `kubectl logs --since=… | grep <caseId>` reconstructs what the agent ultimately concluded, *as long as the pod hasn't restarted and logs haven't rolled* (constitution trade-off).
  2. *Agent halts on its own (model declines, or `emit_report` is never called):* Agent writes a `Failed` CR with `reason=ModelHalted` — but FR-12 also refuses to overwrite the controller's prior `Failed`. The CR keeps the earlier failure reason; the structured stdout logs are the only record of what the agent did, with the same durability caveat.

This is a deliberate hackathon-scope trade-off bound by the constitution: the agent prioritizes effective analysis over respecting the controller's clock, and accepts that late-completion observability is ephemeral.

### Walkthrough — duplicate caseId (controller retry)
Controller `POST /diagnose` a second time with the same `caseId` (e.g. after a controller restart mid-case).
- Agent looks up `caseId` in the in-memory case table. If still queued or running: return `202` with `status="queued"`.
- If already complete: return `202` with `status="complete"` (controller will see the final CR via informer either way).
- No second loop is started for the same `caseId`.

---

## 5. Functional requirements

### FR-1 — `POST /diagnose` endpoint
The agent MUST serve `POST /diagnose` on `:8080` per `nodemedic-scope.md` §3.1.

- **No authentication for hackathon scope.** The endpoint is open on the in-cluster Service. The deviation from `nodemedic-scope.md` §3.1 (which specifies a Bearer token) is acceptable because:
  - The Service is `ClusterIP` only — not exposed outside the cluster (no Ingress, no LoadBalancer, no NodePort).
  - The cluster is non-production per Constitution Article I.5 — `test-*`, designated Azure kubeadm test clusters, or legacy dev clusters (`cf1z`/`jc1z`/`sk1z`); the only callers in-cluster are the controller and ad-hoc `kubectl port-forward` from the demo operator.
  - A NetworkPolicy SHOULD restrict ingress to the controller's pod selector if it is ergonomic to add, but is not required for the hackathon.

  The 401 path remains in the spec at the contract level (`nodemedic-scope.md` §3.3) because the controller spec's FR-4 documents how it would be handled. The agent simply never returns 401 in this iteration; the auth header (if sent by the controller) is ignored. Restoring authentication is a deferred production-hardening item tracked alongside the other constitution simplifications.

- Body validation: `caseId`, `nodeName`, `clusterName`, `provider ∈ {aws,azure}`, `region`, `instanceId`, `trigger.{type,reason,message,observedAt}`, `budgets.{maxTurns,maxBudgetUSD,deadlineSec}` are all required. Missing or wrong-typed → `400`.
- Response: `202 Accepted` with `{caseId, status:"queued"}`. Result is **not** in the body — this is an async endpoint. Latency is not contracted (NFR-1).
- Concurrency cap: configurable via `MAX_CONCURRENT_CASES` env (default `32`). When at cap, return `429` (controller backoff path per its FR-4). The cap bounds simultaneously-active case workers, not reserved capacity — idle slots cost nothing. `32` is sized to absorb a hand-crafted fault-storm rehearsal without surprising the memory budget; see NFR-8 for the resource implications.
- Idempotency: see G7 / FR-10.

### FR-2 — Health & ops endpoints
- `GET /healthz` → `200` if process is up. Required.
- `GET /readyz` → `200` only if Anthropic API and the NR MCP backend are reachable from a 5-second probe at startup. Cached for 30 s. Required.
- `GET /metrics` → Prometheus exposition with the metrics in NFR-3. **Optional** — implement only if it falls out cheaply (e.g. the FastAPI Prometheus instrumentation drops in with one decorator). The demo doesn't depend on metrics; structured stdout logs cover observability for the hackathon. Absence of `/metrics` is not a defect.

### FR-3 — Claude Agent SDK loop
For each accepted case the agent MUST:
- Construct a **fresh** `ClaudeSDKClient` instance and a **fresh** `ClaudeAgentOptions` for that case. No `ClaudeSDKClient` MAY be reused across cases. No message history, conversation state, prior tool results, or scratchpad data MAY carry over from any prior case.
- Construct `ClaudeAgentOptions` with:
  - **Provider: internal nerd-completion gateway.** Auth via `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_BASE_URL` env vars — the standard Claude Agent SDK env-var protocol with the base URL pointed at `https://nerd-completion.staging-service.nr-ops.net`. Token is mounted from `Secret/nodemedic-anthropic-token`, sourced from Vault path `containers/teams/nova/staging/nova-service/NERD_COMPLETION_API_TOKEN` (reusing Nova's `nova-service` path for the hackathon — same path `nova/k8s-agent-claude-sdk` uses). No Bedrock IRSA, no separate Anthropic billing setup.
  - **Primary model: `claude-opus-4-7`.** Set via `CLAUDE_MODEL` env var. The 1M-context variant is conservative headroom but the workload footprint (system prompt ~2 KB, user prompt small, tool results 4 KB-truncated, even long loops well under 200 KB total) does not actually require it; whatever the gateway exposes is fine.
  - **Fallback model: `claude-sonnet-4-6`** (default context). The Claude Agent SDK's fallback mechanism handles automatic switchover.
  - **Gateway-availability fallback chain (per Clarifications binding):** the requested IDs (`claude-opus-4-7` / `claude-sonnet-4-6`) MAY not be in the nerd-completion gateway's catalog at deploy time — the gateway's catalog is curated by its operators and lags Anthropic's public API. The runner MUST resolve the requested IDs at startup as part of `/readyz`:
    1. If `CLAUDE_MODEL` is in the gateway catalog, use it.
    2. Else, fall back to the best-available Opus on the gateway (newest version).
    3. Else, fall back to the best-available Sonnet on the gateway (newest version).
    4. Else, fail `/readyz` (no usable Claude model — block startup, surface the error).

    The resolved primary and fallback model IDs MUST be logged at INFO at startup with `event=model_resolved primary=… fallback=…` so the demo deck can cite what actually shipped. `CLAUDE_MODEL` and `CLAUDE_FALLBACK_MODEL` are overridable per-cluster Helm value if a cluster wants to pin a specific gateway-curated ID.
  - **SDK wiring is the standard env-var protocol.** No code change versus the public Anthropic API path — the SDK's HTTP client respects `ANTHROPIC_BASE_URL`, so pointing it at the gateway is the entire integration. The plan confirms the exact base URL (staging vs prod nerd-completion endpoint) at deploy time.
  - `system_prompt = load_runbook()` reads the canonical runbook at `/app/prompts/runbook.md` (baked into the image at build time per the Clarifications session — single source of truth, no per-cluster overrides, no runtime ConfigMap mount). Anthropic prompt-cache `cache_control` is set on the system block. Prompt caching applies **only** to the static runbook prefix — it is a token-cost optimization on the system block, not conversation memory. The user prompt, tool calls, tool results, and assistant turns are case-local and MUST NOT be cached or replayed across cases.
  - `permission_mode = "bypassPermissions"`. (Constitution hackathon-scope simplification.)
  - `hooks = [("PreToolUse", tool_log_hook)]` — emits a structured stdout log line per tool call (NFR-3).
  - `mcp_servers = {"nr": <NR HTTP MCP config>}` (FR-4).
  - `Bash` tool enabled with no allow-list. (Constitution hackathon-scope simplification.)
  - SDK built-in tools (`Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebFetch`, `WebSearch`, etc.) MAY be enabled per FR-4 (4). No restriction on which are wired; the runbook prompt steers usage and structured stdout logs record every call.
- Call `client.query(build_user_prompt(case))` with a user prompt that injects `nodeName`, `clusterName`, `provider`, `region`, `instanceId`, `trigger`, and `caseId`.
- Iterate `client.receive_response()` until `emit_report` is called (terminal tool, FR-8) or the model halts on its own (no further tool calls, no further messages). No agent-side timeout, no turn cap, no cost cap (FR-7).
- On case finalize (success or failure), discard the `ClaudeSDKClient` instance and any per-case in-memory state besides the entry in the case table (FR-10). No on-disk artifacts to flush.

The agent process MUST be long-lived so the system-prompt cache stays warm across cases. Long-lived process ≠ long-lived session; the SDK client is per-case.

### FR-4 — Tool surface
The agent MUST be wired with exactly two tool surfaces:

1. **`Bash` (built-in SDK tool)** — used for all CLI investigation. The pod has the following installed and on PATH:
   - `kubectl` (configured to use the in-cluster ServiceAccount automatically — no kubeconfig needed for the agent's own cluster).
   - `aws` CLI v2 — credentials from IRSA on the agent pod (AWS-cluster install) or absent (Azure-cluster install).
   - `az` CLI — credentials from workload identity (Azure-cluster install) or absent (AWS-cluster install).
   - `ssh` — uses the hackathon SSH private key at `$SSH_KEY_PATH` (FR-16).
   - Standard utilities: `curl`, `jq`, `cat`, `grep`, `awk`, `sed`, `dig`, `nslookup`, `ss`, `nproc`, `uptime`, `df`.

   No allow-list, no command-prefix match, no argument schema. The runbook prompt steers the agent toward read-only commands; the credential layer (IAM read-only, kube SA read-only, etc.) bounds what mutation calls would actually achieve. Constitution Article I, hackathon-scope simplifications.

2. **New Relic HTTP MCP server (external)** — the public NR MCP at `docs.newrelic.com/docs/agentic-ai/mcp/`. Wired as `mcp_servers["nr"]` with the NR auth token from `Secret/nodemedic-nr-token`. Tools used: `execute_nrql_query`, `analyze_entity_logs`, `analyze_golden_metrics`, `list_change_events`, `get_entity`. The runbook prompt MUST instruct the agent to set `account_id=1` (staging) on every call. (Overrides via callback are not enforced per the constitution's hackathon-scope simplifications; if the agent passes a different account, the NR token's own scope is the safety boundary.)

3. **`emit_report` terminal tool** — registered via the SDK as an in-process tool the runner intercepts. See FR-8.

4. **SDK built-in tools are permitted** — the Claude Agent SDK's stock tool set (`Read`, `Write`, `Edit`, `Glob`, `Grep`, `WebFetch`, `WebSearch`, etc.) MAY be enabled and used by the agent. These operate on the agent pod's local filesystem and the public internet rather than on the target node, but for the hackathon scope we accept that:
   - The agent might use `WebFetch`/`WebSearch` to look up an unfamiliar error code or kernel panic signature mid-diagnosis. Useful.
   - The agent's own scratchpad on `/tmp` (or in-process state) covers most "recall what I already tried" needs. There's no durable audit file to read back from.
   - The agent could read its own SSH private key (`$SSH_KEY_PATH`) or other Secret-mounted credentials via `Read`. The runbook prompt MUST explicitly forbid this and structured stdout logs capture every `Read` call so the operator can see if it happens during the demo. Constitution Article I.1 (credential-layer least privilege) plus Article I.5 (non-production clusters only) still bound the blast radius — even if the agent reads the key, the *surface* it can SSH into is the same set of read-only-credentialed nodes it could already SSH into via `Bash`. No new mutation paths opened.

   This is a deliberate hackathon-scope relaxation from an earlier draft of this spec which forbade the SDK built-in tools. The relaxation is consistent with the constitution framing: structural restrictions are deferred in favor of credential-layer safety. To restore for production: re-add the prohibition to this FR and disable the built-ins via SDK options.

### FR-5 — (Removed)

Two-layer allow-list enforcement is deferred per the constitution's hackathon-scope simplifications. Credential-layer least privilege (NFR-4) is the primary safety boundary. To restore for production: see the constitution's "Production hardening" list (items 1–3).

### FR-6 — Cloud dispatch via runbook prompt
The agent MUST be told via the runbook system prompt (`/app/prompts/runbook.md` per Clarifications) and the per-case user prompt which cloud the case concerns:
- The user prompt injects `provider=case.provider` (`aws` or `azure`) along with `region` and `instanceId`.
- The runbook has a section per cloud telling the agent to use `aws …` commands when `provider=aws` and `az …` commands when `provider=azure`.

The agent pod's per-cluster install only carries the credentials for that cluster's cloud (per NFR-5):
- AWS-cluster install: `aws` CLI authenticated via IRSA; `az` CLI present but unauthenticated → `az` calls fail.
- Azure-cluster install: `az` CLI authenticated via workload identity; `aws` CLI present but unauthenticated → `aws` calls fail.

Cross-cloud probing is prevented by the credential layer, not by code in the agent.

### FR-7 — No agent-side termination ceiling
Per the constitution's hackathon-scope simplifications, the agent does NOT enforce any of the budgets in the request payload:

| Budget field | Source | Agent behavior |
|---|---|---|
| `case.budgets.maxTurns` | request body | Accepted and echoed best-effort into `status.diagnosis.turnsUsed`. NOT enforced. |
| `case.budgets.maxBudgetUSD` | request body | Accepted and echoed best-effort into `status.diagnosis.costUSD`. NOT enforced. |
| `case.budgets.deadlineSec` | request body | Accepted but ignored. NOT enforced. |

The agent loop terminates on exactly one of:
- `emit_report` called → success path (FR-8).
- Model halts on its own (no more tool calls, no more messages) → write a `Failed` CR with `reason=ModelHalted`.
- Anthropic API error / hard runner exception → write a `Failed` CR with `reason=ModelError` or `ToolError`.

**Two upstream bounds remain in effect** but are not the agent's responsibility:
- *Controller deadline.* Spec 001 FR-5 marks the NHD `Failed` ~60 s after `observedAt` if `phase=Diagnosing` is unchanged. The agent's `Status().Update` may still arrive later — see FR-12 for the race resolution.
- *API key spend cap.* The hackathon-only Anthropic API key has an account-level budget ceiling (§10.1). A genuinely stuck loop fails closed at the provider tier.

A `Failed` case MUST still update the CR — `status.diagnosis.completedAt` and `modelUsed` are filled in even when no diagnosis content was produced — so the controller and demo deck can see what happened. `turnsUsed` and `costUSD` are populated on best-effort from SDK metadata if available. `auditLogRef.objectStore` is left empty (the constitution defers the durable audit JSONL); restoring it is part of the production rollout per the constitution's "Production hardening" list.

**Production restoration:** an agent-side wall-clock deadline with proper cancellation semantics is on the constitution's "Production hardening" list (item 6).

### FR-8 — `emit_report` terminal tool

#### What it is and why it exists

`emit_report` is a **sentinel tool the runner registers with the Claude Agent SDK**. It looks like a normal tool to the model, but the runner intercepts the call instead of forwarding it to a real backend. It is the agent's mechanism for declaring "I'm done; here is my structured answer."

It is invoked exactly once per successful case, by the **agent (LLM)**, as the final tool call in the loop. No human, no external system, no other component invokes it. It exists entirely inside the agent process.

It exists because the runner needs three things at the end of every successful case, and `emit_report` delivers all three from a single point:

1. **A structured payload.** The controller's confidence gate (Spec 001 FR-6) reads `confidence`, counts distinct `evidence[].source` values, and routes on `recommendation.action`. None of those are gateable from free-text. Tool-call args are schema-validated by the SDK; that's the most reliable way to extract structured data from an LLM.
2. **A clean termination signal.** In an autonomous loop the agent decides when it is done. The runner needs an unambiguous "stop now" event; calling `emit_report` is it.
3. **A single point to validate, write, and finalize.** Schema check (FR-8 step 1), CR `Status().Update` with phase-conflict guards (FR-12), case-complete log line — all happen in one place, in one ordering, by the runner.

The runbook prompt instructs the agent: *"When you have gathered enough evidence to form a conclusion, call `emit_report` with the diagnosis. That ends the case. Do not call any other tool after `emit_report`."*

Alternatives considered and rejected for the hackathon shape:

- *Parse the agent's final assistant message as JSON.* Brittle — models drift between markdown fences, prose, partial JSON across rephrasings. Tool-call schemas are deterministic.
- *Use the SDK's structured-output / response-format on a designated final turn.* Forces the runner to know which turn is final; in an autonomous loop only the agent knows.
- *Let the agent write the CR directly via `Bash kubectl patch`.* Technically possible (the agent has `Bash` per FR-4 and the kube SA has `update` on NHD per FR-14), but it loses every safety lever — no schema validation, no FR-12 phase-conflict guard, no single ordered finalize. Catastrophic for production restoration.

Restoration note: `emit_report` is a load-bearing pattern, not a hackathon shortcut. It stays even when the constitution's hackathon-scope simplifications are reverted for production.

#### Behavior

When the agent calls `emit_report`:

1. Validate the payload schema (and only the schema):
   - `rootCause`: non-empty string ≤ 4 KB.
   - `rcaCategory ∈ {Conntrack, FD, PID, Inode, Disk, DNS, IMDS, Kernel, Kubelet, Unknown}`.
   - `confidence ∈ [0.0, 1.0]`.
   - `evidence` is a non-empty list (≥ 1 entry). Each entry has `{source, ref, result, observedAt}` with `source ∈ {nrql, ssh, kubectl, cloud, proc, log}` and `result` ≤ 4 KB (truncated if larger).
   - `recommendation.action ∈ {Cordon, DrainAndCordon, NoAction}`. `recommendation.reason` non-empty.
2. **No agent-side evidence-count gate.** A report with a single evidence entry is accepted and written to the CR. The controller's confidence gate (Constitution Article I.3 / controller spec FR-6) is the single layer that enforces "≥ 2 distinct sources" for auto-cordon. Reports below that bar still produce a CR with full diagnosis content; they just route to `HumanInLoop` instead of cordoning.
3. On valid payload: build the NHD `status.diagnosis` object per `nodemedic-scope.md` §2.2:
   - `modelUsed`, `completedAt` populated by the runner from session state. `turnsUsed`/`costUSD` populated best-effort from SDK metadata if available. `auditLogRef.objectStore` is left empty (no durable audit log; constitution "Production hardening" item 7 to restore).
4. `Status().Update` the existing NHD CR (named `<node-short>-<unix-ts>` per the controller spec; the agent finds it by `caseId` matching `spec.case.caseId`). Use server-side apply with field manager `nodemedic-agent`. The CR is created by the controller before `POST /diagnose` is issued, but apiserver caching plus cross-pod timing means the agent's k8s client cache MAY not have observed the `Create` yet at the moment the agent writes — see the `NotFound` retry policy below.
5. Set `status.phase="Diagnosed"` on the CR.
6. End the loop. Emit `case_complete` metric.

The agent process MUST never call `Create` on NHD — the controller is the sole creator.

If `Status().Update` fails, the runner MUST retry per the failure class:

| Failure class | Retry policy |
|---|---|
| `NotFound` (the CR isn't visible to the agent's k8s client cache yet — controller `Create` race) | **3 attempts at 250 ms / 500 ms / 1 s.** Absorbs the structural race between the controller's `Create` and the agent's first cache observation. Per the Clarifications binding. |
| Conflict (`409`) on the status subresource | Retry once after 1 s with a fresh read of the CR (server-side-apply field-manager semantics handle merge). |
| Other apiserver errors (5xx, transient network) | Retry once after 1 s. |
| RBAC denied (`403`) or schema validation (`422`) | No retry — terminal. |

If retries are exhausted (or the failure class is terminal): mark the case `Failed` with `reason=CRWriteFailed`, log the error verbatim, and return.

The runbook prompt MUST instruct the agent to:
- Reflect evidence quality in `confidence` — single-source or contradictory evidence should yield a value below 0.7.
- Choose `recommendation.action="NoAction"` when evidence is too thin to recommend cordoning.

This pushes the "is this enough to cordon?" judgment to the controller's gate, where it belongs, and keeps the agent's job to "report what you found, calibrated honestly."

### FR-9 — (Removed)

The constitution defers the durable audit JSONL to the production hardening track. The tool-call recording requirement now lives in NFR-3 (structured stdout logs); the per-tool-call hook is wired in FR-3 (`hooks=[("PreToolUse", tool_log_hook)]`). To restore the durable JSONL for production: see the constitution's "Production hardening" list (item 7) — re-add this FR with the JSONL-on-PVC pattern, the `auditLogRef.objectStore` CR field, and the case-finalize fsync.

### FR-10 — Idempotent `POST /diagnose`
On receipt the agent MUST consult an in-memory case table keyed by `caseId`:

| Existing state | Behavior |
|---|---|
| not present | enqueue a new worker task; return `202 {status:"queued"}`. |
| `queued` or `running` | return `202 {status:"queued"}`. Do NOT start a second loop. |
| `complete` (within retention window) | return `202 {status:"complete"}`. The controller will see the result via the CR informer. |

Retention window: 30 minutes per case after completion. After that, a re-POST starts a fresh loop. The 30-minute window comfortably covers controller retries (controller spec FR-7 retries within ~120 s) and any operator-driven replay during a demo rehearsal block.

The case table MUST survive in-process restarts of `ClaudeSDKClient` for a single case but MAY be lost on agent pod restart — the controller's retry path covers that.

### FR-11 — Failure handling and CR finalization
Every case MUST end in exactly one of two states:

| Outcome | CR mutation |
|---|---|
| Success (FR-8 succeeded) | `status.phase="Diagnosed"`; full `status.diagnosis` populated; `conditions[ReportReady]=True` with `reason=EvidenceValid`. |
| Failure | `status.phase="Failed"`; partial `status.diagnosis` (`modelUsed`, `completedAt`, `auditLogRef`; `turnsUsed`/`costUSD` populated on best-effort if SDK metadata exposed them); `conditions[ReportReady]=False` with `reason ∈ {ModelHalted, ModelError, ToolError, CRWriteFailed}`. |

The controller is responsible for deciding what to do with `Failed` (per its FR-7). The agent MUST not retry mid-case and MUST not re-issue the report.

Note: prior drafts of this spec carried `TurnsExhausted`, `BudgetExhausted`, `InsufficientEvidence`, and `DeadlineExceeded` as failure reasons. Each was removed when the constitution dropped its corresponding enforcement (per-turn caps, per-cost caps, agent-side evidence-count gate, agent-side wall-clock deadline). `ModelHalted` was added to capture the case where the model returns no further tool calls / messages without ever invoking `emit_report`. All removed reasons return when their corresponding enforcement returns for production rollout.

### FR-12 — Idempotent diagnosis writes (and the controller-deadline race)
The agent's `Status().Update` MUST be idempotent and MUST defer to any prior terminal decision on the CR. Before writing, the runner MUST re-read the CR and inspect `status.phase`:

| Observed phase before agent write | Agent behavior |
|---|---|
| `""` or `Pending` or `Diagnosing` | Proceed with the write. This is the normal path. |
| `Diagnosed` | Do NOT overwrite. A previous successful write reached the apiserver but the response was lost (network blip, agent pod killed mid-write). The existing CR is authoritative. |
| `Acted` | Do NOT overwrite. The controller has already gated and acted; mutating now would invalidate its decision. |
| `Failed` | Do NOT overwrite. **This covers the controller-deadline race introduced by FR-7's no-agent-side-deadline rule:** the controller's deadline (Spec 001 FR-5) marked the case `Failed` while the agent was still working. The agent's late success or late failure does NOT update the CR. The structured stdout logs (NFR-3) are the only record of what the agent ultimately did — durable for the lifetime of the pod / log-rotation window per the constitution's hackathon-scope simplifications. |

In all no-overwrite cases, the runner MUST log the deferred write at INFO with `caseId`, the observed `phase`, and what it was about to write. The runner MUST exit normally — no error metric, no `Failed` self-report, since the CR's terminal state was set by someone else.

This rule is necessary because FR-7 lets the agent outlive the controller's deadline. Without it, a slow but successful agent would silently overturn the controller's `HumanInLoop` Slack message and an operator would be confused.

### FR-13 — (Removed)

The credential-layer verification job (programmatic `auth can-i` / `--dry-run` checks against kube SA, AWS IAM, Azure RBAC, NR token) is deferred for hackathon scope. Constitution Article I.1 still binds — the credentials MUST be scoped read-only — but verification is by chart review (captains eyeball the rendered `Role`/`ClusterRole`/IRSA policy/Azure role assignment before installing) rather than a post-install test job.

This is a deliberate trade-off:
- *Saved:* a multi-target test job (kubectl + aws + az + NR), Helm post-install hook plumbing, and the operational complexity of a job that must run before the first case is processed.
- *Lost:* programmatic detection of credential drift. If someone misconfigures the Helm `values.yaml` to bind a wider role, nothing alerts before the agent starts running. The non-production cluster surface (Article I.5) limits the blast radius if drift happens.

Production restoration: restore this FR with the verification job described in earlier drafts (`auth can-i create pods` → `no`, `aws ec2 terminate-instances --dry-run` → `UnauthorizedOperation`, `az vm delete` → `AuthorizationFailed`, NR query against account `2` → unauthorized). It SHOULD be a Helm `post-install` Job that exits non-zero on any unexpected outcome.

A unit-test surface for rejecting structurally-dangerous calls (`rm -rf /`, `kubectl delete node`, `/etc/shadow` reads via the structured allow-list) is also deferred — see the constitution's "Production hardening" list (item 4).

### FR-14 — In-cluster CR client
The agent MUST use the in-cluster Kubernetes service account, with RBAC limited to:
- `nodehealthdiagnosisais` (NHD): `get,list,watch,update` (status subresource on `update`).
- `nodes`: `get,list,watch` (read-only — for evidence gathering only, never patch).
- `pods`: `get,list,watch`.
- `events`: `get,list,watch`.

Anything broader is forbidden. The Helm chart MUST fail to install with a wider role.

### FR-15 — Independent in-cluster deployment
The agent runs in the **same** target cluster as the controller, but as an **independent Deployment + Service**, not as a sidecar in the controller's pod and not on `interlinked`. Implications:

- The agent has its own Deployment (`nodemedic-agent`), its own ServiceAccount, its own RBAC, its own Service (`nodemedic-agent.<ns>.svc:8080`), its own resource limits, and its own restart/rollout lifecycle.
- The controller reaches the agent over the in-cluster Service.
- Both reach the apiserver directly for CR reads/writes (no shared filesystem, no shared process state — per Constitution Article II.1, the only cross-scope channels are the CRD and `POST /diagnose`).
- The agent MUST NOT need cross-cluster credentials. One Helm install per target cluster.
- Restart of the controller does not restart the agent and vice versa.

(Decision bound 2026-06-12; centralized hub deployment on `interlinked` is post-hackathon.)

### FR-16 — SSH path (hackathon-only)
For any SSH probe the agent runs via `Bash`:
- A single hackathon-scoped SSH key pair is generated out-of-band and the **private** key is mounted as a Secret on the agent pod (`/etc/nodemedic/ssh/id_ed25519`, mode `0600`). Path exposed as `$SSH_KEY_PATH`.
- The matching public key is provisioned on the target cluster's worker AMIs / cloud-init out of band — confirmed before Day 1 for whichever non-production cluster the demo runs against.
- Direct TCP from the agent pod to node IP, port 22 (non-production cluster network policies allow this).
- No bastion, no SSM, no Azure Bastion, no AAD-SSH in v1.
- `StrictHostKeyChecking=no` is acceptable for the demo; a `known_hosts` populated at deploy time is preferred and MAY be added without a spec change.
- The runbook prompt instructs the agent to invoke SSH as `ssh -i $SSH_KEY_PATH -o StrictHostKeyChecking=no <user>@<node-ip> '<cmd>'`. Resolving the node IP is the agent's job (typically `kubectl get node -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'`).

(Decision bound 2026-06-12. Production posture requires SSM / Bastion and is post-hackathon.)

---

## 6. Non-functional requirements

### NFR-1 — Latency (informational, not a contract)
The agent's `POST /diagnose` is async — the controller's reconcile loop does not block on diagnosis latency, and the CR informer picks up the result whenever the agent finishes. Therefore:

- **No agent-side ceiling on diagnosis duration.** Per FR-7 and the constitution, the agent runs as long as it needs to. There is no hard cutoff to fail against.
- **The controller's deadline (Spec 001 FR-5, ~60 s) bounds the user-visible case duration**, but it does not cancel the agent. The agent may still complete after the controller marks the NHD `Failed`; FR-12 governs that path.
- **Soft targets** (used to size resource limits and observability dashboards, not to gate `Failed`/`Acted`):
  - `POST /diagnose` receipt → `202` returned: a few seconds is fine; queue-time is not a correctness property.
  - Loop start → `emit_report`: the demo shape is ~30–50 s on `ContainerRuntimeUnhealthy`-class faults. Not a contract; can be longer for harder cases. The 90 s containerd-shadow window in the chaos cronjob means the agent should observe the fault while it is still active if investigation lands within the first ~60 s; later investigations may see only the recovery transition.
  - CR write after `emit_report`: ~1–2 s typical.

A case that runs 90 s, 5 min, or longer is not a defect — it's an agent doing the work the demo is meant to showcase. The right operator response is to read the structured stdout logs and the eventually-written diagnosis (subject to FR-12), not to add a timeout.

### NFR-2 — Availability
Single-replica deployment is acceptable for v1. Survive transient apiserver hiccups (built-in retries via the k8s Python client). Multi-replica is a stretch; if added, only one replica processes a given `caseId` at a time (lease or sticky routing — out of scope for v1).

### NFR-3 — Observability
Structured logs (JSON) on stdout are the **only** observability surface for the hackathon (the constitution defers the durable audit JSONL). Every loop turn MUST log:

- `caseId`, `turn`, `tool` (`Bash` / `nr.<name>` / `emit_report` / SDK built-in name), `latency_ms`.
- For `Bash` turns: the literal `command` string (truncated to 2 KB if longer), the exit code, stdout/stderr size in bytes (not contents — log volume).
- For NR MCP / SDK built-in / `emit_report` turns: the `args` object (truncated to 2 KB if larger).

INFO for happy path, WARN for retries / non-zero `Bash` exits, ERROR for terminal failures.

Operators retrieve a case's full tool-call trace during the demo via `kubectl logs <agent-pod> --since=10m | jq 'select(.caseId=="<caseId>")'`. Durability is bounded by the pod's lifetime and the cluster's log-rotation policy — gone after pod restart. This is the constitution's hackathon-scope trade-off; restoring per-case durable JSONL is on the constitution's "Production hardening" list (item 7).

**Prometheus `/metrics` is OPTIONAL** (per FR-2). If implemented, the recommended metric set is:
- `nodemedic_agent_cases_total{outcome="success|failed"}`
- `nodemedic_agent_failure_reason_total{reason=...}` (one of the FR-11 reasons)
- `nodemedic_agent_latency_seconds{phase="receive|loop|cr_write"}` (histogram)
- `nodemedic_agent_tool_calls_total{tool="Bash|nr.<name>|emit_report"}`
- `nodemedic_agent_inflight_cases` (gauge)
- `nodemedic_agent_queue_depth` (gauge)
- `nodemedic_agent_build_info{sha=...,version=...}` (gauge, value `1`)

Per-turn cost / token usage histograms were removed when the constitution dropped per-case caps. They will return when the caps return for production rollout.

### NFR-4 — RBAC and credential-layer least privilege
Per FR-14 (kube SA scoping). Programmatic verification of all credential layers (kube SA, AWS IAM, Azure RBAC, NR token) is deferred for hackathon scope per FR-13; verification is by chart review until production restoration. The constitution makes credential-layer least privilege the **primary** safety boundary, not a layered defense. The Helm chart MUST:
- Render only the Role/ClusterRole verbs in FR-14. README must state this explicitly so any drift is reviewable.
- Mount only one cloud's credentials per install — AWS-cluster install does NOT have Azure credentials, and vice versa (per FR-6).
- ~~Run the FR-13 post-install verification job before declaring the install ready.~~ Deferred for hackathon scope (FR-13 is removed); restore for production.

### NFR-5 — Cloud parity (per-install credentials)
Same image, same Helm chart, one install per target cluster. Per-cluster Helm values:
- `agentService.nrAccountId` — locked at `1` for both clusters.
- `cloud.provider` — `aws` or `azure`. Determines which CLI's credentials are mounted.
- AWS install: assume the agent pod's IRSA role has the read-only AWS policy (`ec2:DescribeInstanceStatus`, `ec2:DescribeInstances`, `health:DescribeEvents`, EC2 Scheduled Events read). The `az` CLI is present on the image but unauthenticated.
- Azure install: SP client-secret in `Secret/nodemedic-azure-creds` (decided per §10.3 — workload identity is the production-restoration path, not the hackathon shape). The `aws` CLI is present on the image but unauthenticated. The SP MUST have Reader on the cluster's resource group; nothing wider.

The agent image MUST be identical on both clusters; the only difference is the credential set the Helm install mounts. Cross-cloud probing (e.g. agent on AWS install runs `az …`) is prevented by the credential layer alone — there is no code-level guard.

### NFR-6 — Configuration surface
All deploy-time parameters are env vars on the Deployment:

| Env | Default | Purpose |
|---|---|---|
| `AGENT_LISTEN_ADDR` | `:8080` | bind addr |
| ~~`AGENT_SHARED_TOKEN`~~ | (removed) | hackathon scope has no `/diagnose` auth; restore for production |
| `MAX_CONCURRENT_CASES` | `32` | concurrency cap; `429` when full |
| ~~`DEADLINE_SEC_DEFAULT`~~ | (removed) | hackathon scope has no agent-side deadline (FR-7); restore for production. |
| `MODEL` | `claude-opus-4-7` | primary model — pinned to 1M-context variant (FR-3). Exact SDK invocation TBD in plan. |
| `MODEL_CONTEXT_VARIANT` | `1m` | selects the 1M-context flavor of the primary model. Plan resolves this to whatever knob the SDK actually exposes (model-ID suffix, beta header, or distinct ID). |
| `FALLBACK_MODEL` | `claude-sonnet-4-6` | SDK fallback (see §10.9 — fallback context-window choice is open). |
| `ANTHROPIC_AUTH_TOKEN` | (required) | nerd-completion gateway token, mounted from `Secret/nodemedic-anthropic-token`. Sourced from Vault: `containers/teams/nova/staging/nova-service/NERD_COMPLETION_API_TOKEN` (reusing Nova's path for the hackathon). |
| `ANTHROPIC_BASE_URL` | `https://nerd-completion.staging-service.nr-ops.net` | nerd-completion endpoint. Plan confirms staging vs prod URL at deploy. |
| `CLAUDE_MODEL` | `claude-opus-4-7` | primary model (requested ID). SDK's standard env var. Resolved at startup against the gateway catalog per FR-3's fallback chain; if absent, the runner falls back to best-available Opus → best-available Sonnet → fails `/readyz`. Resolved ID logged at startup. |
| `CLAUDE_FALLBACK_MODEL` | `claude-sonnet-4-6` | fallback model (requested ID). Same gateway-availability resolution as `CLAUDE_MODEL`. |
| `NR_MCP_URL` | (required) | NR HTTP MCP base URL |
| `NR_MCP_TOKEN` | (required) | NR MCP auth — mounted from `Secret/nodemedic-nr-token` |
| `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` | (required on Azure install) | mounted from `Secret/nodemedic-azure-creds` |
| `NR_ACCOUNT_ID` | `1` | hard-coded; configurable but locked at deploy |
| ~~`AUDIT_DIR`~~ | (removed) | hackathon scope has no durable audit JSONL; restore for production |
| `SSH_KEY_PATH` | `/etc/nodemedic/ssh/id_ed25519` | mounted from `Secret/nodemedic-ssh-key` |
| `LOG_LEVEL` | `info` | |
| `READYZ_PROBE_TIMEOUT_SEC` | `5` | startup probe of NR + Anthropic |

Secrets are mounted from Kubernetes Secrets, never baked into the image.

### NFR-7 — Observability (hackathon: ephemeral, restored to durable in production)
Every tool call MUST appear in the structured stdout logs (NFR-3). For the lifetime of the pod and within the cluster log-rotation window, `kubectl logs` is the agent's reasoning trace.

The CR's `status.diagnosis.evidence[]` MUST be sufficient to reconstruct the agent's *conclusion* — citations of what evidence supported the diagnosis. The CR is durable (apiserver-backed), the logs are not. Production restoration brings back the per-case JSONL on PVC plus `auditLogRef.objectStore` on the CR (constitution "Production hardening" item 7).

### NFR-8 — Resource footprint

**Expected load shape** (per Clarifications binding):
- **Typical demo**: 1 concurrent case (operator drives one fault injection at a time, watches it through, then moves on).
- **Peak rehearsal**: 10 concurrent cases (hand-crafted storm — operator injects faults across multiple nodes back-to-back to stress-test the agent under load).

NFR-8's sizing keeps 22 cases of headroom above peak rehearsal so the storm scenario doesn't bump up against the cap; controller-side debounce (Spec 001 FR-1, 30 s per `(node, condition)`) plus operator-driven fault injection mean we never expect to *organically* hit the 32-case cap.

**Hackathon target:**
- **Memory**: request `1 Gi`, limit `3 Gi`. Idle baseline is ~400 MB (CLI bundle: `aws` v2 ~150 MB resident, `az` ~200 MB on first invocation, Python runtime + agent + SDK ~100 MB). Each active case adds ~75 MB (SDK session + Bash subprocess buffers + 4 KB-truncated tool result buffers).
  - At typical load (1 case): ~475 MB peak — well below the 1 Gi request, no spillover.
  - At peak rehearsal (10 cases): ~1.15 GB peak — slight burst above the 1 Gi request, comfortably inside the 3 Gi limit.
  - At cap (32 cases — not expected to occur during hackathon): ~2.8 GB ceiling, still inside the 3 Gi limit. Sized for safety margin, not as a target.
- **CPU**: request `1`, limit `4`. The agent is mostly I/O-bound (waiting on Anthropic, NR MCP, SSH, cloud APIs) — CPU bursts come from JSON parsing and SDK message handling.
- **PVC**: not required for hackathon scope (the constitution defers the durable audit JSONL). `emptyDir` is fine for any scratch the agent writes via SDK built-in `Write`/`Edit`. Production restoration adds back a `2Gi` PVC for `/var/lib/nodemedic/audit`.

No empirical baseline yet; revisit after Day 1 evening. If 32-case storms in rehearsal turn out to need more headroom, raise the limit before raising the cap.

### NFR-9 — Image size and build
Single Python image, multi-stage build. Target ≤ 1.5 GB (the CLI bundle is unavoidable given the `Bash`-driven tool surface):
- `aws` CLI v2: ~150 MB.
- `az` CLI: ~250 MB.
- `kubectl` 1.33: ~50 MB.
- Python runtime + agent code + SDK: ~300 MB.

Runtime deps pinned (`pyproject.toml`). CLI versions pinned (per §10.8). The canonical runbook (`prompts/runbook.md` in the agent repo) is copied to `/app/prompts/runbook.md` at build time per the Clarifications binding — runbook content is part of the image, not a runtime mount, so the image SHA pins the runbook version. Image MUST be reproducibly built (no random uuids in layers, no per-build timestamps in any non-OCI label).

The 1.5 GB target is larger than would be ideal for production — accepted because the constitution's hackathon-scope simplification chose `Bash` + bundled CLIs over in-proc `@tool` Python wrappers around `boto3`/`azure-mgmt-compute`, which is a substantial Day-1 work saving despite the larger image.

---

## 7. The two cross-scope contracts (binding)

The constitution forbids any cross-scope channel besides these two. This spec produces them; the controller spec consumes them.

### 7.1 NodeHealthDiagnosisAI CRD
Schema as defined in `nodemedic-scope.md` §2.2. The agent:
- **Owns the schema** — Constitution Article II.2. CRD changes go through this scope's PR.
- **Writes:** `status.diagnosis.*`, `status.phase` (only the `Diagnosing → Diagnosed` and `… → Failed` transitions), `status.conditions[ReportReady]`.
- **Reads:** `spec.case.*`, `spec.budgets.*` (from the controller).
- Never writes `spec.*`. Never writes `status.action.*` (controller's territory). Never writes `status.evaluation.*` (out of scope).
- Never `Create`s NHD objects — only `Update`s the existing CR identified by `caseId`.

The CRD definition (Go types or OpenAPI YAML) lives in this scope's repo. The controller imports/renders it.

### 7.2 POST /diagnose
Request and response shape as defined in `nodemedic-scope.md` §3.1, §3.2, §3.3. The agent:
- Returns `202` for queued/running/complete cases.
- Returns `400` for malformed bodies.
- ~~Returns `401` for bad/missing tokens.~~ Auth disabled for hackathon scope (FR-1); the agent does not return 401. Contract-level shape preserved for the controller's reference and for production restoration.
- Returns `429` when at concurrency cap.
- Returns `5xx` only on internal panics — never as a "soft retry me" signal.

Idempotency is the agent's responsibility (FR-10). The controller is allowed to assume that retrying with the same `caseId` is safe.

---

## 8. Acceptance criteria (the gate for "v1 ships")

Each item is independently demonstrable on the demo cluster:

- [ ] **AC-1** Apply CRD + install Helm chart on **cf1z**. `kubectl --context=cf1z get deploy nodemedic-agent -n container-fabric` shows 1/1 ready. `/healthz` and `/readyz` return 200. (`/readyz` includes the model-resolution check from FR-3 — the resolved primary + fallback model IDs MUST be in the startup log.)
- [ ] **AC-2** `curl -X POST :8080/diagnose -d @case.json` (no auth header — hackathon scope has no auth per FR-1) returns `202 {status:"queued"}`. Missing field → `400`. Wrong `provider` enum → `400`. Calls with an `Authorization: Bearer …` header are accepted (header is ignored, not rejected). (No latency assertion — async endpoint.)
- [ ] **AC-3** Trigger the `chaos-containerd-unhealthy` cronjob on cf1z's canary node (`kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy chaos-test-manual -n default`). Within ~30 s of the bind-mount, NPD's `check-containerd.sh` flips `Node.status.conditions[ContainerRuntimeUnhealthy]=True` with `reason=ContainerdUnreachable`; the controller creates an NHD CR and calls `POST /diagnose`. The agent eventually `Update`s `status.diagnosis` with `confidence ≥ 0.7`, `evidence[]` containing **at least 2 distinct sources**, `recommendation.action=Cordon`, and `phase=Diagnosed`. "Eventually" has no agent-side time bound (FR-7); for the demo we expect ~30–50 s but a 90 s case still passes this AC. The case passes this AC if the diagnosis lands before the controller marks the CR `Failed` (Spec 001 FR-5, ~60 s); if it lands later, FR-12 governs the write suppression and AC-7 covers that late-write path.
- [ ] **AC-3b** Trigger the `chaos-kubelet-unhealthy` cronjob on the same canary node (`kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy chaos-kubelet-test-manual -n default`). Within ~30 s of the iptables REJECT installing on `127.0.0.1:10248`, NPD's `check-kubelet-healthz.sh` flips `Node.status.conditions[KubeletUnhealthy]=True` with `reason=KubeletHealthzFailed`; the controller creates an NHD CR and calls `POST /diagnose`. The agent's diagnosis (per same rules as AC-3) cites at least one NRQL row, one SSH probe (e.g. `journalctl -u kubelet --since "5 min ago"`), and one cloud-side or kube-side probe (e.g. `kubectl get events --field-selector involvedObject.name=<canary>`). After the 90 s rule-removal window, NPD recovers `KubeletUnhealthy` to `False` with `reason=KubeletIsHealthy`. **Verified end-to-end on cf1z 2026-06-14** via the live chaos cronjob test (NPD detection latency ~10 s after rule install, ~9 s after rule removal).
- [ ] **AC-4** Cloud parity is naturally satisfied by AC-3 — cf1z is itself an Azure kubeadm cluster, so the AC-3 happy path exercises the Azure backend (`provider=azure`, `az` CLI, Azure Resource Health). For an explicit AWS-side demonstration, repeat AC-3's pattern on a `test-*` EKS cluster with an equivalent fault injector once one ships (no AWS chaos cronjob exists today). Until then, the same image, same chart, same runbook serving cf1z is the binary-portability evidence.
- [ ] ~~**AC-5** Credential-layer verification (FR-13)~~. **Removed** in this iteration (FR-13 deferred for hackathon scope). Captains MUST eyeball the rendered Helm `Role`/`ClusterRole`/IRSA policy / Azure role assignment / NR token scope before each install — manual review replaces the verification job. AC returns when FR-13 returns for production.
- [ ] **AC-6** `emit_report` with a **single evidence source** is accepted (schema-valid; FR-8 has no count gate). The CR is written with the single-source `evidence[]`. Downstream, the controller's confidence gate routes the case to `HumanInLoop` rather than cordoning. Verifies the redundant agent-side gate has been removed and the controller-side gate is the sole enforcer.
- [ ] **AC-7** Late-write race: hand-craft a case where the controller marks the NHD `Failed` (e.g. by directly patching `phase=Failed` while the agent is mid-loop). When the agent eventually calls `emit_report`, FR-12 detects the existing `Failed` phase and refuses to overwrite. The CR retains `phase=Failed` with the controller's reason. Structured stdout logs (NFR-3) record the agent's tool calls and the deferred write decision (INFO log line with the observed phase) — visible via `kubectl logs` while the pod is up. No agent-side metric reports a failure.
- [ ] **AC-8** Idempotency: `POST /diagnose` twice with the same `caseId` while the first is still running. Second call returns `202 {status:"queued"}` without starting a second loop. Verified by `kubectl logs | jq 'select(.caseId=="<caseId>")'` containing exactly one `case_started` log line.
- [ ] **AC-9** Tool-call observability: after a successful case, run `kubectl logs <agent-pod> --since=10m | jq 'select(.caseId=="<caseId>")'`. Every tool call from the loop is present — every `Bash` invocation (with `command` field), every NR MCP call (with `args` field), every SDK built-in call. The case-complete log line appears last.
- [ ] **AC-10** Concurrency cap: with `MAX_CONCURRENT_CASES=2` (test-only override; production default is `32`), post 3 cases simultaneously. Third returns `429`.
- [ ] **AC-11** Kube RBAC: Helm install with a wider role fails (chart README check / pre-install hook). Agent's actual role permits only the verbs in FR-14.
- [ ] **AC-12** *(optional, only if `/metrics` is implemented per FR-2)* Prometheus metrics in NFR-3 are populated with non-zero values after running the demo case end-to-end. If `/metrics` is not implemented, this AC is N/A and the demo deck relies on structured stdout logs alone for observability evidence.
- [ ] **AC-13** Cross-cloud probing prevented by credentials: on the AWS install, the agent running `az vm get-instance-view …` fails (no Azure credential mounted). On the Azure install, `aws ec2 describe-instance-status …` fails. Verifies FR-6's credential-layer-only enforcement of cloud dispatch.
- [ ] **AC-14** Case isolation: run two cases back-to-back against `cf1z-general-nodes-2000002` — case A via `kubectl create job --from=cronjob/chaos-containerd-unhealthy …` (trigger `ContainerRuntimeUnhealthy`), then case B via `kubectl create job --from=cronjob/chaos-kubelet-unhealthy …` (trigger `KubeletUnhealthy`) once case A finishes. Filtering `kubectl logs | jq 'select(.caseId=="A")'` and `... select(.caseId=="B")` returns disjoint trees of tool calls. Case B's diagnosis MUST cite only evidence about case B's fault and MUST NOT reference case A's fault or any tool result observed during case A. Verifies G2 / FR-3 isolation guarantee.
- [ ] **AC-15** Runbook readiness checklist (gates AC-3): the canonical runbook at `prompts/runbook.md` (per Clarifications) MUST contain the seven binding clauses below, verified by a build-time lint (a `tests/check_runbook.sh` script in the agent repo, run by CI and by the Helm pre-install Job — exact wiring is plan-phase). The lint MUST exit non-zero and block AC-3 from being claimed if any clause is missing. Required clauses:
  1. **Cloud-dispatch sections** — separate `aws …` and `az …` command guidance keyed off `case.provider` (per FR-6).
  2. **Evidence calibration paragraph** — instructs the agent to lower `confidence` below 0.7 when evidence is single-source or contradictory (per FR-8).
  3. **Recommendation rubric** — instructs the agent to choose `recommendation.action="NoAction"` when evidence is too thin to recommend cordoning (per FR-8).
  4. **Credential-path forbid list** — explicitly forbids reading `$SSH_KEY_PATH`, `/etc/nodemedic/ssh/*`, `/var/run/secrets/**`, and any other Secret-mounted credential path (per FR-4 §4 and §11 risks).
  5. **NRQL account-ID instruction** — instructs the agent to set `account_id=1` (staging) on every NR MCP call (per FR-4 §2).
  6. **`emit_report` calling discipline** — instructs the agent to call `emit_report` exactly once per case as the final tool call, and to call no other tool after it (per FR-8 prose).
  7. **Decision-tree sections** for each cf1z-deployed NodeCondition — `ContainerRuntimeUnhealthy` and `KubeletUnhealthy` (per Clarifications binding). Each section names: which probes to run first, what evidence to gather, what's a slam-dunk vs ambiguous diagnosis, and which `rcaCategory` to emit. The §4.2 fault classes (Conntrack, FD, PID, Inode, Disk-fill, DNS, IMDS) MAY be present as stub sections noting "not currently produced by any deployed NPD plugin" — Day-2+ runbook expansion turns these into full decision trees as Scope 1's plugins land.

---

## 9. Demo flow (how this spec earns its keep)

**Demo target: cf1z + `ContainerRuntimeUnhealthy`.** Per Clarifications binding. cf1z is the active hackathon Azure kubeadm cluster (k8s 1.33.8). Canary node `cf1z-general-nodes-2000002` is already labeled `canary-chaos-test=true`. Harrison's `chaos-containerd-unhealthy` cronjob is deployed in `default` namespace.

1. Operator triggers a manual chaos run: `kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy chaos-test-manual -n default`.
2. The chaos pod finds the `hack-node-problem-detector` pod on the canary node, `kubectl exec`s into it, and bind-mounts a regular file over `/host/run/containerd/containerd.sock` (the existing NPD pod's container view of the host socket — host socket is untouched). Shadow holds for 90 s.
3. Within ~30 s of shadow activation, NPD's `check-containerd.sh` plugin (30 s probe interval) sees `! -S /host/run/containerd/containerd.sock`, exits 1, and NPD flips `Node.status.conditions[ContainerRuntimeUnhealthy]=True` with `reason=ContainerdUnreachable`.
4. Controller (Spec 001) sees the condition flip via informer, debounces 30 s per `(node, condition)`, creates the NHD CR, and `POST`s `/diagnose`. Agent returns `202` promptly; pod log shows `case_received caseId=…`.
5. Live tail of the agent pod log streams the loop. Illustrative tool sequence (the agent picks the actual order):
   - `Bash: kubectl get node cf1z-general-nodes-2000002 -o yaml | yq '.status.conditions'` — confirm the trigger.
   - `nr.execute_nrql_query` against account `1`: `SELECT * FROM K8sNodeSample WHERE clusterName='cf1z' AND nodeName='cf1z-general-nodes-2000002' SINCE 15 minutes ago`.
   - `Bash: ssh -i $SSH_KEY_PATH -o StrictHostKeyChecking=no <user>@<canary-internal-ip> 'ls -la /run/containerd/containerd.sock; pgrep -fa containerd; journalctl -u containerd --since "5 min ago" --no-pager | tail -20'` — verifies the host socket vs the container's view (a clue for the runbook to interpret: host socket is fine but the NPD pod's view is shadowed).
   - `Bash: az vm get-instance-view --name <vm-name> --resource-group <rg>` — Azure-side instance health.
6. Eventually (typically ~30–50 s; can be longer): `emit_report` accepted; CR `status.diagnosis` updated; `phase=Diagnosed`. If the agent runs past the controller's ~60 s deadline, the controller posts a "needs human review" Slack message and the agent's eventual write is suppressed by FR-12.
7. Controller sees the update via informer, applies confidence gate, cordons.
8. `kubectl --context=cf1z get nhd <name> -n container-fabric -o yaml` shows: `case` populated by controller, `diagnosis` populated by agent, `action` populated by controller. Three scopes, one CR, full trace.
9. `kubectl --context=cf1z logs <agent-pod> -n container-fabric --since=2m | jq 'select(.caseId=="<caseId>")'` — every probe shown with its full command string. (No durable JSONL for hackathon scope; the demo captures this output during the live run for the deck.)
10. Wait ~60 s after the chaos pod's `umount` — `ContainerRuntimeUnhealthy` flips back to `False` with `reason=ContainerRuntimeIsHealthy`. The cordon does NOT auto-uncordon (Spec 001 self-heal behavior is intentional); operator manually uncordons after reviewing the diagnosis.

For the **gate-fail rehearsal**: hand-craft a case where the runbook prompts the agent to gather evidence that comes back empty, then have the agent emit a low-confidence report. Controller's confidence gate fails (confidence < 0.7) and routes to HumanInLoop.

For the **controller-deadline race demo**: directly patch the CR to `phase=Failed` while the agent is mid-loop, then watch the agent's eventual `emit_report` get suppressed by FR-12 — `kubectl logs` (while still showing this pod's history) is the only record of the late completion.

**Second condition demo (`KubeletUnhealthy`):** swap step 1's manual job trigger to `kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy chaos-kubelet-test-manual -n default`. Mechanism is different — a privileged `hostNetwork: true` pod installs an `iptables REJECT` on `127.0.0.1:10248` for 90 s instead of bind-mounting a file. From the agent's perspective the only change is `trigger.type=KubeletUnhealthy` and the runbook's decision-tree section that fires (probes target kubelet logs / `journalctl -u kubelet` / kube-side events instead of containerd's). Same auto-recovery shape, same controller cordon decision. The two demo paths together exercise both real Day-1 NodeConditions on cf1z (per AC-3 + AC-3b).

**AWS / EKS path:** not part of the v1 demo. cf1z is Azure kubeadm and the deployed chaos cronjobs target it. An EKS demo requires equivalent fault injectors on a `test-*` cluster (out of scope for hackathon kickoff).

---

## 10. Open questions (need closure before plan)

1. ~~**Anthropic API key.**~~ **Closed.** Use the internal **nerd-completion gateway** (same pattern as `nova/k8s-agent-claude-sdk`). The Claude Agent SDK speaks the standard `ANTHROPIC_*` env-var protocol; pointing `ANTHROPIC_BASE_URL` at the gateway and providing `ANTHROPIC_AUTH_TOKEN` from Vault is the whole integration — no SDK code changes, no Bedrock IRSA, no separate billing. Plan-phase confirms staging vs prod gateway URL at deploy time.
2. ~~**NR MCP staging account stability.**~~ **Closed.** NerdGraph's documented per-account rate limit is **3,000 queries/min** (combined NerdGraph + Insights Query API; per the TDP query-rate-limiting CDD), with `429 + Retry-After` on breach. Our worst-case load is ~30 queries/min (10 concurrent cases × 3 NRQL queries each at peak rehearsal per NFR-8) — **1% of the budget**. Account `1` saturation requires fundamentally different traffic shapes (e.g., a service account with 21k+ authorized accounts driving multi-MB-per-request heap allocation, per the NerdGraph Feb-2026 RCA). NR token is mounted as a Kubernetes Secret per NFR-6.
3. ~~**Azure auth mechanism.**~~ **Closed.** Use **Service Principal client-secret** mounted as a Kubernetes Secret (`Secret/nodemedic-azure-creds`) for hackathon scope. Workload identity is the production-restoration path. NFR-5 / NFR-6 updated.
4. ~~**SSH key provisioning.**~~ **Closed.** Pre-provisioned hackathon-only private key mounted as a Kubernetes Secret (`Secret/nodemedic-ssh-key`). Public half pushed to target node AMIs out-of-band before Day 1. FR-16 / NFR-6 already align with this; manifest template lives under `manifests/` (see §10 footer).
5. ~~**Slack "View audit log" link target — closed (drop the button).**~~ Already closed in an earlier iteration. Hackathon scope has no durable audit log; the controller drops the button. Returns when the durable JSONL is restored for production.
6. ~~**Runbook caching across model swap.**~~ **Closed.** Anthropic's prompt cache is keyed on `(model, cache_control_block_hash)` — different model = different cache namespace. First Sonnet call after fallback pays the full system-prompt cost (~500–1000 input tokens for the ~2 KB runbook). At Sonnet 4.6 input pricing, this is **~$0.003 per fallback event**. Even at 100 fallbacks across the hackathon: $0.30. Cost asymmetry is real but irrelevant at hackathon volume; accepted.
7. ~~**PVC sizing.**~~ Closed for hackathon scope: no PVC required.
8. **CLI versions in the image.** Pin `aws` v2.x, `az` 2.x, `kubectl` matching cluster minor versions. **cf1z is on k8s 1.33.8** (verified live 2026-06-13), so `kubectl` 1.33.x is the safe choice for the hackathon image.

---

**Cross-team flag (not a spec question, tracked for visibility):** Harrison's [PR #129](https://source.datanerd.us/container-fabric/k8s-resource-recommendations/pull/129) (`tests/containerd-unhealthy-cronjob.yaml`) has a stale pod selector — it looks for `app.kubernetes.io/name=node-medic-agent`, but cf1z's deployed DaemonSet is labeled `hack-node-problem-detector`. The cronjob currently *running* in cluster has the corrected label (someone fixed it in-place), so the demo path works — but a fresh cluster bootstrap from the PR YAML would fail. Review comment posted on the PR 2026-06-13.

**Secret manifest templates** for the closed items live at `.specify/specs/002-nodemedic-agent/manifests/` — apply them per cluster install and patch values in. The Helm chart in the plan phase will absorb these as templated values once the chart skeleton lands.

---

## 11. Glossary

- **NHD** — `NodeHealthDiagnosisAI` CRD (this project).
- **MCP** — Model Context Protocol. The only MCP server the agent talks to is the public NR HTTP MCP at `docs.newrelic.com/docs/agentic-ai/mcp/`.
- **`Bash`** — the Claude Agent SDK's built-in shell tool. The agent's primary investigation surface (kubectl, aws, az, ssh, /proc reads). The constitution permits raw `Bash` because credential-layer least privilege bounds blast radius.
- **Credential-layer least privilege** — the constitution's primary safety boundary: the agent's IAM/RBAC/kube SA/NR token are scoped read-only, so the agent cannot mutate the system regardless of what `Bash` command it tries to run. Verified by captain chart-review during hackathon scope; programmatic verification returns with FR-13 for production.
- **Confidence gate** — the controller-side check (Constitution Article I.3 / controller spec FR-6). The agent supplies the inputs; it never gates itself.
- **`emit_report` (terminal tool)** — a sentinel tool the runner registers with the Claude Agent SDK. The agent (LLM) calls it once per successful case to declare "I'm done; here is my structured diagnosis." The runner intercepts the call before it reaches any backend, validates the schema, writes the diagnosis to the NHD CR's `status.diagnosis`, emits the case-complete log line, and ends the loop. It is the agent's only structured way to produce a gateable answer; the controller's confidence gate (Spec 001 FR-6) reads what `emit_report` wrote. See FR-8 for the full rationale.
- **Wall-clock deadline** — *removed for hackathon scope per the constitution.* The agent has no termination ceiling. The controller's deadline (Spec 001 FR-5) bounds *user-visible* case duration but does not cancel the agent. Restore for production per the constitution's "Production hardening" list.
- **Non-production cluster** — any cluster that does not serve customer traffic: `test-*`, designated Azure kubeadm test clusters, or legacy dev clusters (`cf1z`, `jc1z`, `sk1z`). Per Constitution Article I.5, this is the permitted deployment surface for the agent during the hackathon. Forbidden: `stg-*`, `us-*`, `eu-*`.
- ~~**PVC** — PersistentVolumeClaim.~~ Removed for hackathon scope (no durable audit JSONL). Returns when the JSONL is restored for production.
