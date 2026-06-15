"""Terminal `emit_report` SDK tool (T053).

Registered as an in-process MCP tool via the Claude Agent SDK's
``@tool`` + ``create_sdk_mcp_server`` API. The model invokes it once per
case as the final tool call; the runner intercepts the call before any
network egress, validates the args, writes the NHD CR's
``status.diagnosis``, emits the case-complete log line, and ends the
loop naturally.

Validation order (FR-8 step 1):

1. **SDK-level**: the JSON Schema in ``EMIT_REPORT_INPUT_SCHEMA`` gates
   the call before this function runs.
2. **Pydantic-level**: ``EmitReportPayload.model_validate(args)`` covers
   the semantic constraints (``confidence ∈ [0, 1]``, non-empty
   ``evidence``, blank-rejection on ``rootCause`` and
   ``recommendation.reason``).
3. **Runner-level**: ``truncate_evidence_results(args, limit=4096)``
   trims oversized result strings before validation. Truncation is
   logged at WARN with the case_id and entry index.
4. **CR pre-write**: the writer's ``update_status`` does the FR-12
   phase-conflict re-read; deferred phases return cleanly.
5. **CR write**: ``replace_namespaced_custom_object_status`` per FR-8
   retry policy. ``WriteOutcome`` drives the structured envelope this
   tool returns to the model.

The tool function is built per case via ``make_emit_report(case,
nhd_writer)`` — closes over the per-case ``Case`` so every loop has its
own bound writer + caseId. Each ``ClaudeSDKClient`` instance gets its
own ``create_sdk_mcp_server`` (FR-3 fresh-session-per-case).
"""
from __future__ import annotations

import copy
from datetime import datetime, timezone
from typing import Any, Awaitable, Callable, Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from claude_agent_sdk import SdkMcpTool, create_sdk_mcp_server, tool

from nodemedic_agent.kube.nhd_writer import NHDWriter, WriteOutcome
from nodemedic_agent.logging import get_logger
from nodemedic_agent.runner.case_table import Case

log = get_logger(__name__)

# Hard cap on evidence[].result length per FR-8 / contract.
EVIDENCE_RESULT_LIMIT = 4096


# ---------------------------------------------------------------------------
# JSON Schema for the SDK tool decorator (mirrors contracts/emit-report-tool.md)
# ---------------------------------------------------------------------------


EMIT_REPORT_INPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["rootCause", "rcaCategory", "confidence", "evidence", "recommendation"],
    "additionalProperties": False,
    "properties": {
        "rootCause": {
            "type": "string",
            "minLength": 1,
            "maxLength": 4096,
            "description": (
                "One-paragraph plain-text RCA describing why the node is "
                "unhealthy. Be concrete; cite what evidence supports the "
                "conclusion."
            ),
        },
        "rcaCategory": {
            "type": "string",
            "enum": [
                "Conntrack",
                "FD",
                "PID",
                "Inode",
                "Disk",
                "DNS",
                "IMDS",
                "Kernel",
                "Kubelet",
                "Unknown",
            ],
            "description": (
                "Failure family. Use Unknown only when evidence is too thin "
                "to classify."
            ),
        },
        "confidence": {
            "type": "number",
            "minimum": 0.0,
            "maximum": 1.0,
            "description": (
                "How confident you are in the diagnosis. Lower below 0.7 "
                "when evidence is single-source or contradictory."
            ),
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
                        "enum": ["nrql", "ssh", "kubectl", "cloud", "proc", "log"],
                    },
                    "ref": {
                        "type": "string",
                        "description": (
                            "The query, command, or path that produced this "
                            "evidence."
                        ),
                    },
                    "result": {
                        "type": "string",
                        "maxLength": EVIDENCE_RESULT_LIMIT,
                        "description": (
                            "Raw result. Truncated to 4 KB by the runner if "
                            "longer."
                        ),
                    },
                    "observedAt": {
                        "type": "string",
                        "format": "date-time",
                    },
                },
            },
            "description": (
                "Citations supporting the diagnosis. Single-entry is "
                "permitted; lower confidence accordingly."
            ),
        },
        "recommendation": {
            "type": "object",
            "required": ["action", "reason"],
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["Cordon", "DrainAndCordon", "NoAction"],
                    "description": (
                        "Cordon = stop scheduling new pods. DrainAndCordon "
                        "= same (controller honors as Cordon — agent has "
                        "no drain credential). NoAction = explicitly "
                        "recommend no automated action; controller routes "
                        "to HumanInLoop."
                    ),
                },
                "reason": {
                    "type": "string",
                    "minLength": 1,
                    "description": (
                        "Short justification (1–2 sentences). Goes to the "
                        "Slack message."
                    ),
                },
            },
        },
    },
}


# ---------------------------------------------------------------------------
# Pydantic models (semantic-level validation)
# ---------------------------------------------------------------------------


EvidenceSource = Literal["nrql", "ssh", "kubectl", "cloud", "proc", "log"]
RCACategory = Literal[
    "Conntrack",
    "FD",
    "PID",
    "Inode",
    "Disk",
    "DNS",
    "IMDS",
    "Kernel",
    "Kubelet",
    "Unknown",
]
RecommendationAction = Literal["Cordon", "DrainAndCordon", "NoAction"]


