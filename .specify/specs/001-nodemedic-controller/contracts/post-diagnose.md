# Contract — `POST /diagnose`

**Authority**: `nodemedic-scope.md` §3 (cross-scope contract). This file is the controller-side reference + frozen golden bodies for golden-file testing.

**Direction**: controller → agent. **Auth**: bearer token from `Secret/nodemedic-agent-token`. **Default URL**: `http://nodemedic-agent.cf-monitoring.svc:8080/diagnose` (overridable via `--agent-url`).

---

## Request

```http
POST /diagnose HTTP/1.1
Host: nodemedic-agent.cf-monitoring.svc:8080
Authorization: Bearer <token>
Content-Type: application/json
Content-Length: <n>
```

### JSON body schema

| Field | Type | Required | Notes |
|---|---|---|---|
| `caseId` | string (UUIDv4) | yes | Mirrors `nhd.spec.case.caseId` |
| `nodeName` | string | yes | apiserver Node name |
| `clusterName` | string | yes | from `--cluster-name`; starts with `test-` |
| `provider` | enum `aws \| azure` | yes | resolved per FR-2 |
| `region` | string | yes | e.g. `us-east-2` or `eastus2` |
| `instanceId` | string | yes | AWS instance-id or Azure VM name |
| `trigger.type` | string | yes | NodeCondition type, e.g. `ConntrackSaturated` |
| `trigger.reason` | string | yes | NPD rule reason |
| `trigger.message` | string (≤256B) | yes | NPD rule message, may be truncated |
| `trigger.observedAt` | string (RFC3339) | yes | when NPD flipped Condition to True |
| `budgets.maxTurns` | int | yes | default `15` |
| `budgets.maxBudgetUSD` | string (decimal) | yes | default `"0.50"` |
| `budgets.deadlineSec` | int | yes | seconds; controller computes from `nhd.spec.budgets.deadline` |

### Golden request body (used in golden-file test `internal/agentclient/client_test.go`)

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

---

## Response — async

```http
HTTP/1.1 202 Accepted
Content-Type: application/json

{ "caseId": "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c", "status": "queued" }
```

`status ∈ {queued, completed}`. The controller does not branch on this — both 202 outcomes flip the NHD to `Diagnosing` and rely on the informer to learn about completion via the CR's `status.diagnosis` field (research R-5).

---

## Errors and controller behavior

| HTTP code | Meaning | Controller behavior (FR-4 + FR-7) |
|---|---|---|
| 202 | Queued or already in-flight | Set `phase=Diagnosing`, `Condition[AgentInvoked]=True{reason=Posted}` |
| 400 | Missing required field / malformed | Set `Failed{reason=BadRequest}`. NO retry. |
| 401 | Bad/missing token | Set `Failed{reason=Unauthorized}`. Page Slack with `severity=critical`. NO retry. |
| 429 | Concurrency cap | Exponential backoff `1s/2s/4s`, max 3 attempts, then `Failed{reason=AgentUnreachable}` + FR-7 retry once. |
| 5xx | Agent crash | Same as 429. |
| network/timeout | Agent unreachable | Same as 429. |

Per-attempt timeout: 1.5 s. Total wall-clock for the retry chain is bounded by `min(spec.budgets.deadline, max-retries-budget=10s)`.

---

## Idempotency contract (research R-5)

The controller may issue `POST /diagnose` more than once with the same `caseId`. The agent MUST:
- Treat a repeat `caseId` as idempotent.
- Return `202 { "status": "queued" }` if the case is in-flight.
- Return `202 { "status": "completed" }` if the case is already written to the CR.
- (Tolerated, not preferred) Return `409` to indicate "already in-flight" — the controller treats this identically to 202.

This is the only retry surface in the agent contract. Scope 3's open question §6.6 confirms this is the intended behavior.

---

## What the controller **does not** do

- Does NOT POST `status` updates to the agent. The agent learns nothing from the controller after the initial POST.
- Does NOT read the response body's `status` field as authoritative — the NHD CR is.
- Does NOT include any auth token besides the bearer.
- Does NOT include any custom headers besides `Authorization` and `Content-Type`.
- Does NOT include the NHD's `metadata` or `status` in the request — only the fields above.

This minimalism is deliberate: every additional field is a place where Scope 2 and Scope 3 can drift.
