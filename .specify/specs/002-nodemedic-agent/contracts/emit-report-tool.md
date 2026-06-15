# Contract — `emit_report` terminal tool

**Authority**: Spec 002 owns this contract — it is purely internal to the agent process. The controller never sees `emit_report` arguments directly; it sees the resulting `status.diagnosis` on the NHD CR. Schema changes here ripple to the controller's confidence-gate inputs (Spec 001 FR-6); coordinate any change with Spec 001 in the same PR.

**Scope**: `emit_report` is registered as an in-process MCP tool via the Claude Agent SDK's `@tool` + `create_sdk_mcp_server` API. The model invokes it once per successful case as the final tool call; the runner intercepts the call before any network egress, validates the args, writes the NHD CR's `status.diagnosis`, emits the case-complete log line, and ends the loop.

The full rationale for why `emit_report` exists (and why simpler alternatives — last-message JSON parse, structured-output, agent-side `kubectl patch` — were rejected) lives in spec FR-8. This file is the schema reference.

---

## Tool registration

```python
from claude_agent_sdk import tool, create_sdk_mcp_server

# Closes over per-case Case object; runner instantiates one of these per case.
def make_emit_report(case: Case, nhd_writer: NHDWriter) -> ...:
    @tool(
        name="emit_report",
        description=(
            "Emit the final diagnosis for this case and end the loop. "
            "Call exactly once per case as the FINAL tool call. "
            "Do not call any other tool after emit_report."
        ),
        input_schema=EMIT_REPORT_INPUT_SCHEMA,
    )
    async def emit_report(args: dict) -> dict:
        # Pydantic-validate args against EmitReportPayload.
        # On success: build status.diagnosis, write CR via nhd_writer, end loop.
        # On schema failure: return {"error": "...", "isError": True}.
        ...

    return emit_report

# Per case:
mcp_server = create_sdk_mcp_server(name="nodemedic", tools=[make_emit_report(case, writer)])
options = ClaudeAgentOptions(
    mcp_servers={"nodemedic": mcp_server, "nr": NR_MCP_CONFIG},
    allowed_tools=["mcp__nodemedic__emit_report", "mcp__nr__*", "Bash", "Read", "Write", ...],
    ...
)
```

The SDK exposes the tool to the model under the canonical `mcp__nodemedic__emit_report` name. The runbook prompt instructs the agent to call it by that name.

---

## Input schema (JSON Schema, fed to `@tool` decorator)

```json
{
  "type": "object",
  "required": ["rootCause", "rcaCategory", "confidence", "evidence", "recommendation"],
  "properties": {
    "rootCause": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4096,
      "description": "One-paragraph plain-text RCA describing why the node is unhealthy. Be concrete; cite what evidence supports the conclusion."
    },
    "rcaCategory": {
      "type": "string",
      "enum": ["Conntrack", "FD", "PID", "Inode", "Disk", "DNS", "IMDS", "Kernel", "Kubelet", "Unknown"],
      "description": "Failure family. Use Unknown only when evidence is too thin to classify."
    },
    "confidence": {
      "type": "number",
      "minimum": 0.0,
      "maximum": 1.0,
      "description": "How confident you are in the diagnosis. Lower below 0.7 when evidence is single-source or contradictory."
    },
    "evidence": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["source", "observedAt"],
        "properties": {
          "source": {
            "type": "string",
            "enum": ["nrql", "ssh", "kubectl", "cloud", "proc", "log"]
          },
          "ref": {
            "type": "string",
            "description": "The query, command, or path that produced this evidence."
          },
          "result": {
            "type": "string",
            "maxLength": 4096,
            "description": "Raw result. Truncated to 4 KB by the runner if longer."
          },
          "observedAt": {
            "type": "string",
            "format": "date-time"
          }
        }
      },
      "description": "Citations supporting the diagnosis. Single-entry is permitted; lower confidence accordingly."
    },
    "recommendation": {
      "type": "object",
      "required": ["action", "reason"],
      "properties": {
        "action": {
          "type": "string",
          "enum": ["Cordon", "DrainAndCordon", "NoAction"],
          "description": "Cordon = stop scheduling new pods. DrainAndCordon = same (controller honors as Cordon — agent has no drain credential). NoAction = explicitly recommend no automated action; controller routes to HumanInLoop."
        },
        "reason": {
          "type": "string",
          "minLength": 1,
          "description": "Short justification (1–2 sentences). Goes to the Slack message."
        }
      }
    }
  },
  "additionalProperties": false
}
```

**Rationale for `additionalProperties: false`**: prevents the model from smuggling unstructured fields the controller's gate would silently ignore. If the schema needs to grow, bump it explicitly in a coordinated PR with Spec 001.

---

## Validation order (FR-8 step 1)

1. **SDK-level**: the Claude Agent SDK validates against the JSON Schema above before invoking the tool function. Schema violations surface to the model as a tool error and the model can retry the call with corrected args (one-shot retry; persistent invalidity ends the loop with `Failed{reason=ToolError}`).
2. **Pydantic-level**: the tool function runs `EmitReportPayload.model_validate(args)`. Catches semantic constraints not expressible cleanly in JSON Schema (e.g., reason text being non-blank).
3. **Runner-level**: result strings on each `evidence[]` entry are truncated to 4 KB if longer (lenient — log a WARN with `case_id` + which entry was truncated). The truncation is post-validation so the model sees its original args echoed back.
4. **CR pre-write**: runner re-reads the NHD CR and consults FR-12's phase-conflict matrix (data-model.md §1.3). If `status.phase ∈ {Diagnosed, Acted, Failed}` → defer write, log INFO, return `{"deferred": true, "observedPhase": "<phase>"}` to the model so the loop ends cleanly.
5. **CR write**: build `status.diagnosis` payload, set `status.phase = "Diagnosed"`, set `status.conditions[ReportReady] = True{reason=EvidenceValid}`, call `replace_namespaced_custom_object_status`. Apply FR-8 retry policy (data-model.md §8 / R-13).

