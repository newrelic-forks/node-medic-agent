<!-- SPECKIT START -->
For additional context about technologies to be used, project structure,
shell commands, and other important information, read the current plan at
[`.specify/specs/001-nodemedic-controller/plan.md`](.specify/specs/001-nodemedic-controller/plan.md).

The plan covers the **NodeMedic Controller** (Scope 2 of the AFA 2026 hackathon):
- Spec: [`.specify/specs/001-nodemedic-controller/spec.md`](.specify/specs/001-nodemedic-controller/spec.md)
- Constitution: [`.specify/memory/constitution.md`](.specify/memory/constitution.md)
- Phase 0 research: [`.specify/specs/001-nodemedic-controller/research.md`](.specify/specs/001-nodemedic-controller/research.md)
- Phase 1 artifacts: [`data-model.md`](.specify/specs/001-nodemedic-controller/data-model.md), [`contracts/`](.specify/specs/001-nodemedic-controller/contracts/), [`quickstart.md`](.specify/specs/001-nodemedic-controller/quickstart.md)

The controller code lives **in this repo** (decision 2026-06-12), namespaced
under `cmd/nodemedic-controller/`, `api/v1alpha1/`, `internal/nodemedic/...`,
`config/nodemedic/`, `deployment/helm/nodemedic-controller/`, and
`test/nodemedic/`. Work happens on branch `hackathon-2026/scope2-controller`
cut from `hackathon-2026/cf1z-baseline`. NPD's existing tree is untouched.
<!-- SPECKIT END -->
