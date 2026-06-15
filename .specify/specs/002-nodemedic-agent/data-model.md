# Phase 1 Data Model — NodeMedic Agent

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-13

This document captures the entities the agent reads, writes, and reasons about. The cross-scope CRD schema is owned by Spec 001's `api/v1alpha1/nodehealthdiagnosisai_types.go` and vendored at [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml); this document is the **agent's model of those entities**, plus the small set of internal types the agent owns.

Entities below map 1:1 to a Python module in `nodemedic_agent/...` per the plan's repo layout.

---

## 1. External entity: `NodeHealthDiagnosisAI` (NHD)

**Owner**: Scope 3 (Constitution Article II.2). The agent's model of this CR is **the agent writes `status.diagnosis` + a narrow slice of `status.phase` and `status.conditions[]`**, and reads `spec.case.*` + `spec.budgets.*` from the controller's `Create`. The agent never `Create`s, never writes `spec.*`, never writes `status.action.*`, and never writes `status.evaluation.*`.

**Source**: [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml) — vendored from Spec 001's frozen v1alpha1 schema.

**Python access**: via `kubernetes.client.CustomObjectsApi` (R-3). No generated Python types; the dynamic client returns `dict`s. Pydantic models in `nodemedic_agent/tools/emit_report.py` and `nodemedic_agent/api/diagnose.py` validate the shapes the agent produces or receives.

### 1.1 Fields the agent writes

| Path | When | Validation |
|---|---|---|
| `status.phase` | On `emit_report` success and on terminal failures | enum: `Diagnosed` (success) or `Failed` (failure). Never `Acted`/`Evaluating`/`Evaluated` — those belong to other scopes. |
| `status.conditions[ReportReady]` | Same writes as `status.phase` | Type=`ReportReady`, `status=True` on `Diagnosed` with `reason=EvidenceValid`; `status=False` on `Failed` with `reason ∈ {ModelHalted, ModelError, ToolError, CRWriteFailed}` (FR-11). |
| `status.diagnosis.rootCause` | On `emit_report` success | non-empty string ≤ 4 KB (FR-8 step 1). |
| `status.diagnosis.rcaCategory` | On `emit_report` success | enum `{Conntrack, FD, PID, Inode, Disk, DNS, IMDS, Kernel, Kubelet, Unknown}` (FR-8). |
| `status.diagnosis.confidence` | On `emit_report` success | float `[0.0, 1.0]` (FR-8). |
| `status.diagnosis.evidence[]` | On `emit_report` success | list, length ≥ 1; each item `{source ∈ {nrql, ssh, kubectl, cloud, proc, log}, ref, result≤4KB, observedAt}` (FR-8). |
| `status.diagnosis.recommendation.action` | On `emit_report` success | enum `{Cordon, DrainAndCordon, NoAction}`. |
| `status.diagnosis.recommendation.reason` | On `emit_report` success | non-empty string. |
| `status.diagnosis.modelUsed` | On every terminal write (success or failure) | resolved primary or fallback model ID at startup (FR-3). Best-effort on failures. |
| `status.diagnosis.completedAt` | On every terminal write | RFC3339, set by the runner from `time.time_ns()`. |
| `status.diagnosis.turnsUsed` | Best-effort on terminal write | from SDK metadata if available, else omit (FR-7). |
| `status.diagnosis.costUSD` | Best-effort on terminal write | from SDK metadata if available, decimal-as-string, else omit. |
| `status.diagnosis.auditLogRef.objectStore` | Always empty | constitution defers durable JSONL; field exists in CRD for forward-compat but agent leaves it empty (FR-7). |

### 1.2 Fields the agent reads (never writes)

| Path | Used by |
|---|---|
| `spec.case.caseId` | Case-table key (FR-10), `emit_report` payload echo |
| `spec.case.nodeName` | User-prompt injection (FR-3), CR write target via owner reference |
| `spec.case.clusterName` | User-prompt injection |
| `spec.case.provider` | Cloud-dispatch in runbook (FR-6); user-prompt injection |
| `spec.case.region` | User-prompt injection |
| `spec.case.instanceId` | User-prompt injection |
| `spec.case.trigger.{type,reason,message,observedAt}` | User-prompt injection |
| `spec.budgets.maxTurns` | Echoed best-effort into `status.diagnosis.turnsUsed`; NOT enforced (FR-7) |
| `spec.budgets.maxBudgetUSD` | Echoed best-effort into `status.diagnosis.costUSD`; NOT enforced (FR-7) |
| `spec.budgets.deadlineSec` | Logged at INFO at case start; NOT enforced (FR-7) |
| `status.phase` | FR-12 phase-conflict pre-write check (`Diagnosed`/`Acted`/`Failed` → defer write) |

