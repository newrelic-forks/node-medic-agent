# Phase 1 Data Model — NodeMedic On-Call UI + Slack Format Upgrade

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-14

This document captures the entities the UI reads, writes, and reasons about. The UI introduces **no new CRD** and **no new cross-scope HTTP contract** — it reads the existing NHD CRD (Spec 001 owns the schema) and core Kubernetes Node/Pod resources, and writes one new annotation key on the NHD. This document is the **UI's model of those entities**, plus the small set of internal Go types the UI owns.

Entities below map to a Go file under `internal/oncall/...` per the plan's repo layout.

---

## 1. External entity: `NodeHealthDiagnosisAI` (NHD)

**Owner**: Spec 001 controller (schema authority — Constitution Article II.2). The UI's model of this CR is **read-only on `spec.*` and `status.*`, write-only on one annotation key in `metadata.annotations`**. The UI never `Create`s, never `Delete`s, never writes `status.*`, and never writes any `spec.*` field.

**Source**: [`api/v1alpha1/nodehealthdiagnosisai_types.go`](../../../api/v1alpha1/nodehealthdiagnosisai_types.go) — typed Go struct. The UI imports this package; no schema duplication.

**Go access**: typed `client.Client` from `sigs.k8s.io/controller-runtime/pkg/client`. Direct apiserver round-trips for `List`, `Get`, and `Patch` — no informer cache (FR-24: "MUST NOT cache NHD reads beyond a single request").

### 1.1 Fields the UI reads

| Path | Used by |
|---|---|
| `metadata.name` | URL routing key (`/cases/<nhd-name>`); audit-annotation patch target |
| `metadata.namespace` | Reach via Helm value `config.namespace`; default `cf-monitoring` |
| `metadata.creationTimestamp` | List view "Created" column (relative + absolute on hover) |
| `metadata.annotations["nodemedic.cf.newrelic.com/ui-action-history"]` | Action history merge with `status.action.*` |
| `spec.case.caseId` | Per-case page debug field; not surfaced in list view |
| `spec.case.nodeName` | List view "Node" column; per-case page header; FR-17 defensive belt for action endpoints |
| `spec.case.clusterName` | List view "Cluster" column |
| `spec.case.provider` | Per-case page metadata |
| `spec.case.region` | Per-case page metadata |
| `spec.case.instanceId` | Per-case page metadata |
| `spec.case.trigger.{type,reason,message,observedAt}` | List view "Trigger" column; per-case page metadata |
| `status.phase` | List view "Phase" column; per-case page status banner (FR-9a) |
| `status.conditions[*]` | Per-case page debug section (collapsed by default) |
| `status.diagnosis.rootCause` | Per-case page primary RCA paragraph |
| `status.diagnosis.rcaCategory` | List view "rcaCategory" inferred field; Slack header field |
| `status.diagnosis.confidence` | List view "Confidence" column; Slack header field |
| `status.diagnosis.evidence[*].{source,ref,result,observedAt}` | Per-case page evidence list (FR-10 collapsible bodies) |
| `status.diagnosis.recommendation.{action,reason}` | Per-case page recommendation panel |
| `status.diagnosis.modelUsed` | Per-case page metadata footer |
| `status.diagnosis.completedAt` | Per-case page metadata footer |
| `status.action.decision` | List view "Decision" column; per-case page action history |
| `status.action.operation` | Per-case page action history |
| `status.action.appliedAt` | Per-case page action history (timestamp source) |

### 1.2 Field the UI writes

Exactly one annotation key. Nothing else.

| Path | When | Validation |
|---|---|---|
| `metadata.annotations["nodemedic.cf.newrelic.com/ui-action-history"]` | After every successful action (FR-14, FR-15, FR-16); also after FR-17a "node reclaimed" responses | Value is JSON-encoded `[]UIActionEntry` (§3 below), capped at N=20 entries (FR-18a ring buffer). Total annotation value MUST stay under 8 KB (validated by AC-14b). |

