"""Case worker (T036 stub + T057 real loop).

The stub variant (``run_case_stub``) is the Day-1 AM deliverable per
Constitution Article II.3 — sleeps 30 s, writes a hand-canned
``status.diagnosis`` so the controller can flip off ``--stub-agent=true``
and exercise its FR-6 confidence gate + FR-8 cordon path against a real CR.

The real variant (``run_case``) replaces the stub in T057; it owns the
Claude Agent SDK loop. Until then it routes to the stub when
``Settings.stub_agent`` is True.
"""
from __future__ import annotations

import asyncio
from datetime import datetime, timezone
from typing import Any

from nodemedic_agent.kube.nhd_writer import NHDWriter, WriteOutcome
from nodemedic_agent.logging import bind_case_id, get_logger
from nodemedic_agent.runner.case_table import Case, CaseTable

log = get_logger(__name__)

STUB_SLEEP_SEC = 30.0


def _now_rfc3339() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


async def run_case_stub(case: Case, state: Any) -> None:
    """Day-1 AM stub diagnosis (research R-11).

    Sleeps 30 s, writes a hand-canned `status.diagnosis` with confidence
    0.85, two evidence sources, recommendation Cordon. Lets the controller
    exercise its gate-pass path against a real CR shape before the real
    SDK loop is wired (T057).
    """
    case_table: CaseTable = state.case_table
    writer: NHDWriter = state.nhd_writer

    with bind_case_id(case.case_id):
        log.info(
            "case_started",
            mode="stub",
            node_name=case.node_name,
            cluster_name=case.cluster_name,
            provider=case.provider,
        )
        # Stretch the sleep slightly so the controller's deadline path
        # exercises in-window; tests override STUB_SLEEP_SEC via patching.
        await asyncio.sleep(STUB_SLEEP_SEC)

        nhd_name = await _resolve_nhd_name(case, writer)
        if nhd_name is None:
            log.error("case_no_nhd_name", reason="caseId not found in namespace")
            await case_table.mark_complete(
                case.case_id, "Failed", failure_reason="CRWriteFailed"
            )
            return

        diagnosis = _stub_diagnosis(case, model_used=_resolve_model_id(state))
        result = await writer.update_status(
            nhd_name,
            status_diagnosis=diagnosis,
            final_phase="Diagnosed",
        )

        if result.outcome is WriteOutcome.WRITTEN:
            await case_table.mark_complete(case.case_id, "Diagnosed")
            log.info(
                "case_complete",
                write_outcome="written",
                final_phase="Diagnosed",
                duration_ms=int(STUB_SLEEP_SEC * 1000),
            )
        elif result.outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT:
            await case_table.mark_complete(case.case_id, "Diagnosed")
            log.info(
                "case_complete",
                write_outcome="deferred_phase_conflict",
                observed_phase=result.observed_phase,
            )
        else:
            await case_table.mark_complete(
                case.case_id, "Failed", failure_reason="CRWriteFailed"
            )
            log.error(
                "case_complete",
                write_outcome="write_failed",
                failure_reason="CRWriteFailed",
                detail=result.detail,
            )


async def run_case(case: Case, state: Any) -> None:
    """Real SDK-loop worker (T057). Phase 3 routes to the stub.

    T058 flips this dispatch when ``state.settings.stub_agent`` is False
    once the real loop is wired in T057.
    """
    if state.settings.stub_agent:
        await run_case_stub(case, state)
        return
    raise NotImplementedError(
        "real run_case lands in T057 (US2 — Phase 4)"
    )


def _resolve_model_id(state: Any) -> str:
    resolver = state.model_resolver
    cached = getattr(resolver, "cached", None)
    if cached is not None:
        return cached.primary_resolved
    return state.settings.claude_model


def _stub_diagnosis(case: Case, model_used: str) -> dict[str, Any]:
    now = _now_rfc3339()
    return {
        "rootCause": "Day-1 stub diagnosis for integration testing",
        "rcaCategory": "Unknown",
        "confidence": 0.85,
        "evidence": [
            {
                "source": "kubectl",
                "ref": f"kubectl get node {case.node_name} -o yaml",
                "result": "stub-evidence: condition observed",
                "observedAt": now,
            },
            {
                "source": "nrql",
                "ref": (
                    "SELECT count(*) FROM K8sNodeSample "
                    f"WHERE clusterName = '{case.cluster_name}' "
                    f"AND nodeName = '{case.node_name}' SINCE 15 minutes ago"
                ),
                "result": "stub-evidence: 1 sample",
                "observedAt": now,
            },
        ],
        "recommendation": {
            "action": "Cordon",
            "reason": "Day-1 stub diagnosis — controller may proceed with confidence gate",
        },
        "modelUsed": model_used,
        "completedAt": now,
    }


async def _resolve_nhd_name(case: Case, writer: NHDWriter) -> str | None:
    if case.nhd_name:
        return case.nhd_name
    return await writer.find_by_case_id(case.case_id)
