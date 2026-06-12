# NodeMedic Constitution

**Project:** NodeMedic — AI Diagnostic Agent for Sick Kubernetes Nodes
**Context:** Container Fabric · AFA 2026 hackathon (2 days)
**Status:** v1.0 · 2026-06-12
**Audience:** scope captains (Craig, Harrison, Shabeeb, Sachin) and the spec-kit agents that generate specs/plans for this repo.

This constitution is the small set of principles that **must hold for every spec, plan, and PR** in this repo during the hackathon. It does not redesign the system — that lives in [`docs/cf/nodemedic.md`](../../docs/cf/nodemedic.md) and [`docs/cf/nodemedic-scope.md`](../../docs/cf/nodemedic-scope.md). It tells you what we will not compromise on, even under hackathon pressure.

If a spec or plan conflicts with this document, the constitution wins. If you need to break a rule, say so explicitly in the plan with a one-line justification — don't slip it in.

---

## Article I — Safety (non-negotiable)

The agent acts on production-shaped infrastructure. Auto-cordon is only defensible if these hold:

1. **Read-only by default.** Every tool the agent can call is read-only. The single exception is the cordon executor, which is **not an LLM tool** — the controller invokes it after the confidence gate.
2. **Cordon-only.** No drain. No eviction. No pod deletion. No node termination. Only `kubectl cordon` (`Node.spec.unschedulable=true`).
3. **Confidence gate is binding.** Auto-cordon fires only if all three hold:
   - `status.diagnosis.confidence >= 0.7`
   - `>= 2` distinct evidence sources cited (sources: `nrql`, `ssh`, `kubectl`, `cloud`, `proc`, `log`)
   - `recommendation.action ∈ {Cordon, DrainAndCordon}`
   Below threshold → `HumanInLoop`, page on-call, do not cordon.
4. **Tool allow-list.** The agent's `can_use_tool` callback rejects anything not in the allow-list defined in `docs/cf/nodemedic-scope.md` §6.3. New tools require an explicit spec change — not an inline addition.
5. **SSH commands are prefix-matched.** `ssh_run` accepts only the command prefixes in §6.3. Free-form shell is forbidden, including via the LLM.
6. **Permission mode = `default`.** Never `bypassPermissions`. Not for "the demo," not for "just this run."
7. **Every tool call is audited.** `PreToolUse` hook writes `{caseId, ts, tool, argsHash, allowed, reason}` to the audit JSONL. Audit log is part of the deliverable, not an optional extra.
8. **Hard caps on the loop.** `max_turns` and `max_budget_usd` are enforced; exceeding either ends the case as `Failed` with a reason. No "just one more turn" override.
9. **Test clusters only.** Fault injection and demos run on `test-*` clusters (EKS) or designated kubeadm test clusters on Azure. Never `stg-*`, `us-*`, or `eu-*`.

**Rule of thumb:** if a change reduces the safety surface, it needs sign-off from at least one captain outside the scope making the change.

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

## Amendment process

This is a hackathon. Amendments are cheap:

1. Open a PR against this file.
2. Tag the four captains.
3. Merge with one approval from a captain outside the scope proposing the change.
4. Bump the version (`v1.0` → `v1.1` for clarifications, `v2.0` for any change to Article I).

Inline overrides in specs/plans are not amendments — they're exceptions, and they need the one-line justification called out at the top of this doc.

---

## Pointers

- Design doc: [`docs/cf/nodemedic.md`](../../docs/cf/nodemedic.md)
- Scope decomposition + shared contracts: [`docs/cf/nodemedic-scope.md`](../../docs/cf/nodemedic-scope.md)
- Architecture diagrams: [`docs/cf/diagrams/`](../../docs/cf/diagrams/)