### 1.3 Phase machine — UI is a reader, not a participant

The UI does NOT contribute to phase transitions. The agent owns `Diagnosing → Diagnosed` / `Diagnosing → Failed`; the controller owns `Diagnosed → Acted{Applied|HumanInLoop}` and any other transition. The UI **renders** the current phase as a status banner and as the list-view "Phase" column, but does not patch `status.phase`.

This is deliberate per Constitution Article II.1: every additional writer to the CR is a place where contracts can drift. Keeping the UI's writes scoped to one annotation key keeps the cross-scope surface minimal.

---

## 2. External entity: `Node` (kube core)

**Owner**: Kubernetes apiserver. The UI's model is **read on multiple fields, write on `spec.unschedulable` and on one annotation key removal**.

**Go access**: typed `corev1.Node` via the same controller-runtime split client.

### 2.1 Fields the UI reads

| Path | Used by |
|---|---|
| `metadata.name` | Lookup key (matched against `nhd.spec.case.nodeName`) |
| `metadata.annotations["machine-lifecycle.newrelic.com/skipDeletion"]` | Clear-skipDeletion button enablement (FR-9a); idempotency for FR-15 |
| `spec.unschedulable` | Uncordon button enablement (FR-9a); idempotency for FR-14 |
| `spec.providerID` | Per-case page debug (cross-check against `nhd.spec.case.instanceId`) |
| `status.conditions[type=Ready]` | Per-case page node-state panel |
| `metadata.labels` | Per-case page node-state panel (display only) |

### 2.2 Fields the UI writes

| Path | When | Idempotency |
|---|---|---|
| `spec.unschedulable` | `false` on uncordon (FR-14) | If already `false`, return `{result: "already uncordoned (no change)"}` and DO NOT write the audit annotation |
| `metadata.annotations["machine-lifecycle.newrelic.com/skipDeletion"]` | Removed on clear-skip-deletion (FR-15) | If already absent, return `{result: "already cleared (no change)"}` and DO NOT write the audit annotation |

The UI's RBAC (R-9) carries `nodes patch` for these two paths only. It has no `delete`, no `create`, no broader resource access on Nodes.

### 2.3 NotFound handling (FR-17a)

A `NotFound` on the target Node from any of the three action endpoints is treated as a benign "already reclaimed" state per spec §0 Q12. The endpoint:
1. Returns `200 OK` with body `{result: "node no longer exists (already reclaimed)"}`.
2. Appends a single audit entry capturing the action attempted and the not-found outcome (so the audit trail shows the engineer's intent + the system's response).
3. The per-case page on next render shows a "Node was reclaimed by MLC" banner and disables all three action buttons.

---

## 3. New entity: `UIActionEntry` (audit annotation payload)

**Module**: `internal/oncall/audit/annotation.go`

The schema for entries in the `nodemedic.cf.newrelic.com/ui-action-history` annotation. The annotation value is a JSON array of these entries. Schema reference: [`contracts/ui-action-history.schema.json`](./contracts/ui-action-history.schema.json).

```go
type UIActionEntry struct {
    // Ts is the action's wall-clock time, RFC3339 UTC.
    Ts time.Time `json:"ts"`

    // Action is the verb the UI invoked. One of:
    //   - "uncordon"
    //   - "drain"
    //   - "clear-skip-deletion"
    Action string `json:"action"`

    // Actor is the literal "demo-anonymous" for v1. The constitution
    // requires every action be observable; production restoration adds
    // SSO and a real user identifier here.
    Actor string `json:"actor"`

    // Result is the high-level outcome. One of:
    //   - "ok"          — action applied
    //   - "no-change"   — idempotent path; this entry is NEVER written (see §1.2)
    //   - "reclaimed"   — node not found; FR-17a path
    //   - "error"       — apiserver returned a non-200 we couldn't recover from
    //   - "partial"     — drain only; some pods evicted, some skipped/errored
    Result string `json:"result"`

    // Detail carries the human-readable summary line. Capped at 200 chars.
    // For drain: "evicted=12 skipped=3 errored=1". For uncordon: "". For
    // errors: the apiserver's truncated error text.
    Detail string `json:"detail,omitempty"`
}
```

