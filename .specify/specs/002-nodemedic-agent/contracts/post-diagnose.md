# Contract — `POST /diagnose` (agent-side receiver)

**Authority**: Spec 001's `contracts/post-diagnose.md` is the controller-side reference; this file is the agent-side reference + frozen golden bodies for the agent's golden-file tests. The two MUST stay in sync — schema changes need a coordinated PR per Constitution Article II.2.

**Direction**: controller → agent. **Auth**: hackathon scope has no `/diagnose` auth (spec FR-1); the agent ignores any `Authorization: Bearer …` header it receives. **Default URL** the controller uses: `http://nodemedic-agent.cf-monitoring.svc:8080/diagnose` (set by the controller's `--agent-url` flag; default matches `deployment/helm/nodemedic-controller/values-azure.yaml:21`).

---

## Request

```http
POST /diagnose HTTP/1.1
Host: nodemedic-agent.cf-monitoring.svc:8080
Content-Type: application/json
Content-Length: <n>
```

(`Authorization` header is ignored if present.)

### JSON body schema

| Field | Type | Required | Notes |
|---|---|---|---|
| `caseId` | string (UUIDv4) | yes | Echoed back in the response body and in `emit_report` payload. |
| `nodeName` | string | yes | apiserver Node name. |
| `clusterName` | string | yes | from controller's `--cluster-name`; agent does not validate prefix. |
| `provider` | enum `aws \| azure` | yes | resolved by controller per Spec 001 FR-2. |
| `region` | string | yes | e.g. `us-east-2` or `eastus2`. |
| `instanceId` | string | yes | AWS instance-id or Azure VM name. |
| `trigger.type` | string | yes | NodeCondition type. cf1z Day-1 paths: `ContainerRuntimeUnhealthy` (AC-3) or `KubeletUnhealthy` (AC-3b). |
| `trigger.reason` | string | optional | NPD rule reason. |
| `trigger.message` | string (≤256B) | optional | NPD rule message, may be truncated by controller. |
| `trigger.observedAt` | string (RFC3339) | yes | when NPD flipped Condition to True. |
| `budgets.maxTurns` | int | yes | echoed best-effort into `status.diagnosis.turnsUsed`; NOT enforced by agent (spec FR-7). |
| `budgets.maxBudgetUSD` | string (decimal) | yes | echoed best-effort into `status.diagnosis.costUSD`; NOT enforced (spec FR-7). |
| `budgets.deadlineSec` | int | yes | accepted but ignored — no agent-side wall-clock deadline (spec FR-7). |
| `nhdName` | string | optional | Plan-phase extension for Spec 002. If provided, agent uses it as the canonical NHD lookup key (skips the list+filter fallback in data-model.md §1.4). Spec 001 must opt in to send this; default behavior with the field absent still works via the fallback path. |

### Validation rules (Pydantic v2, FR-1 / data-model.md §3)

- `caseId` matches UUIDv4 regex (`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`).
- `provider ∈ {aws, azure}` exactly.
- All required strings non-empty.
- `trigger.observedAt` parseable as RFC3339.
- `budgets.maxBudgetUSD` matches `^[0-9]+\.[0-9]{2}$`.
- Any failure → `400 Bad Request` with FastAPI's default Pydantic error response body.

### Golden request body (used in `tests/nodemedic_agent/fixtures/case_aws.json`)

Mirrors the controller's golden body in [`../../001-nodemedic-controller/contracts/post-diagnose.md`](../../001-nodemedic-controller/contracts/post-diagnose.md) byte-for-byte except for the optional `nhdName` extension:

```json
{
  "caseId": "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c",
  "nodeName": "ip-10-1-2-3.ec2.internal",
  "clusterName": "test-odd-wire",
  "provider": "aws",
  "region": "us-east-2",
  "instanceId": "i-0abc1234deadbeef",
  "trigger": {
    "type": "ConntrackSaturated",
    "reason": "ConntrackHigh",
    "message": "nf_conntrack_count=262100 max=262144",
    "observedAt": "2026-06-12T15:00:00Z"
  },
  "budgets": {
    "maxTurns": 15,
    "maxBudgetUSD": "0.50",
    "deadlineSec": 60
  }
}
```

### Golden cf1z (Azure) body (used in `tests/nodemedic_agent/fixtures/case_azure.json`)

The shape the controller will actually send on cf1z. Note `provider=azure`, the Azure VM-name `instanceId`, and the Day-1 `ContainerRuntimeUnhealthy` trigger:

```json
{
  "caseId": "1a2b3c4d-5e6f-4789-9abc-def012345678",
  "nodeName": "cf1z-general-nodes-2000002",
  "clusterName": "cf1z",
  "provider": "azure",
  "region": "eastus2",
  "instanceId": "cf1z-general-nodes-2000002",
  "trigger": {
    "type": "ContainerRuntimeUnhealthy",
    "reason": "ContainerdUnreachable",
    "message": "containerd socket /run/containerd/containerd.sock unreachable",
    "observedAt": "2026-06-13T14:00:00Z"
  },
  "budgets": {
    "maxTurns": 15,
    "maxBudgetUSD": "0.50",
    "deadlineSec": 60
  }
}
```

---

## Response — async

Idempotency rules (spec FR-10):

| Existing case state | Response | Meaning |
|---|---|---|
| not present | `202 {caseId, status: "queued"}` | new case, worker spawned |
| `queued` or `running` | `202 {caseId, status: "queued"}` | duplicate POST during in-flight case; no second worker spawned |
| `complete` (within 30-min retention) | `202 {caseId, status: "complete"}` | duplicate POST after success/failure; controller will see CR result via informer |
| `complete` (older than 30-min retention) | treated as not-present (entry purged); `202 {caseId, status: "queued"}` | re-attempt on a stale case starts fresh |

Standard 202 response shape:

```http
HTTP/1.1 202 Accepted
Content-Type: application/json

{ "caseId": "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c", "status": "queued" }
```

The controller does not branch on `status` (per Spec 001 research R-5) — it relies on the CR's eventual `status.diagnosis` either way.

---

## Errors and agent behavior

| HTTP code | When | Body | Notes |
|---|---|---|---|
| 202 | new case spawned, or duplicate during in-flight, or duplicate post-completion | `{caseId, status: "queued" \| "complete"}` | see idempotency table |
| 400 | malformed body / wrong types / missing required field | FastAPI's Pydantic error JSON | NO retry from controller (Spec 001 contract) |
| 401 | n/a — auth disabled for hackathon (spec FR-1) | — | The agent never returns 401. Controller's 401 handling stays valid for production restoration. |
| 429 | concurrency cap reached (`MAX_CONCURRENT_CASES` slots full) | `{caseId, status: "rejected", reason: "concurrency_cap"}` | Controller backs off per its FR-7 (1s/2s/4s × 4 attempts max). |
| 5xx | internal panic / unhandled exception | FastAPI default error envelope | Reserved for genuine internal failure — never used as a "soft retry me" signal. |

The agent never returns 503 from `/diagnose` itself — `/readyz` is the readiness signal.

---

## Idempotency contract (re-statement of spec FR-10)

The controller may issue `POST /diagnose` more than once with the same `caseId` (controller spec FR-7's retry path). The agent MUST:
- Look up `caseId` in the in-memory case table FIRST, before checking the concurrency cap.
- Return `202` with the appropriate `status` per the idempotency table above.
- NEVER spawn a second case worker for the same `caseId` within the 30-minute retention window.

This invariant is locked by the integration test `tests/nodemedic_agent/integration/test_idempotent.py` — POST the same body twice during an in-flight case, assert exactly one `case_started` log line in stdout.

---

## Auth restoration path (production hardening)

Spec FR-1 carries the rationale for dropping auth in the hackathon:
- Service is `ClusterIP`, not exposed outside the cluster.
- Cluster is non-production per Constitution Article I.5.
- A NetworkPolicy SHOULD restrict ingress to the controller's pod selector if ergonomic.

Restoration:
1. Re-add `AGENT_SHARED_TOKEN` to `nodemedic_agent/config.py` `Settings` (currently removed in NFR-6).
2. Add a FastAPI dependency that validates `Authorization: Bearer <token>` against the configured token; reject with 401 on mismatch.
3. Add the `nodemedic-agent-token` Secret back to the chart, mount as env var, source from Vault.
4. Update the controller's `nodemedic-agent-token` Secret (already in its chart) so both sides see the same value.

Production restoration aligns with Spec 001's existing 401 handling (controller already has the unauthorized failure path wired) — the change is one-sided.
