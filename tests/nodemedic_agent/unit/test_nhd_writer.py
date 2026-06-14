"""Unit tests for `NHDWriter` (T017).

Covers each FR-8 retry case:
  - 200 happy path (Diagnosed and Failed)
  - 404 NotFound × 3 then success
  - 404 NotFound × 4 → exhaustion → WRITE_FAILED
  - 409 Conflict on status → re-read + 1s × 1 → success
  - 5xx then success
  - 403 RBAC denied → terminal
  - 422 schema invalid → terminal
  - FR-12: terminal phase observed pre-write → DEFERRED_PHASE_CONFLICT

Uses injected `getter` / `replacer` callables instead of patching the kube
client — `respx` would only help for raw HTTP, but the writer's contract
lives at the API-call boundary.
"""
from __future__ import annotations

import asyncio
import time
from typing import Any, Awaitable, Callable

import pytest
from kubernetes.client.exceptions import ApiException

from nodemedic_agent.kube.nhd_writer import (
    NHDWriter,
    WriteOutcome,
)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

NAMESPACE = "cf-monitoring"
NAME = "cf1z-general-nodes-2000002-1718380800"


def _existing_cr(phase: str = "Diagnosing") -> dict[str, Any]:
    return {
        "apiVersion": "nodemedic.cf.newrelic.com/v1alpha1",
        "kind": "NodeHealthDiagnosisAI",
        "metadata": {
            "name": NAME,
            "namespace": NAMESPACE,
            "resourceVersion": "12345",
        },
        "spec": {
            "case": {
                "caseId": "1a2b3c4d-5e6f-4789-9abc-def012345678",
                "nodeName": "cf1z-general-nodes-2000002",
            },
        },
        "status": {
            "phase": phase,
            "conditions": [
                {
                    "type": "AgentInvoked",
                    "status": "True",
                    "reason": "DiagnosisStarted",
                    "lastTransitionTime": "2026-06-13T14:00:00Z",
                },
            ],
        },
    }


def _diagnosis() -> dict[str, Any]:
    return {
        "rootCause": "containerd unreachable",
        "rcaCategory": "Kernel",
        "confidence": 0.85,
        "evidence": [
            {"source": "kubectl", "ref": "kubectl get node", "result": "...", "observedAt": "2026-06-13T14:00:30Z"},
            {"source": "ssh", "ref": "ssh node ls /run/...", "result": "...", "observedAt": "2026-06-13T14:00:35Z"},
        ],
        "recommendation": {"action": "Cordon", "reason": "containerd unreachable"},
        "modelUsed": "claude-opus-4-7",
        "completedAt": "2026-06-13T14:01:00Z",
    }


def _make_writer(
    getter: Callable[[str, str], Awaitable[dict[str, Any]]],
    replacer: Callable[[str, str, dict[str, Any]], Awaitable[dict[str, Any]]],
    notfound_backoff: tuple[float, ...] = (0.001, 0.002, 0.003),
    other_backoff: float = 0.001,
) -> NHDWriter:
    """Construct a writer with tiny backoffs so tests run in milliseconds."""
    return NHDWriter(
        namespace=NAMESPACE,
        getter=getter,
        replacer=replacer,
        notfound_backoff=notfound_backoff,
        other_backoff=other_backoff,
    )


def _api_exception(status: int, reason: str = "") -> ApiException:
    exc = ApiException(status=status, reason=reason)
    return exc


# ---------------------------------------------------------------------------
# Happy paths
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_happy_path_diagnosed_writes_status() -> None:
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")

    assert result.outcome is WriteOutcome.WRITTEN
    assert len(written) == 1
    body = written[0]
    assert body["status"]["phase"] == "Diagnosed"
    assert body["status"]["diagnosis"]["confidence"] == 0.85
    [report_ready] = [c for c in body["status"]["conditions"] if c["type"] == "ReportReady"]
    assert report_ready["status"] == "True"
    assert report_ready["reason"] == "EvidenceValid"
    # AgentInvoked preserved.
    assert any(c["type"] == "AgentInvoked" for c in body["status"]["conditions"])


@pytest.mark.asyncio
async def test_happy_path_failed_writes_phase_failed() -> None:
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(
        NAME, {}, final_phase="Failed", failure_reason="ModelError"
    )

    assert result.outcome is WriteOutcome.WRITTEN
    body = written[0]
    assert body["status"]["phase"] == "Failed"
    assert "diagnosis" not in body["status"]  # no diagnosis on Failed
    [report_ready] = [c for c in body["status"]["conditions"] if c["type"] == "ReportReady"]
    assert report_ready["status"] == "False"
    assert report_ready["reason"] == "ModelError"