class EmitReportEvidence(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    source: EvidenceSource
    ref: str = ""
    result: str = Field(default="", max_length=EVIDENCE_RESULT_LIMIT)
    observed_at: datetime = Field(alias="observedAt")


class EmitReportRecommendation(BaseModel):
    action: RecommendationAction
    reason: str = Field(min_length=1)


class EmitReportPayload(BaseModel):
    """Validated `emit_report` arguments.

    Mirrors data-model.md §4 + contracts/emit-report-tool.md verbatim.
    Field aliases preserve the camelCase wire shape from the SDK while
    the Python attributes use snake_case.
    """

    model_config = ConfigDict(populate_by_name=True)

    root_cause: str = Field(alias="rootCause", min_length=1, max_length=4096)
    rca_category: RCACategory = Field(alias="rcaCategory")
    confidence: float = Field(ge=0.0, le=1.0)
    evidence: list[EmitReportEvidence] = Field(min_length=1)
    recommendation: EmitReportRecommendation


# ---------------------------------------------------------------------------
# Runner-level helpers
# ---------------------------------------------------------------------------


def truncate_evidence_results(
    args: dict[str, Any],
    limit: int = EVIDENCE_RESULT_LIMIT,
) -> tuple[dict[str, Any], int]:
    """Lenient pre-validation truncation of ``evidence[].result``.

    Returns ``(new_args, truncation_count)``. The original ``args`` dict
    is not mutated; a deep copy is returned. Strings ≤ ``limit`` are
    untouched.
    """
    new_args = copy.deepcopy(args)
    evidence = new_args.get("evidence")
    if not isinstance(evidence, list):
        return new_args, 0
    hits = 0
    for entry in evidence:
        if not isinstance(entry, dict):
            continue
        result = entry.get("result")
        if isinstance(result, str) and len(result) > limit:
            entry["result"] = result[:limit]
            hits += 1
    return new_args, hits


def _now_rfc3339() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def build_diagnosis_payload(
    payload: EmitReportPayload,
    *,
    model_used: str,
    completed_at: str | None = None,
    turns_used: int | None = None,
    cost_usd: str | None = None,
) -> dict[str, Any]:
    """Map the validated tool payload to the NHD ``status.diagnosis`` shape.

    Field names match the CRD schema in contracts/nhd-crd.yaml and the
    Go struct in Spec 001's ``api/v1alpha1/nodehealthdiagnosisai_types.go``
    (camelCase, byte-stable).
    """
    diagnosis: dict[str, Any] = {
        "rootCause": payload.root_cause,
        "rcaCategory": payload.rca_category,
        "confidence": payload.confidence,
        "evidence": [
            {
                "source": e.source,
                "ref": e.ref,
                "result": e.result,
                "observedAt": e.observed_at.isoformat().replace("+00:00", "Z"),
            }
            for e in payload.evidence
        ],
        "recommendation": {
            "action": payload.recommendation.action,
            "reason": payload.recommendation.reason,
        },
        "modelUsed": model_used,
        "completedAt": completed_at or _now_rfc3339(),
    }
    if turns_used is not None:
        diagnosis["turnsUsed"] = turns_used
    if cost_usd is not None:
        diagnosis["costUSD"] = cost_usd
    return diagnosis


# ---------------------------------------------------------------------------
# Tool factory
# ---------------------------------------------------------------------------


# A factory returns the SDK tool object so each case can have its own
# bound writer + caseId without leaking state between cases.
EmitReportFactory = Callable[..., SdkMcpTool[Any]]


def make_emit_report(
    case: Case,
    nhd_writer: NHDWriter,
    *,
    model_used: str,
    nhd_name: str | None = None,
    outcome_sink: dict[str, Any] | None = None,
) -> SdkMcpTool[Any]:
    """Build the per-case `emit_report` tool.

    Args:
        case: the per-case Case object (carries caseId and metadata).
        nhd_writer: NHDWriter the tool will dispatch to.
        model_used: resolved model id (FR-3) — written verbatim into
            ``status.diagnosis.modelUsed``.
        nhd_name: ``metadata.name`` of the target NHD CR. Resolved from
            ``case.nhd_name`` (controller-supplied) or via the writer's
            list+filter fallback before the tool is constructed.
        outcome_sink: optional mutable dict the tool stamps with the
            terminal `WriteOutcome`, ``observed_phase``, and ``detail``
            after the call completes. The runner uses this to map to
            FR-11 ``case_complete`` shape without re-parsing the model
            stream. ``emitted`` is also set to True. The sink is only
            written on successful tool entry; runner-side timeouts /
            exceptions are surfaced via the runner's own state.
    """
    # Per-case mutable state for "already emitted" detection. Always a
    # local dict regardless of whether outcome_sink is provided so we can
    # detect duplicate calls.
    state: dict[str, Any] = {"emitted": False}

    @tool(
        name="emit_report",
        description=(
            "Emit the final diagnosis for this case and end the loop. "
            "Call exactly once per case as the FINAL tool call. Do not "
            "call any other tool after emit_report."
        ),
        input_schema=EMIT_REPORT_INPUT_SCHEMA,
    )
    async def emit_report(args: dict[str, Any]) -> dict[str, Any]:
        if state["emitted"]:
            return _error_envelope(
                "AlreadyEmitted",
                "emit_report has already been called for this case; "
                "the runner refuses duplicate calls.",
            )

        # 1. Lenient truncation (post-SDK schema, pre-Pydantic).
        truncated_args, hits = truncate_evidence_results(args)
        if hits > 0:
            log.warning(
                "emit_report_result_truncated",
                case_id=case.case_id,
                truncated_entries=hits,
                limit=EVIDENCE_RESULT_LIMIT,
            )

        # 2. Pydantic-level validation. SDK schema already gates most of
        #    this, but Pydantic catches the blank-rejection on
        #    `recommendation.reason` and the like.
        try:
            payload = EmitReportPayload.model_validate(truncated_args)
        except ValidationError as exc:
            log.error(
                "emit_report_validation_failed",
                case_id=case.case_id,
                detail=str(exc)[:512],
            )
            return _error_envelope(
                "ValidationFailed",
                f"emit_report payload failed validation: {exc.errors()[:3]}",
            )

        # 3. Build status.diagnosis.
        diagnosis = build_diagnosis_payload(payload, model_used=model_used)

        # 4. Resolve target NHD name.
        target_name = nhd_name or case.nhd_name
        if not target_name:
            target_name = await nhd_writer.find_by_case_id(case.case_id)
        if not target_name:
            log.error(
                "emit_report_no_nhd_name",
                case_id=case.case_id,
                reason="caseId not found in namespace",
            )
            return _error_envelope(
                "CRWriteFailed",
                f"NHD with caseId={case.case_id} not found in namespace "
                f"{nhd_writer.namespace}",
            )

        # 5. Write status.diagnosis (FR-8 + FR-12 in NHDWriter).
        result = await nhd_writer.update_status(
            target_name,
            status_diagnosis=diagnosis,
            final_phase="Diagnosed",
        )

        # 6. Mark emitted on every terminal outcome — even deferred and
        #    failed writes count as the case's single emit_report call.
        state["emitted"] = True
        if outcome_sink is not None:
            outcome_sink["emitted"] = True
            outcome_sink["outcome"] = result.outcome
            outcome_sink["observed_phase"] = result.observed_phase
            outcome_sink["detail"] = result.detail

        if result.outcome is WriteOutcome.WRITTEN:
            return _ok_envelope(case_pid=case.case_id)
        if result.outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT:
            return _deferred_envelope(observed_phase=result.observed_phase or "")
        # WriteOutcome.WRITE_FAILED
        return _error_envelope(
            "CRWriteFailed",
            result.detail or "apiserver write failed",
        )

    return emit_report


# ---------------------------------------------------------------------------
# Per-case MCP server
# ---------------------------------------------------------------------------


def make_emit_report_server(
    case: Case,
    nhd_writer: NHDWriter,
    *,
    model_used: str,
    nhd_name: str | None = None,
):
    """Build the per-case in-process MCP server hosting `emit_report`."""
    return create_sdk_mcp_server(
        name="nodemedic",
        version="1.0.0",
        tools=[
            make_emit_report(
                case,
                nhd_writer,
                model_used=model_used,
                nhd_name=nhd_name,
            )
        ],
    )


# ---------------------------------------------------------------------------
# Tool return-value helpers (contracts/emit-report-tool.md §Tool return value)
# ---------------------------------------------------------------------------


def _content_envelope(payload: dict[str, Any], *, is_error: bool = False) -> dict[str, Any]:
    """Wrap the diagnostic envelope in the SDK's ``content`` shape.

    The SDK's `@tool` handlers return a dict with a ``content`` list of
    blocks; the model sees the JSON-stringified payload. We additionally
    surface the structured fields at the top level for runner-side
    inspection (the SDK ignores extra keys on the dict).
    """
    import json

    result: dict[str, Any] = {
        "content": [{"type": "text", "text": json.dumps(payload)}],
    }
    if is_error:
        result["is_error"] = True
    # Mirror the structured payload so the runner can introspect the
    # outcome without re-parsing the content text.
    result.update(payload)
    return result


def _ok_envelope(case_pid: str) -> dict[str, Any]:
    return _content_envelope(
        {
            "ok": True,
            "phase": "Diagnosed",
            "casePid": case_pid,
            "writtenAt": _now_rfc3339(),
        }
    )


def _deferred_envelope(observed_phase: str) -> dict[str, Any]:
    return _content_envelope(
        {
            "ok": False,
            "deferred": True,
            "observedPhase": observed_phase,
            "reason": (
                "controller already marked CR "
                f"{observed_phase} before agent finished — agent's "
                "diagnosis is in the structured logs"
            ),
        }
    )


def _error_envelope(error: str, detail: str) -> dict[str, Any]:
    return _content_envelope(
        {
            "ok": False,
            "error": error,
            "detail": detail[:512],
        },
        is_error=True,
    )
