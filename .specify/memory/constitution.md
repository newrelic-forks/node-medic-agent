# NodeMedic Constitution

**Project:** NodeMedic — AI Diagnostic Agent for Sick Kubernetes Nodes
**Context:** Container Fabric · AFA 2026 hackathon (2 days)
**Audience:** scope captains (Craig, Harrison, Shabeeb, Sachin) and the spec-kit agents that generate specs/plans for this repo.

This constitution is the small set of principles that **must hold for every spec, plan, and PR** in this repo during the hackathon. It does not redesign the system — that lives in [`docs/cf/nodemedic.md`](../../docs/cf/nodemedic.md) and [`docs/cf/nodemedic-scope.md`](../../docs/cf/nodemedic-scope.md). It tells you what we will not compromise on, even under hackathon pressure.

If a spec or plan conflicts with this document, the constitution wins. If you need to break a rule, say so explicitly in the plan with a one-line justification — don't slip it in.

This is a living doc — edits land in-place as the team agrees on changes. Git history is the change log; we don't version-stamp the file.

---

## Article I — Safety (non-negotiable)

The agent acts on production-shaped infrastructure. Auto-cordon is only defensible if these hold:

1. **Credential-layer least privilege is the primary safety boundary.** Every credential mounted to the agent pod (Kubernetes ServiceAccount RBAC, AWS IAM role, Azure RBAC role, NR MCP token, SSH key target ACL) MUST be scoped to read-only operations on the resources the agent legitimately needs. The agent cannot do what its credentials cannot do — that is the defense, not LLM-side argument validation.
2. **Cordon-only.** No drain. No eviction. No pod deletion. No node termination. The cordon executor is **not an LLM tool** — the controller invokes it after the confidence gate. The agent has no credential that grants `nodes/patch` (controller spec FR-14 binds this).
3. **Confidence gate is binding.** Auto-cordon fires only if all three hold:
   - `status.diagnosis.confidence >= 0.7`
   - `>= 2` distinct evidence sources cited (sources: `nrql`, `ssh`, `kubectl`, `cloud`, `proc`, `log`)
   - `recommendation.action ∈ {Cordon, DrainAndCordon}`
   Below threshold → `HumanInLoop`, page on-call, do not cordon.
4. **Every tool call is observable.** The agent's hook surface (`PreToolUse` for SDK-native tools, equivalent shim for `Bash`-shelled CLIs) emits a structured-log line per tool call carrying at minimum `{caseId, ts, tool, command-or-args}`. For the hackathon, the transport is stdout structured logs — durable for the lifetime of the pod / log-rotation window. Production will need durable JSONL on a PVC; that's a known follow-up (see "Production hardening" below).
5. **Non-production clusters only.** Fault injection, demos, and any agent deployment run on **non-production** clusters: `test-*` clusters (EKS), designated kubeadm test clusters on Azure, and dev clusters under the legacy naming scheme (`cf1z`, `jc1z`, `sk1z`). **Never** on `stg-*`, `us-*`, or `eu-*` — those serve customer traffic and are off-limits for the hackathon.

**Rule of thumb:** if a change reduces the safety surface, it needs sign-off from at least one captain outside the scope making the change.

### Hackathon-scope simplifications (vs. the production posture)

For the hackathon we deliberately defer several controls that the production rollout will need to put back. These choices are acceptable **only because** the credential layer (Article I.1), non-production cluster surface (Article I.5), and tool-call observability (Article I.4) keep blast radius bounded.

- *No structured tool allow-list.* The agent calls `Bash` directly (kubectl, aws, az, ssh, dmesg, journalctl, /proc reads). No SDK `can_use_tool` callback.
- *No SSH command prefix-match.* `ssh` invocations are free-form against the cluster's worker nodes via the hackathon SSH key.
- *`permission_mode = "bypassPermissions"`.* The autonomous loop is not interrupted by per-tool prompts.
- *No agent-side termination ceiling.* The agent loop runs until `emit_report` is called or the model halts. No `max_turns`, no `max_budget_usd`, no wall-clock `deadline`. The controller's own ~60 s deadline (Spec 001 FR-5) bounds *user-visible* case duration but does NOT cancel the agent's loop. A long-running agent can produce a useful diagnosis after the controller has given up; Spec 002 FR-12 governs the resulting status-write race. Global cost is bounded by the hackathon-only Anthropic API key's account-level budget cap.
- *Ephemeral audit log.* The `PreToolUse` hook still records every tool call (Article I.4), but to stdout structured logs rather than a durable PVC-backed JSONL. Operators retrieve them during the demo via `kubectl logs <agent-pod> --since=… | jq 'select(.caseId=="…")'`. After pod restart or log rotation, the trace is gone. The CR's `status.diagnosis.evidence[]` remains durable (apiserver-backed) — that's the agent's permanent conclusion record, separate from the per-tool-call trace.