**Idempotency**: per FR-14 / FR-15, no audit entry is written for the "no change needed" idempotent path. This keeps the ring buffer dense with actual state changes — an engineer hammering the uncordon button on an already-uncordoned node doesn't fill 20 entries with no-ops.

**FIFO ring buffer (FR-18a)**: when appending an entry would exceed N=20, drop entries from the front: `entries = entries[len(entries)-20:]` after the append. This keeps the buffer bounded and gives the engineer the **most recent** 20 events, not the **first** 20.

**Patch shape** (research R-6): JSON merge patch on `metadata.annotations`. The apiserver merges the one key without disturbing other annotations:

```json
{
  "metadata": {
    "annotations": {
      "nodemedic.cf.newrelic.com/ui-action-history": "<json-encoded array>"
    }
  }
}
```

`client.Patch(ctx, &nhd, client.MergeFrom(original))` produces this shape automatically.

---

## 4. Internal entity: `DetailPageData` (template input for `/cases/<nhd-name>`)

**Module**: `internal/oncall/render/data.go`

The struct passed to `html/template.Execute` for the per-case detail page. Computed by the detail handler from one NHD `Get` + one Node `Get`.

```go
type DetailPageData struct {
    // Identification
    NHDName     string
    Namespace   string
    CaseID      string
    NodeName    string
    ClusterName string

    // Trigger metadata
    Trigger TriggerView

    // Diagnosis (may be partial if phase ∈ {Diagnosing, Pending})
    Phase             string  // "Diagnosing" | "Pending" | "Diagnosed" | "Acted" | "Failed"
    PhaseBannerKind   string  // "" | "in-progress" | "failed"  (drives the banner CSS class)
    Diagnosis         DiagnosisView  // zero value if not yet written

    // Action history (merged: controller's status.action + UI annotation entries, sorted by ts ASC)
    ActionHistory []ActionHistoryRow

    // Live node state — drives button enablement (FR-9a)
    NodeExists                bool   // false → all buttons disabled, banner "Node was reclaimed by MLC"
    NodeUnschedulable         bool
    NodeHasSkipDeletion       bool
    UncordonEnabled           bool   // computed: NodeExists && NodeUnschedulable
    UncordonDisabledReason    string // tooltip text when disabled
    ClearSkipDeletionEnabled  bool   // computed: NodeExists && NodeHasSkipDeletion
    ClearSkipDeletionDisabledReason string
    DrainEnabled              bool   // computed: NodeExists
    DrainDisabledReason       string

    // Drain progress (from process-local sync.Map[nodeName] *DrainProgress, if any)
    DrainInProgress           bool
    DrainProgressEvictedCount int
    DrainProgressTotalCount   int
}

type TriggerView struct {
    Type       string
    Reason     string
    Message    string
    ObservedAt time.Time
}

type DiagnosisView struct {
    RootCause      string
    RCACategory    string
    Confidence     float64
    Evidence       []EvidenceView  // each with a Collapsible bool the template inspects
    Recommendation RecommendationView
    ModelUsed      string
    CompletedAt    time.Time
}

type EvidenceView struct {
    Source      string
    Ref         string
    Result      string
    ObservedAt  time.Time
    Collapsible bool  // true if len(Result) > 800 (FR-10)
}

type RecommendationView struct {
    Action string
    Reason string
}

type ActionHistoryRow struct {
    Ts       time.Time
    Actor    string  // "controller" | "UI: demo-anonymous"
    ActionVerb string // "Cordon" | "Uncordon" | "Drain" | "ClearSkipDeletion"
    Result   string
}
```