### 1.3 Phase machine writes the agent participates in

The phase machine itself is owned by the controller (Spec 001 data-model.md §1.3). The agent only contributes the two transitions below:

```
Diagnosing ──► Diagnosed     (on emit_report success, FR-8)
Diagnosing ──► Failed        (on model halt / error / tool error / CR write failure, FR-11)
```

Anything the agent observes in any other phase before write triggers FR-12's defer-write rule (no overwrite). The `Diagnosing` state is set by the controller after `POST /diagnose` returns 202.

### 1.4 NHD identification

The agent finds the CR by **`metadata.name` derived from `caseId`**, but the canonical lookup path is via `spec.case.caseId == caseId`. Specifically:
- The `POST /diagnose` body carries `caseId` and `nodeName`.
- The controller's name convention (Spec 001 data-model.md §1.5) is `<nodeNameTruncated50>-<unixTsSeconds>`. The agent does NOT reconstruct this — instead it uses a `kubernetes.CustomObjectsApi.list_namespaced_custom_object` call with `field-selector` on `spec.case.caseId` (or, in the fallback path if the CRD doesn't have a status field index, list+filter in-process).

  *Practical implementation note for tasks.md:* k8s CRDs can't have arbitrary fields used in `field-selector` without explicit registration. The simplest implementation is to receive `metadata.name` directly in the `POST /diagnose` body, which the controller already knows since it just `Create`d the CR. The plan-phase resolution: extend the `POST /diagnose` body to include `nhdName` (canonically derived but eliminates the lookup race) — coordinate with Spec 001 in tasks.md.

  Fallback if Spec 001 declines the body extension: list NHDs in the namespace, filter by `spec.case.caseId == caseId` in Python. The volume is small (concurrent cases ≤ 32 per NFR-8) and the call is cheap. Either path satisfies FR-8.

---

## 2. Internal entity: `Case` (in-memory)

In-memory only; never persisted. Lives inside the case table (FR-10) for the duration of the agent process, retention 30 minutes after completion.

**Module**: `nodemedic_agent/runner/case_table.py`

```python
from datetime import datetime
from enum import Enum
from typing import Optional

from pydantic import BaseModel

class CaseStatus(str, Enum):
    QUEUED = "queued"
    RUNNING = "running"
    COMPLETE = "complete"

class Case(BaseModel):
    case_id: str                       # UUIDv4
    node_name: str
    cluster_name: str
    provider: str                      # "aws" | "azure"
    region: str
    instance_id: str
    nhd_name: Optional[str]            # populated if Spec 001 sends it; else resolved by list
    trigger_type: str
    trigger_reason: str
    trigger_message: str
    trigger_observed_at: datetime
    deadline_sec: int
    max_turns: int
    max_budget_usd: str

    status: CaseStatus
    started_at: datetime
    completed_at: Optional[datetime]
    final_phase: Optional[str]         # "Diagnosed" or "Failed" once complete
    failure_reason: Optional[str]      # one of FR-11's reasons
```

The case table is a `dict[str, Case]` keyed by `case_id`, guarded by a single `asyncio.Lock` for the register/lookup path. The 30-minute retention sweep runs on a background `asyncio.Task` that wakes every 5 minutes and drops `COMPLETE` cases older than 30 minutes (FR-10).

**Idempotency rules** (FR-10):
- New `case_id` not in table → register as `QUEUED`, spawn worker, return `202 {status: "queued"}`.
- Existing `case_id` in `QUEUED` or `RUNNING` → return `202 {status: "queued"}`. Do NOT spawn a second worker.
- Existing `case_id` in `COMPLETE` (within 30-min retention) → return `202 {status: "complete"}`.
- After 30 minutes the record is dropped; a re-POST starts a fresh worker.

---

## 3. Internal entity: `DiagnoseRequest` (the `POST /diagnose` body)

**Module**: `nodemedic_agent/api/diagnose.py`

Mirrors the controller's `DiagnoseRequest` Go struct in [`internal/nodemedic/agentclient/types.go`](../../../internal/nodemedic/agentclient/types.go) byte-for-byte. The contract reference is [`contracts/post-diagnose.md`](./contracts/post-diagnose.md). Validated by FastAPI / Pydantic on entry.