### Production hardening (deferred)

Before any deployment outside non-production clusters (i.e. before `stg-*`/`us-*`/`eu-*`), the simplifications above MUST be reverted:

1. Structured tool allow-list per `docs/cf/nodemedic-scope.md` §6.3 — explicit `kubectl_get`, `kubectl_describe`, `ssh_run`, `read_proc_file`, `cloud_info.*` tools with schema-validated args. SDK `can_use_tool` callback enforced.
2. SSH command prefix-match against the §6.3 list.
3. `permission_mode = default` plus the `can_use_tool` enforcement.
4. Unit-test surface for rejection of the canonical dangerous calls (`rm -rf /`, `kubectl delete node`, `/etc/shadow` reads, etc.).
5. Hard caps on `max_turns` and `max_budget_usd` with per-case cost accounting.
6. Agent-side wall-clock deadline with cancellation semantics that drain the in-flight tool call cleanly and write a partial CR.
7. Durable per-case audit JSONL on a PVC, with `auditLogRef.objectStore` populated on the CR — non-negotiable for incident review and compliance auditing.

This list is the contract for "what production hardening looks like." It is not a roadmap commitment for who does what when; that's a separate planning conversation.

---

## Article II — Scope boundaries (don't dissolve the seams)

The system has three scopes (Scope 1: cluster + NPD + faults; Scope 2: controller; Scope 3: agent). Hackathon pressure tends to dissolve interfaces into shared mutable state. We don't do that here.

1. **There are exactly two cross-scope contracts:**
   - The `NodeHealthDiagnosisAI` CRD (`nodemedic.cf.newrelic.com/v1alpha1`) — schema in `nodemedic-scope.md` §2.2.
   - The `POST /diagnose` HTTP contract — shape in `nodemedic-scope.md` §3.1.
   No other channel is permitted. No shared filesystem, no shared database, no "just call this internal endpoint."
2. **The CRD schema is owned by Scope 3** (agent writes it) and **read by Scope 2** (controller reconciles it). Schema changes require updating both sides in the same PR.
3. **Each scope MUST ship a stub** for the next scope on Day 1 AM, per `nodemedic-scope.md` §7. No scope is allowed to block another by holding the seam.
4. **The agent does not call the controller.** The agent writes the CR; the controller watches. One direction.
5. **The controller does not call MCP tools.** Tool surface belongs to the agent process.
6. **Cluster topology is passed in, not discovered.** The controller resolves `provider`, `region`, `instanceId` from Node labels + `spec.providerID` and writes them into `spec.case`. The agent does not infer the cloud — it dispatches on `case.provider`.
7. **One agent binary, two clouds.** The same agent process serves AWS and Azure cases. Cloud-specific code lives behind the Cloud-Info MCP backends; everything above that line is provider-agnostic.

**Rule of thumb:** if you find yourself adding a code path that lets one scope reach into another, stop and add it to the contract instead — or convince the other captain you don't need it.

---

## Article III — How we make decisions during the hackathon

These are the lighter rules — they govern judgment calls, not safety.

1. **Cite, don't claim.** Every assertion in a spec, plan, or CR diagnosis is backed by a verifiable reference: a file:line, a NRQL query, a command output, a primary doc URL. "It probably works" is not a citation.
2. **Stub before integrate.** The Day 1 AM stubs in `nodemedic-scope.md` §7 are mandatory. End-to-end integration happens once, on Day 1 PM, against real components — not by accreting partial integrations.
3. **Out of scope stays out of scope.** Per `nodemedic-scope.md` §9: no eval agent, no drain, no multi-region, no predictive scoring, no human-approval UI, no production rollout. If a feature feels essential mid-hackathon, write a follow-up issue — don't merge it.
4. **Demo flow drives priorities.** When in doubt, optimize for the demo path in `nodemedic.md` §4 (conntrack on EKS, then on Azure kubeadm). Stretch fault classes only after the primary flow is rehearsed end-to-end.
5. **Ask before pivoting.** If an approach isn't working, surface the blocker to the team rather than silently re-architecting. The seams in Article II are exactly the points where silent re-architecting hurts most.

---

## Changing this document

Hackathon scope: edit it. PR + one captain approval (from a captain outside the scope proposing the change) is enough. No version bumps, no amendments log — `git log` is the source of truth for chronology. Inline overrides in specs/plans are not amendments — they're exceptions, and they need the one-line justification called out at the top of this doc.

---

## Pointers

- Design doc: [`docs/cf/nodemedic.md`](../../docs/cf/nodemedic.md)
- Scope decomposition + shared contracts: [`docs/cf/nodemedic-scope.md`](../../docs/cf/nodemedic-scope.md)
- Architecture diagrams: [`docs/cf/diagrams/`](../../docs/cf/diagrams/)