**Composition rules** (FR-11):
- Controller's `status.action` (if present, `decision != ""`) becomes one `ActionHistoryRow` with `Actor: "controller"`, `ActionVerb: "Cordon"` (since the controller only ever cordons), `Ts: status.action.appliedAt`, `Result: status.action.decision`.
- Each entry in the UI annotation becomes a row with `Actor: "UI: " + entry.Actor`, the verb mapped from `entry.Action` (`uncordon` → `Uncordon`, `drain` → `Drain`, `clear-skip-deletion` → `ClearSkipDeletion`), `Ts: entry.Ts`, `Result: entry.Result + (" — " + entry.Detail if non-empty)`.
- Sort by `Ts` ascending (oldest first). Engineer reads top-to-bottom; ascending puts the controller's `Cordon` before any UI follow-ups.

---

## 5. Internal entity: `ListPageRow` (template input for `/`)

**Module**: `internal/oncall/render/data.go`

One row per NHD in the last 24h. The list page template iterates over `[]ListPageRow`.

```go
type ListPageRow struct {
    NHDName        string
    NodeName       string
    ClusterName    string
    CreatedAt      time.Time
    TriggerType    string
    TriggerReason  string
    Phase          string
    Decision       string  // "Applied" | "HumanInLoop" | "" (pre-action)
    Confidence     float64 // -1 if diagnosis not yet written
    RowClass       string  // "row-applied" | "row-human-in-loop" | "row-failed" | "row-neutral" — drives FR-8 highlighting
}
```

The `GET /api/cases` JSON endpoint returns `[]ListPageRow` directly (`json.Marshal` on the slice). The list-view JS auto-refresh consumes this shape and re-renders the `<tbody>`.

**Filter rule** (FR-5): `metadata.creationTimestamp >= now - 24h`. Implemented by listing all NHDs in the namespace via a direct apiserver call and filtering in-process. The volume is small (≤ ~50 NHDs at peak demo cadence) and the LIST is single-digit-ms on cf1z, so the no-cache shape (FR-24) carries no perceptible cost.

**Sort rule**: `creationTimestamp` descending (newest first).

---

## 6. Internal entity: `BlockKitInput` (Slack builder argument)

**Module**: `internal/nodemedic/notifier/messages.go` (extended; not new file)

The struct passed to the new Block Kit builder functions. Same shape as the existing `AppliedInput` / `HumanInLoopInput` / `CriticalInput` so the controller's call site needs minimal changes — just one extra field (`UIBaseURL`).

```go
type BlockKitInput struct {
    NodeName    string
    ClusterName string
    Namespace   string
    NHDName     string
    UIBaseURL   string  // NEW — from controller's --ui-base-url flag (Helm value config.uiBaseURL)
    Diagnosis   *nodemedicv1alpha1.Diagnosis  // may be nil for BuildBlockKitFailed
    GateReason  string                         // populated only for HumanInLoop
    Failure     *FailureView                   // populated only for Failed
}

type FailureView struct {
    Reason   string  // "DeadlineExceeded" | "AgentUnreachable" | "BadRequest" | ...
    Detail   string
    Attempts int
}
```

**Builder functions** (R-10):

```go
// BuildBlockKitApplied returns the Block Kit JSON payload for a
// gate-pass + cordon-applied case. Header: "🚨 Cordoned: <node> (<cluster>)".
// attachment.color: "danger". Single primary button: "View full diagnosis"
// → "<UIBaseURL>/cases/<NHDName>".
func BuildBlockKitApplied(in BlockKitInput) ([]byte, error) { ... }

// BuildBlockKitHumanInLoop returns the Block Kit JSON payload for a
// gate-failure case. Header: "⚠️ Needs review: <node> (<cluster>)".
// attachment.color: "warning". Same button.
func BuildBlockKitHumanInLoop(in BlockKitInput) ([]byte, error) { ... }

// BuildBlockKitFailed returns the Block Kit JSON payload for a terminal
// agent failure. Header: "❌ Failed: <node> (<cluster>)".
// attachment.color: "#808080". Same button.
func BuildBlockKitFailed(in BlockKitInput) ([]byte, error) { ... }
```

