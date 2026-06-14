<!-- SPECKIT START -->
This repo carries two NodeMedic specs side-by-side under `.specify/specs/`. Read both for full context.

**Constitution** (binding for both scopes): [`.specify/memory/constitution.md`](.specify/memory/constitution.md)

**Scope 2 — NodeMedic Controller** (Go, controller-runtime):
- Spec: [`.specify/specs/001-nodemedic-controller/spec.md`](.specify/specs/001-nodemedic-controller/spec.md)
- Plan: [`.specify/specs/001-nodemedic-controller/plan.md`](.specify/specs/001-nodemedic-controller/plan.md)
- Phase 0 research: [`.specify/specs/001-nodemedic-controller/research.md`](.specify/specs/001-nodemedic-controller/research.md)
- Phase 1 artifacts: [`data-model.md`](.specify/specs/001-nodemedic-controller/data-model.md), [`contracts/`](.specify/specs/001-nodemedic-controller/contracts/), [`quickstart.md`](.specify/specs/001-nodemedic-controller/quickstart.md)
- Code lives in this repo under `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/...`, `deployment/helm/nodemedic-controller/`. Work happens on branch `hackathon-2026/scope2-controller` cut from `hackathon-2026/cf1z-baseline`. NPD's existing tree is untouched.

**Scope 3 — NodeMedic Agent** (Python, Claude Agent SDK):
- Spec: [`.specify/specs/002-nodemedic-agent/spec.md`](.specify/specs/002-nodemedic-agent/spec.md)
- Per-cluster Secret manifests: [`.specify/specs/002-nodemedic-agent/manifests/`](.specify/specs/002-nodemedic-agent/manifests/)
- Plan: not yet generated (`/speckit-plan` is the next step in the spec-kit workflow once the spec is finalized).
- Code: not yet started — the spec is finalized but no Python implementation exists in this repo today.
<!-- SPECKIT END -->