# ---------------------------------------------------------------------------
# 404 NotFound retry schedule (FR-8)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_notfound_then_success_after_three_retries() -> None:
    cr = _existing_cr()
    fail_count = {"n": 0}

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        if fail_count["n"] < 3:
            fail_count["n"] += 1
            raise _api_exception(404, "NotFound")
        return cr

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    writer = _make_writer(getter, replacer, notfound_backoff=(0.001, 0.001, 0.001))
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITTEN
    assert fail_count["n"] == 3


@pytest.mark.asyncio
async def test_notfound_exhausted_returns_write_failed() -> None:
    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        raise _api_exception(404, "NotFound")

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    writer = _make_writer(getter, replacer, notfound_backoff=(0.001, 0.001, 0.001))
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITE_FAILED
    assert result.failure_reason == "CRWriteFailed"


@pytest.mark.asyncio
async def test_notfound_backoff_schedule_timing() -> None:
    """Roughly verify the 250ms / 500ms / 1s schedule shape (R-13).

    Uses scaled-down backoffs (×0.01) so the test runs in ~17 ms but the
    relative ratios still match.
    """
    fail_count = {"n": 0}

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        if fail_count["n"] < 3:
            fail_count["n"] += 1
            raise _api_exception(404, "NotFound")
        return _existing_cr()

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    schedule = (0.0025, 0.005, 0.01)  # 250ms / 500ms / 1s × 0.01
    writer = _make_writer(getter, replacer, notfound_backoff=schedule)

    start = time.perf_counter()
    await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    elapsed = time.perf_counter() - start

    # Sum of schedule = 0.0175s; allow generous slack for asyncio scheduler.
    assert 0.015 < elapsed < 0.5


# ---------------------------------------------------------------------------
# 409 Conflict on status (FR-8)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_409_conflict_then_reread_success() -> None:
    cr = _existing_cr("Diagnosing")
    refreshed = _existing_cr("Diagnosing")
    refreshed["metadata"]["resourceVersion"] = "12346"

    get_calls = {"n": 0}

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        get_calls["n"] += 1
        return cr if get_calls["n"] == 1 else refreshed

    replace_attempts = {"n": 0}

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        replace_attempts["n"] += 1
        if replace_attempts["n"] == 1:
            raise _api_exception(409, "Conflict")
        return body

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITTEN
    assert replace_attempts["n"] == 2
    assert get_calls["n"] == 2  # initial pre-read + post-409 re-read


@pytest.mark.asyncio
async def test_409_then_terminal_phase_defers() -> None:
    """409 followed by a re-read showing Failed → DEFERRED_PHASE_CONFLICT.

    Models the late-write race for AC-7.
    """
    cr = _existing_cr("Diagnosing")
    refreshed = _existing_cr("Failed")
    get_calls = {"n": 0}

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        get_calls["n"] += 1
        return cr if get_calls["n"] == 1 else refreshed

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        raise _api_exception(409, "Conflict")

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT
    assert result.observed_phase == "Failed"


# ---------------------------------------------------------------------------
# 5xx / network — single retry
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_5xx_then_success() -> None:
    cr = _existing_cr()

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    attempts = {"n": 0}

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        attempts["n"] += 1
        if attempts["n"] == 1:
            raise _api_exception(503, "ServiceUnavailable")
        return body

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITTEN


@pytest.mark.asyncio
async def test_5xx_exhausted_after_one_retry() -> None:
    cr = _existing_cr()

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        raise _api_exception(503, "ServiceUnavailable")

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITE_FAILED


# ---------------------------------------------------------------------------
# Terminal classes (403 / 422)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("status_code,reason", [(403, "Forbidden"), (422, "Invalid")])
@pytest.mark.asyncio
async def test_terminal_classes_no_retry(status_code: int, reason: str) -> None:
    cr = _existing_cr()

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    attempts = {"n": 0}

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        attempts["n"] += 1
        raise _api_exception(status_code, reason)

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.WRITE_FAILED
    assert attempts["n"] == 1  # no retry


# ---------------------------------------------------------------------------
# FR-12 phase guard (pre-write defer)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("phase", ["Diagnosed", "Acted", "Failed", "Evaluating", "Evaluated"])
@pytest.mark.asyncio
async def test_terminal_phase_observed_pre_write_defers(phase: str) -> None:
    cr = _existing_cr(phase)
    replace_calls = {"n": 0}

    async def getter(_ns: str, _name: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _name: str, body: dict[str, Any]) -> dict[str, Any]:
        replace_calls["n"] += 1
        return body

    writer = _make_writer(getter, replacer)
    result = await writer.update_status(NAME, _diagnosis(), final_phase="Diagnosed")
    assert result.outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT
    assert result.observed_phase == phase
    assert replace_calls["n"] == 0  # write never attempted