```python
from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

class DiagnoseTrigger(BaseModel):
    type: str = Field(min_length=1)
    reason: str = ""
    message: str = Field(default="", max_length=256)
    observed_at: datetime = Field(alias="observedAt")

class DiagnoseBudgets(BaseModel):
    max_turns: int = Field(alias="maxTurns", ge=1)
    max_budget_usd: str = Field(alias="maxBudgetUSD", pattern=r"^[0-9]+\.[0-9]{2}$")
    deadline_sec: int = Field(alias="deadlineSec", ge=1)

class DiagnoseRequest(BaseModel):
    case_id: str = Field(
        alias="caseId",
        pattern=r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$",
    )
    node_name: str = Field(alias="nodeName", min_length=1, max_length=253)
    cluster_name: str = Field(alias="clusterName", min_length=1)
    provider: Literal["aws", "azure"]
    region: str = Field(min_length=1)
    instance_id: str = Field(alias="instanceId", min_length=1)
    trigger: DiagnoseTrigger
    budgets: DiagnoseBudgets
    nhd_name: Optional[str] = Field(default=None, alias="nhdName")  # plan-phase extension; see §1.4

class DiagnoseResponse(BaseModel):
    case_id: str = Field(serialization_alias="caseId")
    status: Literal["queued", "complete"]
```

Pydantic v2's `alias` machinery preserves the camelCase wire shape while the Python code uses snake_case attributes. JSON tags are byte-stable; the golden body in `tests/nodemedic_agent/fixtures/case_aws.json` locks them.

---

## 4. Internal entity: `EmitReportPayload` (the terminal-tool args)

**Module**: `nodemedic_agent/tools/emit_report.py`

This is the schema the SDK enforces on the agent's `emit_report` tool call. Contract reference: [`contracts/emit-report-tool.md`](./contracts/emit-report-tool.md).

```python
from datetime import datetime
from typing import Literal

from pydantic import BaseModel, Field

EvidenceSource = Literal["nrql", "ssh", "kubectl", "cloud", "proc", "log"]
RCACategory = Literal["Conntrack", "FD", "PID", "Inode", "Disk", "DNS", "IMDS", "Kernel", "Kubelet", "Unknown"]
RecommendationAction = Literal["Cordon", "DrainAndCordon", "NoAction"]

class EmitReportEvidence(BaseModel):
    source: EvidenceSource
    ref: str = ""
    result: str = Field(default="", max_length=4096)  # truncated by writer if larger
    observed_at: datetime = Field(alias="observedAt")

class EmitReportRecommendation(BaseModel):
    action: RecommendationAction
    reason: str = Field(min_length=1)

class EmitReportPayload(BaseModel):
    root_cause: str = Field(alias="rootCause", min_length=1, max_length=4096)
    rca_category: RCACategory = Field(alias="rcaCategory")
    confidence: float = Field(ge=0.0, le=1.0)
    evidence: list[EmitReportEvidence] = Field(min_length=1)  # ≥1; spec FR-8 has no count gate
    recommendation: EmitReportRecommendation
```

**Validation order** (FR-8 step 1):
1. Pydantic validates structural shape (raises `ValidationError` on any violation).
2. The runner truncates `result` to 4 KB if longer (lenient — log a WARN).
3. The runner builds the NHD `status.diagnosis` payload by mapping these fields verbatim (no transformation), adding `modelUsed` / `completedAt` / `turnsUsed` / `costUSD` from runner state.
4. The runner consults FR-12's phase-conflict matrix (re-read CR's `status.phase`) and either writes via `replace_namespaced_custom_object_status` or defers (per the §1.1 / §1.3 rules).

**No agent-side count gate** — single-evidence reports are accepted; the controller's confidence gate is the single enforcement layer (G5 / FR-8 step 2 / AC-6).

---

## 5. Internal entity: `ToolCallLog` (NFR-3 stdout shape)

**Module**: `nodemedic_agent/runner/hooks.py`

Emitted on every `PreToolUse` hook fire — every `Bash` invocation, every NR MCP call, every SDK built-in call, every `emit_report` call. JSON line per call to stdout.

```python
class ToolCallLog(BaseModel):
    ts: datetime                       # RFC3339, UTC
    case_id: str
    turn: int                          # SDK's turn counter; -1 if unavailable
    tool: str                          # "Bash" | "nr.<name>" | "mcp__nodemedic__emit_report" | "Read" | "WebFetch" | …
    command: Optional[str] = None      # for Bash, truncated to 2 KB
    args: Optional[dict] = None        # for non-Bash, truncated to 2 KB
    latency_ms: Optional[int] = None   # populated on PostToolUse if available; None on PreToolUse
    exit_code: Optional[int] = None    # for Bash, populated on PostToolUse
    stdout_bytes: Optional[int] = None # for Bash; size only, not contents
    stderr_bytes: Optional[int] = None # for Bash; size only, not contents
    level: Literal["info", "warn", "error"] = "info"
```

