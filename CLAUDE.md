<!-- SPECKIT START -->
This repo carries three NodeMedic specs side-by-side under `.specify/specs/`. Read all three for full context.

**Constitution** (binding for all scopes): [`.specify/memory/constitution.md`](.specify/memory/constitution.md)

**Scope 2 — NodeMedic Controller** (Go, controller-runtime):
- Spec: [`.specify/specs/001-nodemedic-controller/spec.md`](.specify/specs/001-nodemedic-controller/spec.md)
- Plan: [`.specify/specs/001-nodemedic-controller/plan.md`](.specify/specs/001-nodemedic-controller/plan.md)
- Phase 0 research: [`.specify/specs/001-nodemedic-controller/research.md`](.specify/specs/001-nodemedic-controller/research.md)
- Phase 1 artifacts: [`data-model.md`](.specify/specs/001-nodemedic-controller/data-model.md), [`contracts/`](.specify/specs/001-nodemedic-controller/contracts/), [`quickstart.md`](.specify/specs/001-nodemedic-controller/quickstart.md)
- Code lives in this repo under `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/...`, `deployment/helm/nodemedic-controller/`. Work happens on branch `hackathon-2026/scope2-controller` cut from `hackathon-2026/cf1z-baseline`. NPD's existing tree is untouched.

**Scope 3 — NodeMedic Agent** (Python, Claude Agent SDK):
- Spec: [`.specify/specs/002-nodemedic-agent/spec.md`](.specify/specs/002-nodemedic-agent/spec.md)
- Plan: [`.specify/specs/002-nodemedic-agent/plan.md`](.specify/specs/002-nodemedic-agent/plan.md)
- Phase 0 research: [`.specify/specs/002-nodemedic-agent/research.md`](.specify/specs/002-nodemedic-agent/research.md)
- Phase 1 artifacts: [`data-model.md`](.specify/specs/002-nodemedic-agent/data-model.md), [`contracts/`](.specify/specs/002-nodemedic-agent/contracts/), [`quickstart.md`](.specify/specs/002-nodemedic-agent/quickstart.md)
- Per-cluster Secret manifests: [`.specify/specs/002-nodemedic-agent/manifests/`](.specify/specs/002-nodemedic-agent/manifests/)
- Code: not yet started — the spec, plan, and contracts are finalized but no Python implementation exists in this repo today. `/speckit-tasks` is the next step in the spec-kit workflow.

**Scope 4 — NodeMedic On-Call UI + Slack Format Upgrade** (Go, stdlib net/http + html/template):
- Spec: [`.specify/specs/003-nodemedic-oncall-ui/spec.md`](.specify/specs/003-nodemedic-oncall-ui/spec.md)
- Plan: [`.specify/specs/003-nodemedic-oncall-ui/plan.md`](.specify/specs/003-nodemedic-oncall-ui/plan.md)
- Phase 0 research: [`.specify/specs/003-nodemedic-oncall-ui/research.md`](.specify/specs/003-nodemedic-oncall-ui/research.md)
- Phase 1 artifacts: [`data-model.md`](.specify/specs/003-nodemedic-oncall-ui/data-model.md), [`contracts/`](.specify/specs/003-nodemedic-oncall-ui/contracts/), [`quickstart.md`](.specify/specs/003-nodemedic-oncall-ui/quickstart.md)
- Code: not yet started. Will land under `cmd/nodemedic-oncall-ui/`, `internal/oncall/...`, `deployment/helm/nodemedic-oncall-ui/`, plus extensions to `internal/nodemedic/notifier/` for the Block Kit Slack builders. No new CRD, no new cross-scope HTTP contract — reads existing NHD CRD, writes one new annotation key on the same CR. Demo finale for the AFA 2026 hackathon.
<!-- SPECKIT END -->