---

## Tool return value

The model sees a structured return from the tool:

```json
{
  "ok": true,
  "phase": "Diagnosed",
  "casePid": "<runner internal id>",
  "writtenAt": "<RFC3339>"
}
```

On deferred write (FR-12 phase conflict):

```json
{
  "ok": false,
  "deferred": true,
  "observedPhase": "Failed",
  "reason": "controller already marked CR Failed before agent finished — agent's diagnosis is in the structured logs"
}
```

On terminal write failure (R-13 / FR-8 RBAC denied or schema invalid):

```json
{
  "ok": false,
  "error": "CRWriteFailed",
  "detail": "<apiserver error message, ≤512B>"
}
```

The runbook prompt instructs the agent: *"Whatever `emit_report` returns, do NOT call any other tool. The case is over."* The model halts naturally after the tool call returns.

---

## Golden payload (used in `tests/nodemedic_agent/fixtures/emit_report_payload.json`)

This is the cf1z `ContainerRuntimeUnhealthy` happy-path golden. Three distinct evidence sources, confidence 0.85, recommendation Cordon — meets the controller's gate.

```json
{
  "rootCause": "Containerd socket /run/containerd/containerd.sock is unreachable inside the hack-node-problem-detector pod's mount view, while the host socket is intact. NPD's check-containerd.sh probe correctly reports ContainerRuntimeUnhealthy. The cause is a shadow bind-mount over the in-pod socket path; node-level container runtime is healthy. Recommend cordon to prevent new pod scheduling until the shadow is cleared.",
  "rcaCategory": "Kernel",
  "confidence": 0.85,
  "evidence": [
    {
      "source": "kubectl",
      "ref": "kubectl --context=cf1z get node cf1z-general-nodes-2000002 -o yaml",
      "result": "{conditions: [..., {type: ContainerRuntimeUnhealthy, status: True, reason: ContainerdUnreachable, lastTransitionTime: 2026-06-13T14:00:23Z}], ...}",
      "observedAt": "2026-06-13T14:00:30Z"
    },
    {
      "source": "ssh",
      "ref": "ssh -i $SSH_KEY_PATH ubuntu@10.0.0.42 'ls -la /run/containerd/containerd.sock; pgrep -fa containerd'",
      "result": "srw-rw---- 1 root root 0 Jun 13 13:55 /run/containerd/containerd.sock\n11234 /usr/bin/containerd\n",
      "observedAt": "2026-06-13T14:00:35Z"
    },
    {
      "source": "nrql",
      "ref": "SELECT * FROM K8sNodeSample WHERE clusterName='cf1z' AND nodeName='cf1z-general-nodes-2000002' SINCE 15 minutes ago",
      "result": "{cpuUsedCores: 0.42, memoryUsedBytes: 8.1e9, ...}",
      "observedAt": "2026-06-13T14:00:40Z"
    }
  ],
  "recommendation": {
    "action": "Cordon",
    "reason": "Containerd unreachable inside NPD pod view; new pods would also fail to schedule onto this node. Cordon until shadow mount cleared."
  }
}
```

A **gate-fail golden** at `tests/nodemedic_agent/fixtures/emit_report_payload_thin.json` exercises AC-6:

```json
{
  "rootCause": "Could not gather sufficient evidence — NR MCP returned errors and SSH timed out. Recommend operator review.",
  "rcaCategory": "Unknown",
  "confidence": 0.4,
  "evidence": [
    {
      "source": "kubectl",
      "ref": "kubectl get node cf1z-general-nodes-2000002 -o yaml",
      "result": "(condition observed but no further evidence)",
      "observedAt": "2026-06-13T14:00:30Z"
    }
  ],
  "recommendation": {
    "action": "NoAction",
    "reason": "Insufficient evidence for automated cordon decision"
  }
}
```

This payload is **schema-valid** — agent accepts it, writes the CR. Controller's confidence gate then routes to `HumanInLoop` (confidence 0.4 < 0.7 AND only 1 distinct source). Tested by `tests/nodemedic_agent/unit/test_emit_report_validate.py::test_thin_evidence_accepted`.

---

## Forbidden patterns (caught by code review, not schema)

The runbook prompt MUST forbid these and structured-log lines (NFR-3) MUST surface them if attempted:

- Calling any tool *after* `emit_report` — the runner cannot reliably end the loop if the model continues.
- Reading `$SSH_KEY_PATH`, `/etc/nodemedic/ssh/*`, `/var/run/secrets/**`, or any other Secret-mounted credential path via `Read` (AC-15 clause 4).
- Calling `emit_report` more than once per case (the runner will refuse the second call with `{"error": "AlreadyEmitted"}`).
- Bypassing `emit_report` and writing the NHD via `Bash kubectl patch` (would lose schema validation, FR-12 phase guard, single-ordered finalize — see spec FR-8 alternatives section).

---

## Restoration notes

`emit_report` stays even after the constitution's hackathon-scope simplifications are reverted for production. It is a load-bearing pattern, not a hackathon shortcut. Production hardening adds:
- Persisting the audit JSONL via the runner's tool log (constitution "Production hardening" item 7).
- Re-introducing per-turn cost / token usage tracking for the `costUSD` and `turnsUsed` fields (currently best-effort under FR-7).
- Restoring the agent-side wall-clock deadline that cancels the loop and writes a partial CR (constitution item 6) — this would have its own FR-11 reason `DeadlineExceeded` and `emit_report` would cease to be the sole termination path.
