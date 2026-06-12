# Spec 001 — NodeMedic Controller

**Status:** Draft · v0.1 · 2026-06-12
**Scope:** Scope 2 of the NodeMedic hackathon (Captains: Harrison, Shabeeb)
**Constitution:** [`.specify/memory/constitution.md`](../../memory/constitution.md)
**Companion docs:** [`docs/cf/nodemedic-scope.md`](../../../docs/cf/nodemedic-scope.md) §5; [`docs/cf/nodemedic.md`](../../../docs/cf/nodemedic.md); diagrams in [`docs/cf/diagrams/`](../../../docs/cf/diagrams/)

This spec defines **what the controller does**, not how each line of Go is written. Implementation details that don't change observable behavior are out of scope here and belong in the plan.

---

## 1. Problem statement

Today, when a worker node misbehaves, an engineer notices, finds the right context, and walks logs from memory. MTTR is hours. NodeMedic closes the gap between "NPD raised a `NodeCondition`" and "node is cordoned, evidence is cited, on-call is informed."

The **controller** is the part of NodeMedic that:

1. Notices an NPD-signaled health problem on a Node.
2. Asks the agent (a separate process) to diagnose it.
3. Reads the agent's diagnosis from a Custom Resource.
4. Decides whether to act (cordon) or escalate to a human.
5. Notifies the team.

Without the controller, the agent has no trigger and no actuator. With it, the demo flow in `nodemedic.md` §4 works end-to-end on both EKS and Azure kubeadm test clusters.

---

## 2. Goals (in scope for v1)

| # | Goal | Why |
|---|---|---|
| G1 | Detect a watched `NodeCondition` flipping `True` and create one `NodeHealthDiagnosisAI` (NHD) CR per (node, condition) within **5 s** of the apiserver patch | Core trigger; matches §5.6 DoD |
| G2 | Resolve `provider`, `region`, `instanceId` from the **Node object** (`metadata.labels` + `spec.providerID`) and write them into `spec.case`. NPD itself does not provide these fields (verified, see §7.3). | Agent must not infer the cloud (Constitution Article II.6) |
| G3 | Invoke the agent via `POST /diagnose` per the contract in `nodemedic-scope.md` §3.1 with retries on transient failures | Cross-scope contract, binding |
| G4 | Reconcile the NHD phase machine: `Pending → Diagnosing → Diagnosed → Acted` (`Failed` is terminal) | §5.2 |
| G5 | Apply the confidence gate exactly as defined in Constitution Article I.3 and gate-pass `→ cordon`; gate-fail `→ HumanInLoop` | Auto-cordon must be defensible |
| G6 | Cordon by patching `Node.spec.unschedulable=true` using the controller's ServiceAccount; **never drain, never delete** | Constitution Article I.2 |
| G7 | Post a Slack message to the demo channel for both gate-pass (Applied) and gate-fail (HumanInLoop) outcomes | "Human-in-the-loop" story for the deck |
| G8 | Survive the agent timing out: mark `Failed`, retry once, then page Slack with `severity=critical` framing | User decision (in lieu of PagerDuty for hackathon) |
| G9 | Run on **both** test clusters with no code changes — the same Helm chart, switched only by kubeconfig context | Constitution Article II.7 (one binary, two clouds) |
| G10 | Idempotent across restarts and across duplicate condition flaps within 30 s | Avoid double-cordoning, double-paging |

## 3. Non-goals (explicitly out of v1)

Per Constitution Article III.3 and `nodemedic-scope.md` §9:

- **No drain, no eviction, no pod deletion, no node termination.** Cordon is the only mutation.
- **No automatic uncordon on self-heal.** If `NodeCondition` flips back to `False`, the controller does nothing. Operator validates and uncordons by hand.
- **No eval agent integration.** `status.evaluation` exists in the schema but is not written or read in v1.
- **No PagerDuty.** Slack is the only notifier; below-threshold path uses a `severity=critical` framing in the Slack message.
- **No human-approval UI for cordon.** The confidence gate is the only control between "diagnosed" and "cordoned."
- **No multi-cluster watching from the hub.** One controller per target cluster.
- **No predictive scoring, no historical training data, no multi-region awareness.**
- **No CRD versioning beyond `v1alpha1`.** No conversion webhooks.