Each builder produces the JSON bytes documented in [`contracts/slack-block-kit.md`](./contracts/slack-block-kit.md). Golden tests under `internal/nodemedic/notifier/testdata/block-kit/` lock the byte-for-byte output.

The existing `BuildApplied` / `BuildHumanInLoop` / `BuildCritical` functions stay as the fallback path (controller's `--use-block-kit=false` flag flips back to them). They are not deleted; they are reserved for emergency rollback.

---

## 7. Internal entity: `DrainProgress` (in-flight drain state)

**Module**: `internal/oncall/drain/progress.go`

Process-local state for an in-flight drain. Lives in a `sync.Map[nodeName] *DrainProgress`. Allows:
- Concurrent drain POSTs against the same node to coalesce (`409 Conflict` for the second click) — FR-9 G9.
- A page refresh during a drain to show live counts.

```go
type DrainState string

const (
    DrainStateRunning  DrainState = "running"
    DrainStateComplete DrainState = "complete"
)

type DrainProgress struct {
    mu sync.Mutex // guards counters

    NodeName  string
    State     DrainState
    StartedAt time.Time

    TotalPods   int  // set once at the start of the loop
    EvictedPods int  // incremented as evictions succeed
    SkippedPods int  // incremented for DaemonSet / mirror / system-node-critical
    ErroredPods int  // incremented for PDB violations and other apiserver errors

    CompletedAt time.Time  // zero until State = Complete
}
```

**Lifecycle**:
- A drain handler `LoadOrStore`s a fresh `*DrainProgress{State: Running}` keyed by node name. If `Load` returned an existing `Running` entry, the handler returns `409 Conflict` immediately.
- The eviction goroutine updates counters under the mutex as each pod completes.
- On goroutine exit: `State = Complete`, `CompletedAt = time.Now()`. Entry persists in the map for 5 minutes (a janitor goroutine drops it).
- Pod restart drops the map entirely. The audit annotation on the NHD CR still records the final outcome (the eviction loop writes it before the goroutine exits); only the live-progress UI affordance disappears.

---

## 8. Internal entity: `DrainSSEEvent` (over-the-wire shape)

**Module**: `internal/oncall/drain/execute.go`

The shape emitted to the SSE stream by `POST /api/cases/<nhd>/actions/drain`. Two events are emitted: per-pod (default `event: message`) and terminal (`event: complete`).

```go
type DrainPodEvent struct {
    Pod       string `json:"pod"`
    Namespace string `json:"namespace"`
    Result    string `json:"result"` // "evicted" | "skipped" | "error"
    Detail    string `json:"detail,omitempty"`
}

type DrainCompleteEvent struct {
    EvictedCount int   `json:"evicted"`
    SkippedCount int   `json:"skipped"`
    ErroredCount int   `json:"errored"`
    DurationMs   int64 `json:"durationMs"`
}
```

**On-the-wire format** (R-7):

```
data: {"pod":"foo-abc123","namespace":"default","result":"evicted","detail":""}\n\n
data: {"pod":"bar-def456","namespace":"kube-system","result":"skipped","detail":"DaemonSet"}\n\n
data: {"pod":"baz-ghi789","namespace":"default","result":"error","detail":"would violate PDB foo-pdb"}\n\n
event: complete
data: {"evicted":12,"skipped":3,"errored":1,"durationMs":4523}\n\n
```

Headers on the SSE response:
- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`
- `X-Accel-Buffering: no` — defends against proxy buffering on demo paths that grow ingress later

After each `Write`, the handler calls `w.(http.Flusher).Flush()` to push the bytes down the wire. The browser consumer uses `fetch(url, {method: "POST"})` + `response.body.getReader()` and a hand-rolled SSE-frame parser — not the native `EventSource` API, which is GET-only by spec (research R-7). The `fetch` path forfeits auto-reconnect; for transient network blips mid-stream the audit annotation on completion is still authoritative (the backend completes the eviction loop server-side regardless of whether the consumer is still attached).

---

## 9. Internal entity: agent runtime configuration

**Module**: `internal/oncall/server/server.go` (or a small `config.go` sibling)

CLI flags + env vars validated at startup. Mirrors the controller's flag-binding shape (`flag.StringVar(...)` in `cmd/.../main.go`).

```go
type Config struct {
    ListenAddr      string         // ":8080" — FR-19
    ClusterName     string         // FR-20 / FR-23 — chart-set; binary validates allowlist
    Namespace       string         // "cf-monitoring" — UI's NHD scope
    UIBaseURL       string         // for self-aware deep-links if any; usually empty (UI doesn't deep-link itself)
    LogLevel        string         // "info" | "debug" | "warn" | "error"
    DrainConcurrency int           // 3 (R-3); overrideable via flag for testing
    AuditBufferSize int            // 20 (FR-18a / R-6); overrideable for testing
    KubeContext     string         // empty in-cluster; useful for local dev
}
```

`validateClusterName` is called immediately after flag parsing — refuses `""` or any value not in `{cf1z, jc1z, sk1z}` and not `test-*`. Matches the controller's `cmd/nodemedic-controller/main.go` pattern verbatim.

---

## 10. Field-by-field validation summary (UI-side)

| Field | Validator | Failure handling |
|---|---|---|
| `POST /api/cases/<nhd>/actions/<verb>` path parameter `nhd` | `client.Get(ctx, types.NamespacedName{...}, &nhd)` | 404 page (per-case "Case not found") |
| Action endpoint request body's `nhdName` matches `<path-nhd>`? | (no body — actions are GET-shaped POSTs with no request payload; nhd comes from path) | n/a |
| `nhd.spec.case.nodeName` matches the node the action would target | `validateNodeMatchesCase` (FR-17 defensive belt) | 400 with `{result: "node mismatch"}` — UI never produces such requests but endpoint validates |
| Node existence | `client.Get` on Node | 200 with `{result: "node no longer exists (already reclaimed)"}` (FR-17a); audit entry written |
| Node already in target state? (uncordon when `unschedulable=false`; clear-skip when annotation absent) | per-handler check (FR-14 / FR-15 idempotency) | 200 with `{result: "already X (no change)"}`; NO audit entry written |
| Drain pod filter | `podShouldEvict` (R-3) — DaemonSet, mirror pod, `priority=system-node-critical`, terminating | filtered → SSE event `result=skipped`, `detail` names reason |
| Eviction PDB violation (apiserver returns 429) | per-pod handler in eviction loop | SSE event `result=error`, `detail` carries error text; loop continues with next pod |
| Audit annotation size | `len(json) <= 8192` after append (AC-14b) | If exceeded (shouldn't with N=20 cap), log WARN and truncate Detail fields |
| `clusterName` allowlist | binary `validateClusterName` (FR-23) | exit non-zero on startup |
| Block Kit JSON shape | golden tests in `internal/nodemedic/notifier/testdata/block-kit/` | CI breaks on diff; explicit golden-update commit unblocks |

---

## 11. Cross-references

- Cross-scope CRD schema: [`api/v1alpha1/nodehealthdiagnosisai_types.go`](../../../api/v1alpha1/nodehealthdiagnosisai_types.go) (Spec 001 authority)
- New annotation schema: [`contracts/ui-action-history.schema.json`](./contracts/ui-action-history.schema.json)
- UI HTTP endpoints: [`contracts/oncall-ui-api.yaml`](./contracts/oncall-ui-api.yaml)
- Slack Block Kit goldens: [`contracts/slack-block-kit.md`](./contracts/slack-block-kit.md)
- Constitution: [`.specify/memory/constitution.md`](../../memory/constitution.md)
- Spec acceptance criteria (the gate): [`spec.md`](./spec.md) §8 (AC-1 through AC-14, AC-14a/b/c)
- Plan: [`plan.md`](./plan.md)
- Phase 0 research: [`research.md`](./research.md)
