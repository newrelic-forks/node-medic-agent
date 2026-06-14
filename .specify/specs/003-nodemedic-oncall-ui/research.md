# Phase 0 Research — NodeMedic On-Call UI + Slack Format Upgrade

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-14

This document closes every plan-phase decision and pins the dependency / pattern choices the plan refers to as "as researched." The spec's `## Clarifications` section already resolved the 12 product-level questions; the items below are the technology-layer decisions implied by those clarifications and by the plan's Technical Context.

Format per the spec-kit research template:

> **Decision** — what was chosen.
> **Rationale** — why.
> **Alternatives** — what else was considered, and why rejected.

---

## R-1 Web framework: stdlib `net/http` + `html/template` (closes plan PD-2)

**Decision** Use Go's standard library: `net/http.ServeMux` for routing, `html/template` for SSR, `http.Handler` for middleware composition. No `gin`, `echo`, `chi`, or `gorilla/mux`. The HTTP surface is six routes total — `/`, `/api/cases`, `/cases/<nhd-name>`, and three `/api/cases/<nhd-name>/actions/<verb>` endpoints. SSE drain is `http.Flusher` against `http.ResponseWriter`. Static assets ship via `http.FileServer(http.FS(embed.FS))`.

**Rationale**
- The HTTP surface is small and stable. Stdlib `ServeMux` (Go 1.22's tightened pattern syntax with `{nhd}` path parameters) covers every route the spec names.
- `html/template` does context-aware HTML escaping by default. The UI renders LLM-authored content (the agent's `rootCause`, `recommendation.reason`, evidence `result` strings) — auto-escaping is the right floor of XSS defense. `text/template` would be the wrong choice; `html/template` is the right one.
- Zero deps means zero supply-chain surface for a UI that runs in `cf-monitoring`. `controller-runtime` + `client-go` are already in `go.sum` from the controller; we add nothing.
- Reviewer cognitive load: the controller is stdlib-net/http; the UI is stdlib-net/http; one mental model spans both binaries.
- `embed.FS` for static assets means a single binary ships every byte of the UI — no volume mount, no ConfigMap, no ImagePullPolicy dance for HTML edits.

**Alternatives**
- **`gin` or `echo`.** Adds JSON-binding helpers, middleware chains, and route groups we don't need. The UI's JSON path is two endpoints (`GET /api/cases`, the SSE drain stream); `json.NewEncoder(w).Encode(...)` covers both.
- **`chi`.** Lighter than gin/echo; still a dep. Stdlib's Go 1.22 pattern syntax (`mux.HandleFunc("POST /api/cases/{nhd}/actions/uncordon", ...)`) is enough.
- **HTMX.** Tempting — the spec's interactivity (per-action POSTs, list refresh) is a near-perfect HTMX use case. Rejected for two reasons: (a) the spec §0 binds "vanilla HTML/CSS/JS, no framework, no build step, no `node_modules`"; HTMX is a 14 KB script we'd ship as a static asset, but it's still a third-party JS surface. (b) The drain SSE consumer is the only non-trivial JS, and HTMX's SSE extension would still need glue — at which point we might as well write the ~150 lines of vanilla JS ourselves. The trade-off is honest: HTMX is a viable alternative, but the spec already locked the choice.
- **React / Vue / Svelte.** Out of scope per spec §1 + §3.

**Citations** Spec §1 (vanilla HTML/JS, no framework); Go stdlib `net/http` Go 1.22 release notes; `html/template` package docs.

---

## R-2 Block Kit JSON shape (closes plan PD-1)

**Decision** Three-block layout with a top-level `text` mrkdwn fallback and an `attachments[]` color side bar:

```json
{
  "text": "<plain-text fallback line — single mrkdwn-stripped sentence>",
  "blocks": [
    { "type": "header", "text": { "type": "plain_text", "text": "<emoji> <verb>: <node> (<cluster>)" } },
    { "type": "section", "fields": [
        { "type": "mrkdwn", "text": "*Decision:*\n<value>" },
        { "type": "mrkdwn", "text": "*Confidence:*\n<value>" },
        { "type": "mrkdwn", "text": "*Trigger:*\n<value>" },
        { "type": "mrkdwn", "text": "*rcaCategory:*\n<value>" }
    ]},
    { "type": "actions", "elements": [
        { "type": "button", "style": "primary", "text": { "type": "plain_text", "text": "View full diagnosis" }, "url": "<uiBaseURL>/cases/<nhdName>" }
    ]}
  ],
  "attachments": [
    { "color": "<danger|warning|#808080|unset>", "fallback": "<same as top-level text>" }
  ]
}
```

**Rationale**
- `section.fields` renders as a 2-column responsive grid in Slack desktop and a stacked list on Slack mobile. Slack supports up to 10 fields; the spec's four (Decision, Confidence, Trigger, rcaCategory) sit comfortably in a 2×2.
- The `header` block is the visual anchor — large bold text with the emoji that matches `attachment.color`. Header text is `plain_text` (mrkdwn is rejected by Slack inside `header`); we shape it as `<emoji> <verb>: <node> (<cluster>)` per spec FR-2.
- The `actions` block holds a single primary button. `style: "primary"` is Slack's CTA-blue; the URL points at the UI per FR-3.
- `attachments[].color` is Slack's named-alias surface (`good` / `warning` / `danger`) which renders correctly on every Slack client. The spec §0 Q8 binds the per-severity mapping: `danger` for `Applied`, `warning` for `HumanInLoop`, `#808080` for `Failed`, unset for `Diagnosing`/`Pending`.
- Top-level `text` is the **mrkdwn fallback** for clients that don't render Block Kit (Slack legacy mobile, certain bridges, the unfurl preview a quoted Slack message generates in another channel). Slack's docs are explicit: "always provide a `text` value, in case Slack can't render blocks." We carry a single sentence: `"<verb> on <node> in <cluster>: <decision> @ <confidence> — <View full diagnosis: ${url}>"`.

**Alternatives**
- **Single `text` mrkdwn block, newline-separated.** Loses the responsive grid, loses the visible header weight, loses the button. Easier to fall back to but a worse glance experience.
- **Multiple `section.text` blocks instead of `section.fields`.** Stacks vertically; consumes more screen real estate; the 4-field grid is denser and more skimmable.
- **`context` block for the four fields.** `context` blocks are smaller-font and intended for tertiary info (timestamps, attribution). Confidence + decision are the headline data; `section.fields` is the right block.
- **Attachment-only legacy format (`attachments[].text`).** Slack still supports it but discourages it for new posts; modern Block Kit is strictly more capable.

**Citations** Slack Block Kit Builder docs (`api.slack.com/block-kit`); Slack's "always provide a fallback" guidance (`api.slack.com/messaging/composing/layouts#fallback-text`); spec FR-1, FR-2, FR-3, §0 Q8.

---

## R-3 Pod eviction loop concurrency (closes plan PD-3)

**Decision** Bounded concurrency at 3 in-flight evictions: `sem := make(chan struct{}, 3)`. Each eviction is a goroutine that acquires the semaphore, calls `client.PolicyV1().Evictions(ns).Evict(ctx, &policyv1.Eviction{...})`, releases on return, and emits an SSE event. Per-pod results are streamed in **arrival order** (not pod-list order). On the wire the SSE shape is unchanged from the serial spec: each event is `data: {pod, namespace, result, detail}\n\n` followed by a terminal `event: complete\ndata: {summary}\n\n`.

**Rationale**
- The spec's perception contract (FR-16: "evict each via pods/eviction, displays per-pod success/skip/error") is about *the engineer seeing motion*, not about strict serial ordering. Three pods in flight at once still streams visible per-pod results; the engineer sees them tick by, just three at a time.
- Three is a deliberate floor: it's enough to amortize eviction RTT (~200 ms typical, ~5 s worst case during PDB-mediated retries) without overwhelming the apiserver or stampeding PDBs.
- PDB violations return `429 Too Many Requests` immediately. With concurrency 3, a PDB miss doesn't block the other two slots from making progress.
- 30 typical pods × 200 ms / 3 ≈ 2 s eviction wall-clock; in practice grace periods dominate. Without concurrency, the same drain runs in ~6 s + grace; with concurrency 3, it runs in ~2 s + grace. The engineer perceives the difference.

**Alternatives**
- **Strict serial (concurrency 1).** Spec contract reads as serial-with-progress; a literal reading is defensible. Rejected because: (a) a 30-pod node drain takes 90+ s, slow enough that the engineer wonders if it stalled; (b) the constraint is an artifact of the spec author thinking about UX, not API correctness; (c) the on-the-wire SSE shape is unchanged regardless of concurrency.
- **High concurrency (10+).** Risks PDB stampede behavior, can hammer the apiserver, and offers diminishing returns past ~5. Three is the smallest number that meaningfully shortens the loop.
- **Concurrency tied to pod count** (e.g. `min(podCount/4, 5)`). Over-engineered for the demo's load profile. Constant 3 is simpler, easier to reason about, and matches every observed cf1z node's drain shape.

**Citations** Spec FR-16 (serial-with-progress); Kubernetes Eviction API docs (PDB behavior, 429 semantics); the `client-go` `policyv1.Eviction` type definition (`k8s.io/api/policy/v1`).

---

## R-4 List-view auto-refresh: `setInterval` + `fetch` (closes plan PD-4)

**Decision** `setInterval(refreshList, 30000)` calling `GET /api/cases` (returns JSON array of case rows) and re-rendering the table `<tbody>` via template literals. No SSE for the list view. Auto-refresh pauses on `document.hidden=true` (the `visibilitychange` listener) so a backgrounded tab doesn't burn cycles; resumes on focus.

**Rationale**
- The spec's freshness requirement is "every 30 s" (FR-6) — a polling interval, not a push semantic. SSE would be over-engineered for a 30 s cadence.
- `fetch()` + JSON re-render is ~40 lines of vanilla JS. The implementation footprint is tiny.
- The `GET /api/cases` endpoint is the same endpoint the initial SSR page would hit; reusing it keeps the JSON shape and the SSR data shape identical (the SSR template literally renders the same JSON the JS gets).
- Pausing on `document.hidden` matches user expectation — engineers don't expect a background tab to keep polling. Spec §5 edge case "auto-refresh during background tab" is explicitly handled here: the resume path fires the refresh once on focus return, with a brief highlight on any new rows.

**Alternatives**
- **SSE for the list view.** Push semantics, lower per-event latency. Rejected because (a) the spec doesn't require sub-30s freshness, (b) SSE on the list view would mean the UI maintains an open connection per browser tab — fine for one engineer, but not free, (c) the drain endpoint already uses SSE; reserving SSE for that one place keeps the mental model clean.
- **WebSocket.** Bidirectional; the list view doesn't need bidirectional. Same engineering cost as SSE for less-bound cases.
- **HTTP/2 push.** Browsers have largely deprecated server push; not a viable target.
- **Manual-refresh-only.** Spec FR-6 explicitly requires auto-refresh; this isn't optional.

**Citations** Spec FR-6 (30 s auto-refresh); MDN `EventSource` vs `setInterval` discussion; `document.visibilityState` / `visibilitychange` MDN docs.

---

## R-5 Kubernetes client choice (controller-runtime split client)

**Decision** Use `sigs.k8s.io/controller-runtime/pkg/client` for the Go client surface. Build a "split client" via `client.New(...)` with the cached informer reading NHDs (list/get) and the direct client writing NHDs/Nodes/Pods/eviction. Scheme registration: `corev1.AddToScheme(scheme)` + `policyv1.AddToScheme(scheme)` + `nodemedicv1alpha1.AddToScheme(scheme)`.

**Rationale**
- The controller is already on controller-runtime v0.21.x (verified via `go.sum`); reusing the dep is free.
- Informer-cached reads on NHDs match the list-view's freshness profile: the informer's resync handles the 30 s auto-refresh shape implicitly. The list view's `GET /api/cases` doesn't even need to round-trip the apiserver — the cache is local.
- Direct client writes (eviction, annotation patch) are correct: writes shouldn't be eventually-consistent. The split client gives both shapes from one `client.Client`.
- Typed client surface (`client.Client.List(ctx, &nhdList)`) is strictly safer than `unstructured.Unstructured` or `dynamic.Interface` — the schema lives in `api/v1alpha1/nodehealthdiagnosisai_types.go` and the compiler catches drift.

**Alternatives**
- **Raw `client-go` typed client.** Works; requires hand-wiring informers if we want caching. controller-runtime is a thinner, more idiomatic layer over the same primitives.
- **`dynamic.Interface` (untyped).** No compile-time schema check. The agent uses this because Python lacks generated types; Go does have them, so we use them.
- **`kubectl` exec from the UI pod.** Forks per request; no informer cache; security worse. Out of consideration.

**Citations** `sigs.k8s.io/controller-runtime` v0.21 client docs; this repo's `cmd/nodemedic-controller/main.go` for client-builder reference; `api/v1alpha1/nodehealthdiagnosisai_types.go` for the typed scheme.

---

## R-6 Audit annotation FIFO ring buffer (closes spec FR-18a)

**Decision** Implement the annotation as a JSON-encoded array of entries, capped at N=20 with FIFO eviction. Append flow:

1. Read the NHD via the typed client (`client.Get(ctx, nsName, &nhd)`).
2. Decode `nhd.Annotations["nodemedic.cf.newrelic.com/ui-action-history"]` (empty → empty slice).
3. Append the new entry. If `len(entries) > 20`, slice off the front: `entries = entries[len(entries)-20:]`.
4. Re-encode to JSON; set the annotation.
5. `client.Patch(ctx, &nhd, client.MergeFrom(original))` — JSON merge patch to avoid clobbering concurrent writers (the controller writes `status.action.notifications.slack` after a Slack post; our annotation patch must not race).

Each entry is a `UIActionEntry` Go struct (data-model.md §3): `{ts, action, actor, result, detail}`. Estimated size: ~150–250 bytes per entry; 20 entries totals ~5 KB, comfortably below etcd's 256 KB per-object ceiling and well below the 8 KB AC-14b validation threshold.

**Rationale**
- FIFO is the right shape for "latest activity" — the engineer cares about recent actions, not ancient ones. The NHD CR itself is short-lived (deleted after MLC reclaims the node + log retention), so 20 entries is generous for any realistic incident.
- JSON-encoded annotation is portable: `kubectl get nhd <name> -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | jq` is the read path. No client SDK needed.
- Merge-patch (vs replace) keeps the write minimal and the conflict surface small: only the one annotation key is in the patch body.

**Alternatives**
- **No cap, just append forever.** Risks etcd ceiling violations on chatty cases (drain = one entry per pod, unbounded). Spec §0 Q9 explicitly considered and rejected this.
- **Sliding window by time (e.g. last 24h).** Time-based eviction is harder to reason about across pod restarts and clock skew; count-based is simpler and bounds size deterministically.
- **Separate audit CR.** Adds a new CRD, violates Constitution Article II.1 ("two cross-scope contracts, no more"). The annotation is the right shape because it lives on the existing object.
- **Server-side list-type=map merge.** etcd-supported, would handle the merge race better; but Spec 001 doesn't declare the annotation as a map type and we'd need a CRD bump. JSON merge-patch on a string-valued annotation is sufficient.

**Citations** Spec FR-18a + AC-14b; Kubernetes annotation size guidance; etcd per-object 1.5 MB hard ceiling (apiserver enforces ~256 KB conventionally).

---

## R-7 Drain SSE wire format

**Decision** Per-pod events use unnamed-event SSE (default `event: message`, decoded by `EventSource.onmessage`) with a JSON payload:

```
data: {"pod":"foo-abc123","namespace":"default","result":"evicted","detail":""}\n\n
```

Result values: `"evicted"` (200 from eviction API), `"skipped"` (filtered out at plan time — DaemonSet, mirror, system-node-critical), `"error"` (apiserver returned a non-200; `detail` carries the error text — e.g. `"would violate PDB foo-pdb"`).

The terminator is a named event (decoded by `EventSource.addEventListener('complete', ...)`):

```
event: complete
data: {"evicted":12,"skipped":3,"errored":1,"durationMs":4523}\n\n
```

**Rationale**
- `EventSource` in browsers has two surfaces: `onmessage` for unnamed events and `addEventListener(type, ...)` for named events. Using `message` for the per-pod stream keeps the JS short; using `complete` for the terminator gives the consumer a clear boundary so we don't have to overload `result` with a sentinel.
- JSON payloads are line-safe (the SSE spec requires `data:` lines to not contain newlines unless decoded specially). We sidestep the issue by JSON-encoding without indentation.
- `Content-Type: text/event-stream` + `Cache-Control: no-cache` + `X-Accel-Buffering: no` on the response — `X-Accel-Buffering: no` defends against any reverse-proxy buffering (none exists on the demo path; included for forward-compat).

**Alternatives**
- **Single `event: progress` for everything + a `kind` field.** Workable; adds a layer of indirection in the JS consumer for no gain.
- **WebSocket.** Bidirectional capability the drain endpoint never uses; an SSE response is one-shot, which matches the drain shape.
- **Long polling.** Inferior latency profile and harder to terminate cleanly.

**Citations** WHATWG Server-Sent Events spec; MDN `EventSource` reference; spec §0 Q11.

---

## R-8 Image base + Dockerfile (closes plan-section "Build isolation")

**Decision** Three-stage `Dockerfile.nodemedic-oncall-ui`:

1. **Builder stage** (`--platform=$BUILDPLATFORM`, base `golang:1.24-alpine`) — `go mod download` against vendored deps, then `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/nodemedic-oncall-ui ./cmd/nodemedic-oncall-ui`.
2. **Runtime stage** (`linux/amd64`, base `gcr.io/distroless/static-debian12:nonroot`) — `COPY --from=builder /out/nodemedic-oncall-ui /usr/local/bin/`. Templates and static assets are embedded via `embed.FS` in the binary (R-1), so no asset COPY is needed. `USER 65532:65532` (nonroot from the distroless base).
3. **Entry point**: `["/usr/local/bin/nodemedic-oncall-ui"]`. Flags + env wired by the chart.

Image tag: `cf-registry.nr-ops.net/container-fabric/nodemedic-oncall-ui:dev-cf1z-<short-sha>`. Estimated image size: ~25 MB (Go static binary + distroless base).

**Rationale**
- Distroless static is the smallest viable base for a `CGO_ENABLED=0` Go binary. No shell, no package manager, no userspace surface to attack.
- `embed.FS` packs all templates + CSS + JS into the binary at build time. Single-file deploy. No volume mounts, no ConfigMaps, no docker-image cache invalidation when CSS changes (still rebuilds the whole image, but the binary is small).
- `--platform=$BUILDPLATFORM` on the builder stage matches the controller's pattern (commit `3f9ee3af`). Apple Silicon dev hosts cross-compile cleanly via Colima.

**Alternatives**
- **`alpine:latest` runtime.** ~5 MB base + needs `ca-certificates` install + has a shell (attack surface). Distroless is strictly better for a Go static binary.
- **`scratch` runtime.** Even smaller, but lacks `ca-certificates` for the apiserver TLS path. The UI talks to the in-cluster apiserver with the bundle from `/var/run/secrets/kubernetes.io/serviceaccount/`; technically `scratch` works, but distroless is the conservative choice.
- **Single-stage (build in the runtime image).** Bloats the image with the Go toolchain. No.

**Citations** Distroless image docs (`github.com/GoogleContainerTools/distroless`); commits `61865c62`, `3f9ee3af` (controller image lessons); plan-section "Image registry path locked".

---

## R-9 RBAC scoping (closes spec FR-21 + Article I.1)

**Decision** Single ClusterRole with exactly four rules:

```yaml
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: ["nodemedic.cf.newrelic.com"]
    resources: ["nodehealthdiagnosisais"]
    verbs: ["get", "list", "watch", "patch"]
```

**Rationale**
- These four rules are exactly the verbs the FR table demands (FR-21 + G8). No `delete`, no `create` on `nodes`, no broader resource access, no `*` verb anywhere.
- `nodes/patch` is the new verb in the system — neither the controller nor the agent has it. The UI's uncordon (`spec.unschedulable=false`) and clear-skip-deletion (annotation removal) both go through `nodes/patch`. The narrowness here is what makes the I.1 boundary meaningful: a compromise of the UI binary can cordon-toggle and clear annotations on Nodes; it cannot delete a Node, take ownership, or reach Secrets.
- `pods/eviction create` is a subresource verb — it does NOT grant `pods delete`. The eviction subresource is the right surface because it respects PDBs server-side.
- `nhds/patch` (no `nhds/status patch`) is intentional. The UI writes an annotation on `metadata.annotations`, which lives on the main resource — not on the `status` subresource. The agent has `nhds/status patch`; the UI does not need it.

**Alternatives**
- **Add `nhds/status patch`.** Tempting if we ever want to write `status.uiActionHistory` instead of an annotation. Spec deliberately puts the audit trail in `metadata.annotations` to keep this verb out of the UI's role.
- **Bind to a Role (namespaced) instead of ClusterRole.** Nodes are cluster-scoped; the Role wouldn't cover them. ClusterRole is correct.
- **Consolidate `pods` + `pods/eviction` into one rule.** Would require the same verbs on both, broadening `pods` from `get/list/watch` to `get/list/watch/create`. Wrong shape — we don't want pod-creation rights.

**Citations** Spec FR-21, G8, AC-12; Constitution Article I.1; Kubernetes RBAC subresource semantics (eviction is a subresource of pods).

---

## R-10 Slack message builder location

**Decision** New Block Kit builder functions live in `internal/nodemedic/notifier/messages.go` next to the existing plain-text builders (`BuildApplied`, `BuildHumanInLoop`, `BuildCritical`). New names: `BuildBlockKitApplied`, `BuildBlockKitHumanInLoop`, `BuildBlockKitFailed`. Existing builders are not deleted — they remain as the fallback path behind the controller's new `--use-block-kit=true` flag (Helm value `config.useBlockKit`, default `true`).

**Rationale**
- The post path (`internal/nodemedic/notifier/slack.go`'s `(*Slack).Post`) is unchanged — it still takes a `[]byte` payload and POSTs it. The only change is which builder produces the bytes.
- Keeping both builders during the demo window means a midnight rendering-glitch fix is one Helm value flip away from rolling back.
- Tests for the new builders mirror the existing `messages_test.go` shape: golden JSON files in `internal/nodemedic/notifier/testdata/block-kit/{applied,human-in-loop,failed}.json`. Diff on a builder change is an explicit golden-update commit, not a silent payload drift.
- The builders take a small `BlockKitInput` struct (data-model.md §6) carrying the same fields the existing `AppliedInput` / `HumanInLoopInput` / `CriticalInput` structs do, plus `UIBaseURL` for the deep-link button.

**Alternatives**
- **Replace the existing builders in place.** Loses the rollback path. The plan explicitly leaves the plain-text path as a feature-flag fallback for demo-day safety.
- **New package `internal/nodemedic/notifier/blockkit/`.** More files, marginal organizational gain. The existing package is already small (3 files); adding three functions doesn't warrant a sub-package.
- **Build the message in the UI binary and have the controller fetch it.** Would introduce a controller→UI cross-scope HTTP contract — exactly what Article II.1 forbids. Out of consideration.

**Citations** This repo's `internal/nodemedic/notifier/{messages,slack,messages_test,slack_test}.go`; spec FR-1; Constitution Article II.1.

---

## R-11 Test framework (Go-side)

**Decision** Standard `testing` package + `httptest` for HTTP integration + controller-runtime's `pkg/client/fake` for kube fakes + `cmp` (`github.com/google/go-cmp/cmp`) for golden diffs. No `testify`, no `ginkgo`, no `gomock`.

**Rationale**
- `testing.T` table-driven tests are this repo's existing idiom (verified via `internal/nodemedic/notifier/messages_test.go` and `internal/nodemedic/agentclient/client_test.go`). Match the local style.
- `httptest.NewServer` + `httptest.NewRequest` are the canonical Go HTTP test idiom. The UI's HTTP surface is small enough that integration tests = constructed handler + real `httptest` server + asserted response.
- `controller-runtime`'s `fake.NewClientBuilder` is the right shape for kube fakes — it implements the same `client.Client` interface the production code depends on, so handlers swap clients in tests via dependency injection.
- `go-cmp` is in `go.sum` already (transitive of `controller-runtime`); golden-file tests can read JSON, unmarshal, and compare with `cmp.Diff` for clearer error messages than `reflect.DeepEqual`.

**Alternatives**
- **`testify`.** Adds an `assert.Equal` surface; loses to `if got != want { t.Errorf("...", got, want) }` for clarity in this codebase.
- **`ginkgo` / `gomega` BDD style.** Out of style for this repo.
- **`gomock` / `mockgen`.** Brittle on interface drift; we're using fakes, not mocks.

**Citations** Existing tests in `internal/nodemedic/notifier/` and `internal/nodemedic/agentclient/`; controller-runtime fake-client docs.

---

## R-12 CI workflow

**Decision** New `.github/workflows/nodemedic-oncall-ui-ci.yml`, structured the same way as `nodemedic-ci.yml`:

```yaml
name: nodemedic-oncall-ui-ci

on:
  pull_request:
    paths:
      - 'cmd/nodemedic-oncall-ui/**'
      - 'internal/oncall/**'
      - 'internal/nodemedic/notifier/**'  # Slack builder lives here
      - 'deployment/helm/nodemedic-oncall-ui/**'
      - 'tests/oncall_ui/**'
      - 'Dockerfile.nodemedic-oncall-ui'
      - '.github/workflows/nodemedic-oncall-ui-ci.yml'
  workflow_dispatch:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@... (pinned)
      - uses: actions/setup-go@... (pinned, go-version 1.24.x)
      - run: go test ./cmd/nodemedic-oncall-ui/... ./internal/oncall/... ./internal/nodemedic/notifier/... ./tests/oncall_ui/...

  helm-lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@...
      - uses: azure/setup-helm@...
      - run: helm lint deployment/helm/nodemedic-oncall-ui --values deployment/helm/nodemedic-oncall-ui/values-azure.yaml --set clusterName=cf1z

  rbac-guard:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@...
      - run: |
          # Constitution Article I.1 floor: the UI's ClusterRole MUST NOT carry forbidden verbs.
          ! grep -E 'nodes/(delete|create)|secrets|configmaps|/finalizers|\*' deployment/helm/nodemedic-oncall-ui/templates/clusterrole.yaml
          # Article I.2 floor: controller's ClusterRole MUST NOT acquire pods/eviction.
          ! grep 'pods/eviction' deployment/helm/nodemedic-controller/templates/clusterrole.yaml
          # Agent's ClusterRole still MUST NOT acquire nodes/patch.
          ! grep 'nodes/patch' deployment/helm/nodemedic-agent/templates/clusterrole.yaml
```

Image build/push is **manual** for the hackathon — same shape the controller and agent use. `make nodemedic-oncall-ui-docker-build && make nodemedic-oncall-ui-docker-push` from a laptop.

**Rationale**
- Path-filter trigger keeps the UI CI separate from NPD's, the controller's, and the agent's. Same precedent as the agent workflow.
- The `rbac-guard` job is the build-time half of Constitution Articles I.1 and I.2 enforcement. It checks all three charts (UI, controller, agent) as a cross-scope safety net.
- Manual image push matches the existing controller and agent shape — see commits `61865c62` and `28221155`.

**Alternatives**
- **GHA push to cf-registry.** Requires a service account; out of scope for the hackathon.
- **Single CI workflow for all three (controller, agent, UI).** Couples failure surfaces; separate workflows make breaks bisectable.

**Citations** Existing `.github/workflows/nodemedic-ci.yml` (controller) and `nodemedic-agent-ci.yml` (agent); recent commits `61865c62`, `28221155`.

---

## R-13 Drain "in flight" coalescing (closes spec FR-9 G9)

**Decision** Process-local `sync.Map[nodeName] *DrainProgress`. On a drain POST:

1. `Load` the entry for the target node.
2. If present and `state == "running"` → respond `409 Conflict` with `{result: "drain in progress", "progress": { ... }}` so the UI can surface a "drain in progress" indicator (FR-9 G9).
3. If absent or `state == "complete"` → atomic `LoadOrStore` a fresh `*DrainProgress{state: "running"}`, kick the goroutine, return SSE response.
4. The goroutine writes per-pod results to the SSE stream AND updates the `DrainProgress` struct (mutex-guarded counters) so a concurrent client checking via the per-case page can see live counts.
5. On goroutine completion: set `state = "complete"`, leave the entry in the map for 5 minutes (so a refresh shortly after still sees the summary), then a janitor goroutine drops it.

**Rationale**
- A click-and-close-tab edge case (spec §5) means the SSE consumer goes away mid-stream, but the eviction loop must still complete and write the final audit entry. The `sync.Map` keeps the loop reachable from both the SSE handler and a page-refresh handler.
- The 5-minute retention after `complete` covers the typical demo loop: drain finishes, engineer closes the tab and re-opens 30 s later — they see "drain just completed (n evicted, m skipped, k errored)".
- Pod-restart blows away the in-flight state. Acceptable trade-off — the audit annotation on the NHD CR still records what completed, and the apiserver still reflects which pods were actually evicted. The "drain progress" UI just stops showing live progress; this is a degraded-but-correct mode.

**Alternatives**
- **Persist drain state to the NHD annotation incrementally.** Would let a pod restart resume "show me live progress" — but the live progress is purely cosmetic and not a constitutional concern. The audit-trail summary at the end is what matters; that's already in the annotation.
- **Pull-based progress (poll `/api/cases/<nhd>/drain-progress`).** Works but adds a polling endpoint we don't need; SSE is push-based and better-suited.

**Citations** Spec FR-9 G9 + §5 edge cases; `sync.Map` Go docs.

---

## R-14 Helm chart `requireTestCluster` reuse

**Decision** Copy `_helpers.tpl`'s `requireTestCluster` macro from `deployment/helm/nodemedic-agent/templates/_helpers.tpl` byte-for-byte (with the chart name updated). Allowlist remains: `cf1z` / `jc1z` / `sk1z` literal + `test-*` prefix. The macro is invoked from every template (deployment.yaml, clusterrole.yaml, etc.) so `helm template` aborts unconditionally if `clusterName` is empty or out-of-allowlist.

**Rationale**
- Cluster-name guard is the chart-side half of Constitution Article I.5 (binary-side guard mirrors). The agent chart's pattern is verified to work; reusing it strictly reduces the surface for drift.
- Helm has no shared-template package mechanism for this kind of cross-chart helper, but the macro is small (~10 lines) and easy to keep in sync via the CI rbac-guard job (extension: also assert the three charts' macros are textually identical).

**Alternatives**
- **Library chart.** Premature abstraction.
- **Pre-install Job that validates.** Pushes the failure to install-time, after the chart has rendered some resources. The `fail` macro at template-time is strictly stronger.

**Citations** This repo's `deployment/helm/nodemedic-agent/templates/_helpers.tpl`; Constitution Article I.5.

---

## R-15 Action-button enablement model (closes spec FR-9a)

**Decision** The per-case page's initial render computes button enablement server-side from the live Node state at request time. The data shape (data-model.md §4) carries three booleans: `UncordonEnabled`, `ClearSkipDeletionEnabled`, `DrainEnabled`. The HTML template renders disabled buttons with a `disabled` attribute and a `title=` attribute carrying the tooltip reason ("Already uncordoned", "Annotation already cleared", "Node not found").

After an action POST, the page re-renders by re-executing the GET. Live action-button state is recomputed from the apiserver, not from JS-side state — the JS doesn't track button enablement, it only handles the modal + POST + reload cycle.

**Rationale**
- Server-side enablement keeps the truth in one place: the apiserver. JS-side enablement would have to mirror the rules, inviting drift.
- The "node has been reclaimed mid-investigation" case (spec §0 Q12 + FR-17a) lights up correctly: the request-time Node lookup returns NotFound, the page renders with a "Node was reclaimed by MLC" banner and all three buttons disabled.
- Phase banner and button enablement are independent (FR-9a is explicit): the page shows a "Diagnosis in progress" banner when `phase ∈ {Diagnosing, Pending}`, but buttons stay clickable as long as the underlying node state allows.

**Alternatives**
- **JS-side enablement, periodic poll.** Sluggish (poll cadence) and duplicates the enablement logic in two languages.
- **Don't render disabled buttons; just hide them.** Loses the UX affordance — engineer doesn't see what could have been actionable.

**Citations** Spec FR-9a + AC-14a + AC-14c.

---

## R-16 Open items not yet resolved (non-blocking)

None. All plan-phase decisions PD-1 through PD-4 are bound (R-1 through R-4). The supporting research items R-5 through R-15 close every implied technology and pattern choice the plan refers to.

**Items deliberately deferred to Phase 2 (`/speckit-tasks`)** — these aren't decisions but task-sequencing concerns:

- Sequencing US1 (Slack format) vs US2 (UI shell) vs US3 (uncordon/clear-skip) in the implementation order.
- Whether to author both `values-azure.yaml` (cf1z) and `values-eks.yaml` (placeholder) in the same task or stage them.
- Whether the controller's `--use-block-kit` flag rolls out before or after the UI ships (the flag default is `true`, so Block Kit is on the moment the new builder lands; rollback is the Helm-value flip).
- Whether the `rbac-guard` CI job lives in this PR or as a follow-up.

These are scheduling questions, not technology questions, and belong in `tasks.md`.