**Levels** (NFR-3):
- `info` for happy-path tool calls.
- `warn` for retries (NHD `Status().Update` 409, NotFound retry exhaustion-but-recovery, non-zero `Bash` exit codes).
- `error` for terminal failures (RBAC denied, schema validation, Anthropic API hard error).

**Case-complete log line** — emitted last per case, separately from per-tool-call lines:

```python
class CaseCompleteLog(BaseModel):
    ts: datetime
    case_id: str
    write_outcome: Literal["written", "deferred_phase_conflict", "write_failed"]
    final_phase: Optional[Literal["Diagnosed", "Failed"]] = None  # what the agent attempted to write; None on deferred
    observed_phase: Optional[str] = None  # populated on deferred_phase_conflict (e.g. "Failed", "Acted")
    failure_reason: Optional[str] = None  # populated on write_failed and on agent-side Failed terminal states
    duration_ms: int
    model_resolved: str
    turns_used: Optional[int] = None
    cost_usd: Optional[str] = None
```

Three terminal shapes:
- **`written`** — happy path; `final_phase` is `Diagnosed` (success) or `Failed` (agent-side terminal, FR-11).
- **`deferred_phase_conflict`** — FR-12 fired; `observed_phase` carries what the runner saw on the pre-write read (`Diagnosed`/`Acted`/`Failed`). `final_phase` is None because nothing was written.
- **`write_failed`** — FR-8 retry exhausted or terminal class hit (`403`/`422`); `failure_reason` carries the apiserver error.

Both shapes share `case_id` so `kubectl logs … | jq 'select(.case_id == "<id>")'` reconstructs the full reasoning chain (NFR-3 + AC-9).

---

## 6. Internal entity: `ResolvedModels` (FR-3 startup resolution)

**Module**: `nodemedic_agent/runner/model_resolver.py`

Computed once at startup, cached for the process lifetime, exposed via `/readyz`.

```python
class ResolvedModels(BaseModel):
    primary_requested: str             # e.g. "claude-opus-4-7"
    primary_resolved: str              # what the gateway returned, possibly fallback
    fallback_requested: str            # e.g. "claude-sonnet-4-6"
    fallback_resolved: str
    resolution_path: Literal["exact", "best-opus", "best-sonnet"]
    resolved_at: datetime
```

**Resolution algorithm** (FR-3):
1. Fetch the gateway's model catalog via `GET https://nerd-completion.staging-service.nr-ops.net/v1/models` (or whatever the catalog endpoint resolves to — runner attempts the documented path first; on 404 the runner falls back to a best-effort `POST` against the requested model with `max_tokens=1` to verify availability).
2. If `CLAUDE_MODEL` is in the catalog → `primary_resolved = primary_requested`, `resolution_path = "exact"`.
3. Else, find the highest-version Opus in the catalog → `primary_resolved = <that ID>`, `resolution_path = "best-opus"`.
4. Else, find the highest-version Sonnet in the catalog → `primary_resolved = <that ID>`, `resolution_path = "best-sonnet"`.
5. Else, fail `/readyz` with HTTP 503 and exit code non-zero on the next probe.

The fallback model is resolved with the same algorithm but starts from `CLAUDE_FALLBACK_MODEL`.

Resolved IDs MUST be logged at INFO at startup with `event=model_resolved primary=… fallback=…` (FR-3 / AC-1).

---

## 7. Internal entity: agent runtime configuration

**Module**: `nodemedic_agent/config.py`

Pydantic Settings — env vars validated at startup. Maps directly to spec NFR-6's table.

```python
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings

class Settings(BaseSettings):
    agent_listen_addr: str = ":8080"
    max_concurrent_cases: int = Field(default=32, ge=1)
    claude_model: str = "claude-opus-4-7"
    claude_fallback_model: str = "claude-sonnet-4-6"
    anthropic_auth_token: str = Field(min_length=1)         # from Secret/nodemedic-anthropic-token
    anthropic_base_url: str = "https://nerd-completion.staging-service.nr-ops.net"
    nr_mcp_url: str = Field(min_length=1)
    nr_mcp_token: str = Field(min_length=1)                 # from Secret/nodemedic-nr-token
    nr_account_id: int = 1                                  # locked at staging
    azure_tenant_id: str = ""                               # required only on Azure install
    azure_client_id: str = ""
    azure_client_secret: str = ""
    ssh_key_path: str = "/etc/nodemedic/ssh/id_ed25519"
    log_level: Literal["debug", "info", "warn", "error"] = "info"
    readyz_probe_timeout_sec: int = 5
    runbook_path: str = "/app/prompts/runbook.md"
    cluster_name: str = Field(min_length=1)                 # from Helm --cluster-name; chart-guarded
    cloud_provider: Literal["aws", "azure"] = Field()       # which cloud's creds are mounted
    kube_namespace: str = "cf-monitoring"

    class Config:
        env_prefix = ""                                     # env vars carry their literal NFR-6 names
        case_sensitive = False
```

