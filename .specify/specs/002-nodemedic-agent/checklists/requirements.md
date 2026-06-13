# Specification Quality Checklist: NodeMedic Agent

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-06-12
**Updated:** 2026-06-12 — for spec v0.2 alignment to constitution v2.0
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs) *(see notes)*
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders *(see notes)*
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic *(see notes)*
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification *(see notes)*

## Notes

### Spec convention deviates from base template (intentional)

This spec matches the structural shape of `001-nodemedic-controller/spec.md` rather than the bare `spec-template.md` skeleton. That convention is established in this repo (constitution explicitly anchors specs to `nodemedic-scope.md` and the diagrams) and was confirmed with the user before authoring v0.1.

### "No implementation details" — what's named is contract, not implementation

The agent spec deliberately names specific tools (`Bash`, NR HTTP MCP, `kubectl`, `aws`, `az`, `ssh`, `emit_report`), specific model IDs (`claude-opus-4-7`, `claude-sonnet-4-6`), and specific SDK constructs (`ClaudeSDKClient`, `permission_mode`, `PreToolUse` hook). Treating these as "implementation" would dissolve the safety surface the constitution depends on:

- The tool surface IS the contract — Constitution v2.0 Article I.4 (audit) and the FR-13 credential-layer verification both reference what tools exist.
- The model IDs are bound by Constitution Article III.1 (cite, don't claim) — picking a different model is a spec change, not an implementation choice.
- `permission_mode = "bypassPermissions"` is a relaxation explicitly enumerated in the constitution amendments log; substituting `default` would silently re-tighten the agent and trigger per-tool prompts that break the autonomous loop.

### v0.2 alignment to constitution v2.0

This spec was rewritten on 2026-06-12 to reflect the constitution v2.0 amendment, which relaxed four v1.0 requirements for the hackathon scope:
- Removed the structured tool allow-list / `can_use_tool` two-layer enforcement (was G4 + FR-5 + FR-13 unit tests).
- Removed the SSH command prefix-match (was Article I.5).
- Removed `permission_mode = default` requirement (was Article I.6).
- Removed per-case `max_turns` and `max_budget_usd` enforcement (was G8 + FR-7).

Credential-layer least privilege (FR-14 kube RBAC + IRSA / Azure RBAC / NR token scope) is now the primary safety boundary, with FR-13 verifying it on every install. The wall-clock `deadline` (FR-7) is the only hard loop ceiling.

All four relaxations are tracked as P0 follow-ups in the constitution amendments log and MUST be reverted before production rollout.

### v0.2 dropped latency hard targets

Per user direction (2026-06-12), the agent's `POST /diagnose` is async — the controller doesn't block on diagnosis duration, the CR informer surfaces results whenever they land. NFR-1 in v0.2 reframes prior latency contracts as informational soft targets. Diagnosis time is bounded only by the per-case `deadline`. AC-3 was reworded from "within 60 s" to "before deadline elapses" with the demo-shape note retained for context.

### v0.2 deployment topology clarification

Per user direction (2026-06-12), G11 was rewritten: agent runs as an **independent Deployment + Service** in the same cluster as the controller — not as a sidecar in the controller's pod, not on `interlinked`. Independent RBAC, lifecycle, restart, and rollout. The controller reaches the agent over the in-cluster Service. v0.1 used "sidecar" loosely; v0.2 fixes the terminology.

### Open questions in §10 are plan-phase, not scope-phase

§10 captures 8 plan-phase concerns (Anthropic key sourcing, NR MCP rate limits, Azure auth mechanism, SSH keypair owner, Slack link target, model-fallback cache asymmetry, PVC sizing, CLI versioning). None block the spec from being ready for `/speckit-plan`.