If something here moves into scope mid-hackathon, it requires an amendment per the constitution.

---

## 4. Personas & demo scenarios

**Operator (demo driver).** Runs `make inject-conntrack CLUSTER=test-odd-wire`, watches the Slack channel and `kubectl get node`. Expects: NHD CR appears in `kubectl get nhd -A` within 5 s, agent finishes within 60 s, node is cordoned within 65 s, Slack message lands with the RCA and a link to the CR.

**CF on-call (post-hackathon shape).** Receives a Slack message for `HumanInLoop` cases (gate failed, agent failed, or deadline blown). Expects: enough context in the message to decide whether to investigate the node or dismiss the alert.

**Captains reviewing the design.** Expect the controller's behavior to be auditable from the NHD CR alone: every transition has a `condition` entry with `lastTransitionTime` and `reason`.

### Walkthrough — happy path
1. Operator injects conntrack saturation on a node in `test-odd-wire`.
2. NPD's `conntrack-monitor` plugin flips `NodeCondition[ConntrackSaturated]=True`.
3. Controller's Node informer sees the transition, debounces, generates `caseId`, resolves `provider=aws / region=us-east-2 / instanceId=i-0…`, and `Create`s `NodeHealthDiagnosisAI/<node-short>-<unix-ts>` with `status.phase=Pending`.
4. Controller `POST /diagnose` to the agent service, sets `phase=Diagnosing`, condition `AgentInvoked=True`.
5. Agent runs its loop, then `Update`s `status.diagnosis` and `phase=Diagnosed`.
6. Controller's NHD informer wakes the reconciler. Confidence gate passes (`confidence=0.85`, 3 distinct sources, action=`Cordon`).
7. Controller patches `Node.spec.unschedulable=true`, sets `phase=Acted`, `action.decision=Applied`, condition `ActionApplied=True`.
8. Slack message posts to `#nodemedic-demo` with header `NodeMedic: <node> – Conntrack (conf 0.85)`, RCA paragraph, View CR + View audit log buttons.

### Walkthrough — gate failure
Same as above through step 5. At step 6 the confidence gate fails (e.g. `confidence=0.5`).
- Controller does **not** cordon.
- `phase=Acted`, `action.decision=HumanInLoop`.
- Slack message posts with `Needs human review` framing and the same evidence summary.

### Walkthrough — agent timeout
Steps 1–4 as above. Agent does not write `phase=Diagnosed` before `spec.budgets.deadline`.
- Reconciler sees deadline exceeded, sets `phase=Failed`, condition `AgentInvoked=False, reason=DeadlineExceeded`.
- Controller calls `POST /diagnose` once more with the same `caseId` (idempotent on the agent side).
- If the second attempt also fails: terminal `Failed`, Slack post with `severity=critical` framing.
- Node is **not** cordoned.

---

## 5. Functional requirements

### FR-1 — Watched-condition trigger
The controller MUST run an informer on `Node` and `Event`. It MUST trigger case creation only when:
- The transition is on a `NodeCondition.type` listed in the `--watched-conditions` flag.
- `status` transitioned to `True` (False→True or Unknown→True).
- The transition is not within 30 s of a previous transition for the same `(node, condition)` pair (debounce).

The default value of `--watched-conditions` MUST cover the 7 fault classes from `nodemedic-scope.md` §4.2 (`ConntrackSaturated`, `FDExhaustion`, `PIDExhaustion`, `InodeExhaustion`, `DiskFill`, `DNSPartition`, `IMDSThrottle`).

### FR-2 — Cluster-metadata resolution

NPD's signal carries only `(type, status, reason, message, transitionTime, heartbeatTime)` on the NodeCondition and `(reason, message, source.Component, host)` on the Event — verified against NPD source in §7.3. None of provider/region/instanceId are present in either object. The controller MUST therefore read them off the **Node object** before creating the NHD:

| Field | Resolution rule | Source in the Node object |
|---|---|---|
| `provider` | Use Node label `cf.newrelic.com/cloud-provider` if present (set by argo-webhook). Otherwise parse the scheme of `Node.spec.providerID`: `aws://…` → `aws`, `azure://…` → `azure`. | label or `spec.providerID` |
| `region` | Read `topology.kubernetes.io/region` (set by the cloud-controller-manager on both EKS and the kubeadm cluster's azure-cloud-controller). Fall back to the deprecated `failure-domain.beta.kubernetes.io/region` label only if the GA label is missing. | `metadata.labels` |
| `instanceId` | AWS: parse `spec.providerID` of the form `aws:///<az>/<i-…>` and take the last path segment. Azure: parse `azure:///subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<vm>` and take the VM name. | `spec.providerID` |
| `clusterName` | Controller-level flag `--cluster-name` (set per Helm install). The controller has no reliable in-cluster way to learn its own cluster name. | flag, not the Node |

If any of `provider`, `region`, or `instanceId` cannot be resolved, the controller MUST NOT create the NHD. It MUST emit a Kubernetes Event on the Node with reason `MetadataResolutionFailed` and the specific missing field in the message.

The resolution functions MUST be unit-tested against fixture `Node` objects from real EKS and azure-cloud-controller-managed kubeadm clusters (capture during Day 1 AM smoke test).

### FR-3 — NHD creation
The controller MUST `Create` an NHD object with:
- Name: `<node-name-truncated-to-50-chars>-<unix-ts-seconds>`.
- Namespace: `container-fabric` (created by Helm if missing).
- `spec.case.caseId`: a fresh UUIDv4.
- `spec.case` populated per FR-2 + the trigger Condition payload.
- `spec.budgets`: `maxTurns=15`, `maxBudgetUSD="0.50"`, `deadline = observedAt + 60s` (RFC3339).
- `status.phase = "Pending"`.

The NHD `Create` MUST be idempotent: if a CR with the same name already exists, the controller MUST NOT recreate it and MUST treat the existing CR as the case.

### FR-4 — Agent invocation
After NHD `Create`, the controller MUST `POST /diagnose` against the agent service URL (`--agent-url`, default `http://nodemedic-agent.container-fabric.svc:8080/diagnose`) with:
- Bearer token from `Secret/nodemedic-agent-token` mounted as env.
- Request body matching `nodemedic-scope.md` §3.1 exactly.

On 202: set `status.phase=Diagnosing`, condition `AgentInvoked=True`.
On 429 or 5xx: exponential backoff 1s/2s/4s, max 3 retries, then mark `Failed` (per FR-7).
On 400/401: mark `Failed` immediately, no retry.

### FR-5 — Phase reconciliation
Reconciler MUST be keyed on NHD objects. Behavior per phase:

| Phase observed | Controller action |
|---|---|
| `""` (just created) | `POST /diagnose` (FR-4); set `Diagnosing` |
| `Diagnosing` | If `now > spec.budgets.deadline`: invoke FR-7 retry path. Else requeue at `min(deadline, now+10s)` |
| `Diagnosed` | Run confidence gate (FR-6); apply action; set `Acted` |
| `Acted` | Terminal — return without requeue |
| `Failed` | Terminal — return without requeue |

### FR-6 — Confidence gate
Auto-cordon iff **all** hold:
- `status.diagnosis.confidence >= 0.7`
- Distinct source count in `status.diagnosis.evidence[].source` is `>= 2` (sources are: `nrql`, `ssh`, `kubectl`, `cloud`, `proc`, `log`)
- `status.diagnosis.recommendation.action ∈ {Cordon, DrainAndCordon}`

Threshold values (`0.7`, `2`) MUST be controller flags (`--min-confidence`, `--min-evidence-sources`) so they can be tuned in the demo without rebuilding.

Gate pass:
1. Patch `Node.spec.unschedulable=true` (FR-8).
2. Set `status.action.decision=Applied`, `operation=cordon`, `appliedAt=<now>`.
3. Set `condition[ActionApplied]=True`.
4. Post Slack (FR-9) with `kind=Applied`.
5. Set `status.phase=Acted`.

Gate fail:
1. Do **not** cordon.
2. Set `status.action.decision=HumanInLoop`, `operation=noop`.
3. Post Slack (FR-9) with `kind=HumanInLoop` (severity=critical framing).
4. Set `status.phase=Acted`.

### FR-7 — Failure handling
When the controller marks an NHD `Failed`, it MUST:
- Set `status.phase=Failed`, condition `AgentInvoked=False`, with a specific `reason` (`DeadlineExceeded`, `AgentUnreachable`, `BadRequest`, `Unauthorized`).
- Retry exactly once for `DeadlineExceeded` and `AgentUnreachable` by re-issuing `POST /diagnose` with the same `caseId`. The agent is required to handle duplicate caseIds idempotently (FR contract on Scope 3 — flag in §10).
- After the retry also fails: terminal `Failed`, post Slack with `kind=Critical` (`severity=critical` framing, summary `NodeMedic agent failed for <node>`).

### FR-8 — Cordon executor
The controller MUST:
- Use its in-cluster ServiceAccount; never invoke `kubectl` out-of-band.
- Patch `Node.spec.unschedulable=true` via `client.Patch` with `client.MergeFrom`.
- Be idempotent: if `unschedulable` is already `true`, succeed without re-patching.
- Never drain. Never delete pods. Never modify any field besides `spec.unschedulable`.
- Refuse to act on a Node whose name does not match the NHD's `spec.case.nodeName`.

### FR-9 — Slack notifier
The controller MUST post to a Slack incoming webhook (URL from `Secret/nodemedic-slack`, mounted as env). One message per terminal outcome (Applied, HumanInLoop, Critical). Payload shape per `nodemedic-scope.md` §5.4 with these adjustments:

- Header text: `NodeMedic: <node> – <rcaCategory> (conf <X>)`.
- For `kind=HumanInLoop`: header text `NodeMedic: <node> – needs human review (conf <X>)`, color/severity styling distinct from Applied.
- For `kind=Critical`: header text `NodeMedic: <node> – AGENT FAILED`, severity styling distinct from Applied/HumanInLoop, includes the failure `reason`.
- Buttons: "View CR" (deep link to `kubectl get nhd <name> -o yaml` equivalent — for hackathon, an Argo CD or k8s-dashboard link if available, else the raw resource path) and "View audit log" (URL from `status.diagnosis.auditLogRef.objectStore` if set).

Retry: 3 attempts at 1s/2s/4s backoff. On final failure, log loudly and emit a Kubernetes Event on the NHD with `reason=NotifierFailed`. Do not roll back the cordon.

### FR-10 — Self-heal behavior
If a `NodeCondition` watched by the controller flips from `True` to `False` for a node that has an existing `Acted` NHD:
- The controller MUST NOT uncordon.
- The controller MUST NOT mutate the NHD.
- The controller MUST emit a Kubernetes Event on the Node with reason `ConditionCleared`, message including the NHD name. (Operator-visible, no automation.)

### FR-11 — Idempotency across restarts
The controller MUST tolerate a pod restart at any point in the phase machine:
- Restart during `Pending`/`Diagnosing` → reconciler picks up the existing NHD; if `phase=""` or `Diagnosing` and not yet past deadline, re-issues `POST /diagnose` with the original `caseId`.
- Restart during `Acted`/`Failed` → no-op (terminal phases).
- Restart between cordon and Slack post → cordon is idempotent (FR-8); Slack post is detected via `condition[ActionApplied]` having no corresponding Slack `ts` recorded — controller re-posts. (Acceptable demo-grade dedup; full Slack `ts`-based dedup is out of scope.)

---

## 6. Non-functional requirements

### NFR-1 — Latency budgets
| Step | Target |
|---|---|
| Condition `True` patch → NHD `Create` | ≤ 5 s |
| NHD `Create` → `POST /diagnose` returns 202 | ≤ 2 s |
| `phase=Diagnosed` observed → cordon executed | ≤ 5 s |
| `phase=Diagnosed` observed → Slack message visible | ≤ 10 s |

End-to-end (NPD signal → cordon) is dominated by the agent loop (~50 s budget), not the controller. Controller overhead must stay well under the §5.6 DoD of 35 s.

### NFR-2 — Availability
Single-replica deployment is acceptable for v1. The controller MUST set `RestartPolicy=Always` and survive transient apiserver hiccups (the controller-runtime cache handles this by default). Multi-replica leader election is a stretch goal.

### NFR-3 — Observability
The controller MUST expose Prometheus metrics on `:9443/metrics`:
- `nodemedic_cases_total{outcome="Applied|HumanInLoop|Failed"}`
- `nodemedic_phase_duration_seconds{phase="Pending|Diagnosing|Diagnosed|Acted",le=...}` (histogram)
- `nodemedic_agent_post_total{result="ok|429|5xx|timeout"}`
- `nodemedic_cordon_total{result="ok|err"}`
- `nodemedic_slack_post_total{kind="Applied|HumanInLoop|Critical",result="ok|err"}`

Every phase transition MUST emit a structured log line with `caseId`, `node`, `phase_from`, `phase_to`, `reason`. Log level `INFO` for happy path, `WARN` for retries, `ERROR` for terminal failures.

### NFR-4 — RBAC (least privilege)
Per `nodemedic-scope.md` §2.4. The controller's ClusterRole MUST be limited to:
- `nhd` resources: `get,list,watch,create,update,patch` (status subresource separately).
- `nodes`: `get,list,watch,patch` (only for `spec.unschedulable`).
- `events`: `create,patch`.

No write access to anything else. The Helm chart MUST fail to install if attempted with a broader role.

### NFR-5 — Cloud parity
The same controller binary, the same Helm values structure (with cluster-specific overrides for `--cluster-name`, kubeconfig context, Slack webhook), MUST work on both EKS and Azure kubeadm test clusters. Provider-specific code is forbidden in the controller — provider awareness is read-only and lives only in FR-2's resolution logic.

### NFR-6 — Configuration surface
All deploy-time parameters are flags or env vars on the Deployment. No hardcoded values for:
- `--watched-conditions` (default per FR-1)
- `--min-confidence` (default `0.7`)
- `--min-evidence-sources` (default `2`)
- `--agent-url`
- `--cluster-name`
- `--debounce-window` (default `30s`)

Secret material (`nodemedic-agent-token`, `nodemedic-slack`) is mounted from Kubernetes Secrets.

### NFR-7 — Auditability
The NHD CR's `status.conditions[]` history MUST be sufficient to reconstruct what the controller decided and when. Every `phase` transition MUST add or update a Condition entry with `lastTransitionTime`, `reason`, and a human-readable `message`.

---

## 7. The two cross-scope contracts (binding)

The constitution forbids any cross-scope channel besides these two. This spec consumes them; it does not redefine them.

### 7.1 NodeHealthDiagnosisAI CRD
Schema as defined in `nodemedic-scope.md` §2.2. The controller:
- **Writes:** `spec.*`, `status.phase`, `status.conditions[]`, `status.action.*`.
- **Reads:** `status.diagnosis.*` (written by Scope 3 agent).
- Never writes `status.diagnosis.*`. Never writes `status.evaluation.*` (out of scope).
- Uses server-side apply with field manager `nodemedic-controller` for spec; status subresource updates use the `Status().Update` path.

CRD definition is owned by Scope 3 per Constitution Article II.2; this spec consumes it. If schema changes are needed, both scopes coordinate on a single PR.

### 7.2 POST /diagnose
Request and response shape as defined in `nodemedic-scope.md` §3.1, §3.2, §3.3. The controller MUST send `caseId` matching the NHD's `spec.case.caseId`. Retry semantics are the controller's responsibility (FR-4); the agent treats duplicate `caseId` as idempotent (open question in §10).

### 7.3 What NPD actually provides (verified, June 2026 baseline)

This section is referenced by G2 and FR-2. It documents what fields the controller can rely on appearing in NPD's apiserver-side output, based on a direct read of the NPD source on this branch (`hackathon-2026/cf1z-baseline`, which is a fork of `kubernetes/node-problem-detector`).

**Internal Condition struct** — `pkg/types/types.go:55-68`:
```go
type Condition struct {
    Type       string          // e.g. "ConntrackSaturated"
    Status     ConditionStatus // True | False | Unknown
    Transition time.Time
    Reason     string
    Message    string
}
```

**Conversion to apiserver-side `v1.NodeCondition`** — `pkg/util/convert.go:29-37`. Output fields: `Type`, `Status`, `LastTransitionTime`, `Reason`, `Message`. Plus `LastHeartbeatTime` injected by the problem client at `pkg/exporters/k8sexporter/problemclient/problem_client.go:103-106`.

**Apiserver patch shape** — `pkg/exporters/k8sexporter/problemclient/problem_client.go:138-145`. The patch body is literally:
```json
{"status":{"conditions":[ ...v1.NodeCondition... ]}}
```
NPD never patches `metadata.labels`, never touches `spec.providerID`, never adds annotations.

**Event shape** — `pkg/exporters/k8sexporter/problemclient/problem_client.go:122-129, 147-154` and `getNodeRef` at L156-160. Events use:
- `EventSource{Component: <plugin source>, Host: <nodeName>}`
- `InvolvedObject` is a Node ref of just `(APIVersion: "v1", Kind: "Node", Name: <nodeName>)`.

No cloud metadata is attached.

**Cloud-aware code in NPD is opt-in and GCE-only.** `pkg/exporters/stackdriver/` populates instance metadata, but only when the operator passes `--exporter=stackdriver`, which we do not. The default k8s exporter (the one whose output the controller watches) is cloud-agnostic.

**Conclusion:** the controller MUST read the Node object itself to derive provider/region/instanceId. FR-2's resolution table is the binding rule.

---

## 8. Acceptance criteria (the gate for "v1 ships")

Each item is independently demonstrable on the demo cluster:

- [ ] **AC-1** Apply CRD + install Helm chart on `test-odd-wire`. `kubectl get crd nodehealthdiagnosisais.nodemedic.cf.newrelic.com` returns the CRD.
- [ ] **AC-2** Inject conntrack saturation. `kubectl get nhd -n container-fabric` shows a CR within 5 s with `spec.case.provider=aws`, `region=us-east-2`, `instanceId=i-…` resolved correctly.
- [ ] **AC-3** With a stub agent that writes `confidence=0.85, evidence=[nrql, ssh, kubectl], action=Cordon`, the controller cordons the node within 35 s and posts a Slack message. `kubectl get node <n> -o jsonpath='{.spec.unschedulable}'` is `true`.
- [ ] **AC-4** With a stub agent that writes `confidence=0.5, evidence=[nrql, ssh], action=Cordon`, the controller does NOT cordon; `status.action.decision=HumanInLoop`; Slack message has the "needs review" framing.
- [ ] **AC-5** With a stub agent that returns 202 then never updates the CR, the controller marks `Failed` after the deadline, retries once, and on second timeout posts a critical Slack message. Node is not cordoned.
- [ ] **AC-6** Inject the same fault twice within 30 s. Only one NHD is created.
- [ ] **AC-7** Self-heal: with the node cordoned, manually flip the condition back to `False`. The controller does not uncordon and emits a `ConditionCleared` Event on the Node.
- [ ] **AC-8** Repeat AC-1 through AC-3 on the Azure kubeadm test cluster. Same Helm chart, no code change. CR shows `provider=azure`, region/instanceId resolved from the Azure providerID.
- [ ] **AC-9** Restart the controller pod mid-`Diagnosing`. Reconciler resumes; case completes successfully.
- [ ] **AC-10** Prometheus `/metrics` endpoint exposes the metrics in NFR-3 with non-zero values after running the demo.

---

## 9. Demo flow (how this spec earns its keep)

1. Operator: `make inject-conntrack CLUSTER=test-odd-wire`.
2. Within 5 s: NHD CR appears. Slack does not yet post.
3. Within ~55 s: agent writes `phase=Diagnosed` with `confidence ≥ 0.7`.
4. Within 5 s of step 3: controller cordons the node and posts Slack.
5. `kubectl get node` shows `SchedulingDisabled`.
6. `kubectl get nhd <name> -o yaml` shows full case history: trigger, all phase transitions, action.decision, evidence count.
7. Repeat on Azure kubeadm cluster — same flow, same chart, only `--cluster-name` and the kubeconfig context differ.

For the gate-fail demo: hand-edit the agent stub to return `confidence=0.5`, re-run, show the HumanInLoop Slack message and the un-cordoned node.

---

## 10. Open questions (need closure before plan)

1. **Helm chart hosting.** Is there an existing Container Fabric Helm chart repo to publish into (e.g. Artifactory under `cf-helm`), or do we ship a tarball + `helm install --repo`? Affects deploy ergonomics, not behavior.
2. **CRD ownership in the chart.** Does Scope 2's chart install the CRD, or is the CRD shipped separately by Scope 3 (since Scope 3 owns the schema)? Recommend: chart installs CRD with a `keep` annotation; cross-scope coordination is a one-line README note.
3. **Idempotent `POST /diagnose` on the agent side.** This spec assumes the agent treats a repeat `caseId` as idempotent (returns 202 with `status="queued"` if the case is already queued, or echoes the existing case status). Needs confirmation from Scope 3 — flagged in their open questions §6.6 too.
4. **Slack "View CR" link target.** No standard k8s dashboard on test clusters. Options: (a) link to a static `kubectl get` instruction, (b) host a tiny static page that embeds the CR YAML, (c) drop the button. Recommend (a) for hackathon — copy-pasteable command in the message body.
5. **Multi-replica leader election.** Marked as stretch in NFR-2. If we don't add it, single-pod restart during reconcile is the only failure mode — covered by FR-11. Confirm we accept that.

---

## 11. Risks

| Risk | Mitigation |
|---|---|
| Agent returns 202 but never writes status (silent failure) | Deadline + retry + critical Slack (FR-7) |
| Watched-condition flap creates duplicate NHDs | 30 s debounce per (node, condition) (FR-1); idempotent `Create` (FR-3) |
| Controller cordons the wrong node (label confusion) | FR-8: refuse if Node.name ≠ NHD.spec.case.nodeName |
| Scope 3 ships a CRD schema change late and breaks the controller | Single source of truth in Scope 3's repo; Scope 2 vendors a generated client. Changes go through the contract amendment process |
| Slack webhook expires/fails mid-demo | 3-retry backoff (FR-9); failure is logged + Event'd but does not block cordon (cordon is the user-facing safety win) |
| Azure cluster's NPD reports `providerID` in an unexpected format | Resolution failure path in FR-2 emits a clear Event; rehearse on Azure cluster Day 1 AM |

---

## 12. Glossary

- **NPD** — node-problem-detector ([github.com/kubernetes/node-problem-detector](https://github.com/kubernetes/node-problem-detector)).
- **NHD** — `NodeHealthDiagnosisAI` CRD (this project).
- **Confidence gate** — the three-part check in FR-6 / Constitution Article I.3.
- **Watched condition** — a `NodeCondition.type` listed in `--watched-conditions`.
- **Cordon** — `Node.spec.unschedulable = true`. Does not affect running pods.
- **Test cluster** — a cluster whose name starts with `test-`. Per Constitution Article I.9, the only place this controller may run during the hackathon.