**Validation behavior**: any required field missing or wrong-typed raises `pydantic_core.ValidationError` at process start; uvicorn fails to launch. The agent never lazily resolves config inside the request path.

---

## 8. Internal entity: NHD writer

**Module**: `nodemedic_agent/kube/nhd_writer.py`

Wraps `kubernetes.client.CustomObjectsApi` to expose two methods the runner uses:

```python
class NHDWriter:
    async def get(self, namespace: str, name: str) -> dict:
        ...  # GET via CustomObjectsApi; returns the raw dict the dynamic client gives us.

    async def update_status(
        self,
        namespace: str,
        name: str,
        status_diagnosis: dict,
        final_phase: Literal["Diagnosed", "Failed"],
        failure_reason: Optional[str] = None,
    ) -> WriteOutcome:
        """
        Pre-read the CR. Apply FR-12 phase-conflict matrix:
          - "" / "Pending" / "Diagnosing" → proceed
          - "Diagnosed" / "Acted" / "Failed" → return WriteOutcome.DEFERRED_PHASE_CONFLICT
            (log INFO with caseId, observed_phase, intended_payload — do NOT raise)
        Build the new status payload. Call replace_namespaced_custom_object_status.
        Apply the FR-8 retry policy (R-13):
          - 404 NotFound: retry 3 times at 250ms / 500ms / 1s
          - 409 Conflict on status: re-read, retry once after 1s
          - other 5xx / network: retry once after 1s
          - 403 / 422: terminal — return WriteOutcome.WRITE_FAILED (no retry)
        Return WriteOutcome.WRITTEN on success.
        """
        ...
```

```python
class WriteOutcome(str, Enum):
    WRITTEN = "written"
    DEFERRED_PHASE_CONFLICT = "deferred_phase_conflict"
    WRITE_FAILED = "write_failed"
```

The runner branches on `WriteOutcome` to populate `Case.final_phase` / `Case.failure_reason` and emit the case-complete log line accordingly.

---

## 9. Field-by-field validation summary (agent-side)

| Field | Validator | Failure handling |
|---|---|---|
| `POST /diagnose` body shape | Pydantic v2 (`DiagnoseRequest`) | 400 with Pydantic's error JSON |
| `caseId` UUIDv4 | Pydantic regex | 400 |
| `provider` enum | Pydantic Literal | 400 |
| Concurrency cap | `asyncio.Semaphore(MAX_CONCURRENT_CASES)` | 429 (FR-1) |
| Runbook contents | `tests/nodemedic_agent/check_runbook.sh` (R-7) at build/install time | Helm install fails |
| `emit_report` payload shape | Pydantic v2 (`EmitReportPayload`) | runner returns the SDK an error tool result; agent CR gets `Failed{reason=ToolError}` |
| `confidence ∈ [0,1]` | Pydantic `ge=0, le=1` | same as above |
| `evidence` non-empty | Pydantic `min_length=1` | same as above |
| `evidence[].result ≤ 4 KB` | Pydantic `max_length=4096` (truncation upstream of validation in the writer if needed) | same as above |
| `recommendation.action` enum | Pydantic Literal | same as above |
| NHD `status` write — `phase` pre-check | runner's `should_overwrite` pure function | defer write, log INFO (FR-12) |
| NHD `status` write — apiserver response | `kubernetes.client.exceptions.ApiException.status` | retry per R-13 / FR-8 |

CRD-level OpenAPI validation at the apiserver catches most of these at write time as a defense-in-depth layer; agent-side validation produces clearer errors and avoids round-tripping garbage to the apiserver.

---

## 10. Cross-references

- Cross-scope CRD schema: [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml) (vendored from Spec 001)
- Cross-scope HTTP contract: [`contracts/post-diagnose.md`](./contracts/post-diagnose.md)
- Terminal-tool contract: [`contracts/emit-report-tool.md`](./contracts/emit-report-tool.md)
- Constitution: [`.specify/memory/constitution.md`](../../memory/constitution.md)
- Spec acceptance criteria (the gate): [`spec.md`](./spec.md) §8 (AC-1 through AC-15)
- Plan: [`plan.md`](./plan.md)
- Phase 0 research: [`research.md`](./research.md)
