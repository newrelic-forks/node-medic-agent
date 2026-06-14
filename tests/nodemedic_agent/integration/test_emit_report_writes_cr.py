"""Integration tests for the `emit_report` tool's CR-write path (T054).

The tool factory in ``nodemedic_agent.tools.emit_report`` closes over a
per-case ``Case`` and an ``NHDWriter``. These tests inject an in-memory
getter / replacer pair into the writer so the test runs without a real
apiserver but exercises the full pipeline:

  Pydantic validation → result truncation → diagnosis payload build →
  writer pre-read (FR-12) → writer replace (FR-8 retry) → tool envelope.

Coverage:

  - happy-path golden → `WriteOutcome.WRITTEN`, returns `{ok: true, …}`
  - thin golden → still `WriteOutcome.WRITTEN`, single-evidence
    accepted (AC-6)
  - terminal pre-existing phase (`Failed`, `Acted`) →
    `WriteOutcome.DEFERRED_PHASE_CONFLICT`, returns `{deferred: true, …}`
  - 403 RBAC denied → tool returns `{ok: false, error: "CRWriteFailed"}`
  - oversized result → truncated to 4 KB pre-validation, write succeeds
  - duplicate emit_report call → second call returns `AlreadyEmitted`
  - missing nhd_name → falls back to `find_by_case_id`
  - find_by_case_id miss → `CRWriteFailed` envelope
"""
from __future__ import annotations

import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Awaitable, Callable

import pytest
from kubernetes.client.exceptions import ApiException

from nodemedic_agent.kube.nhd_writer import NHDWriter, WriteOutcome
from nodemedic_agent.runner.case_table import Case, CaseStatus
from nodemedic_agent.tools.emit_report import make_emit_report

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


CASE_ID = "1a2b3c4d-5e6f-4789-9abc-def012345678"
NHD_NAME = "cf1z-general-nodes-2000003-1781441053"
NAMESPACE = "cf-monitoring"


def _load(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES / name).read_text())


def _case() -> Case:
    return Case(
        case_id=CASE_ID,
        node_name="cf1z-general-nodes-2000003",
        cluster_name="cf1z",
        provider="azure",
        region="eastus2",
        instance_id="cf1z-general-nodes-2000003",
        nhd_name=NHD_NAME,
        trigger_type="ContainerRuntimeUnhealthy",
        trigger_reason="ContainerdUnreachable",
        trigger_message="containerd socket unreachable",
        trigger_observed_at=datetime(2026, 6, 13, 14, 0, 0, tzinfo=timezone.utc),
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        status=CaseStatus.RUNNING,
        started_at=datetime(2026, 6, 13, 14, 0, 5, tzinfo=timezone.utc),
    )


def _existing_cr(phase: str = "Diagnosing") -> dict[str, Any]:
    return {
        "apiVersion": "nodemedic.cf.newrelic.com/v1alpha1",
        "kind": "NodeHealthDiagnosisAI",
        "metadata": {
            "name": NHD_NAME,
            "namespace": NAMESPACE,
            "resourceVersion": "12345",
        },
        "spec": {
            "case": {"caseId": CASE_ID, "nodeName": "cf1z-general-nodes-2000003"},
        },
        "status": {
            "phase": phase,
            "conditions": [],
        },
    }


def _make_writer(
    getter: Callable[[str, str], Awaitable[dict[str, Any]]],
    replacer: Callable[[str, str, dict[str, Any]], Awaitable[dict[str, Any]]],
) -> NHDWriter:
    return NHDWriter(
        namespace=NAMESPACE,
        getter=getter,
        replacer=replacer,
        notfound_backoff=(0.001, 0.001, 0.001),
        other_backoff=0.001,
    )


# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_happy_golden_writes_cr_and_returns_ok_envelope() -> None:
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload.json"))

    assert result["ok"] is True
    assert result["phase"] == "Diagnosed"
    assert result["casePid"] == CASE_ID
    assert "writtenAt" in result
    assert len(written) == 1
    body = written[0]
    assert body["status"]["phase"] == "Diagnosed"
    assert body["status"]["diagnosis"]["confidence"] == 0.85
    assert body["status"]["diagnosis"]["modelUsed"] == "claude-opus-4-7"
    assert body["status"]["diagnosis"]["rcaCategory"] == "Kernel"
    assert len(body["status"]["diagnosis"]["evidence"]) == 3
    assert body["status"]["diagnosis"]["recommendation"]["action"] == "Cordon"
    [report_ready] = [
        c for c in body["status"]["conditions"] if c["type"] == "ReportReady"
    ]
    assert report_ready["status"] == "True"
    assert report_ready["reason"] == "EvidenceValid"


@pytest.mark.asyncio
async def test_thin_golden_writes_cr_single_evidence_accepted() -> None:
    """AC-6 unit-level: single-evidence reports are accepted by the agent."""
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload_thin.json"))

    assert result["ok"] is True
    assert result["phase"] == "Diagnosed"
    body = written[0]
    assert len(body["status"]["diagnosis"]["evidence"]) == 1
    assert body["status"]["diagnosis"]["confidence"] == 0.4
    assert body["status"]["diagnosis"]["recommendation"]["action"] == "NoAction"


# ---------------------------------------------------------------------------
# FR-12 phase-conflict deferral
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("phase", ["Failed", "Acted", "Diagnosed"])
@pytest.mark.asyncio
async def test_terminal_phase_pre_write_returns_deferred(phase: str) -> None:
    cr = _existing_cr(phase)
    replace_calls = {"n": 0}

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        replace_calls["n"] += 1
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload.json"))

    assert result["ok"] is False
    assert result["deferred"] is True
    assert result["observedPhase"] == phase
    assert replace_calls["n"] == 0


# ---------------------------------------------------------------------------
# FR-11 failure-reason mapping (RBAC + payload validation)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_403_rbac_denied_returns_cr_write_failed_envelope() -> None:
    cr = _existing_cr("Diagnosing")

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        raise ApiException(status=403, reason="Forbidden")

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload.json"))

    assert result["ok"] is False
    assert result["error"] == "CRWriteFailed"
    assert "is_error" in result and result["is_error"] is True


@pytest.mark.asyncio
async def test_invalid_payload_returns_validation_failed_envelope() -> None:
    cr = _existing_cr("Diagnosing")

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    bad = _load("emit_report_payload.json")
    bad["confidence"] = 1.5
    result = await tool.handler(bad)

    assert result["ok"] is False
    assert result["error"] == "ValidationFailed"


# ---------------------------------------------------------------------------
# Truncation (oversized evidence[].result)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_oversized_result_truncated_pre_validation() -> None:
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    payload = _load("emit_report_payload.json")
    payload["evidence"][0]["result"] = "Z" * 6000
    result = await tool.handler(payload)

    assert result["ok"] is True
    written_evidence = written[0]["status"]["diagnosis"]["evidence"]
    assert len(written_evidence[0]["result"]) == 4096


# ---------------------------------------------------------------------------
# AlreadyEmitted (defense against the runbook's calling-discipline drifting)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_duplicate_emit_returns_already_emitted_envelope() -> None:
    cr = _existing_cr("Diagnosing")
    written: list[dict[str, Any]] = []

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        written.append(body)
        return body

    writer = _make_writer(getter, replacer)
    tool = make_emit_report(_case(), writer, model_used="claude-opus-4-7")
    payload = _load("emit_report_payload.json")
    first = await tool.handler(payload)
    second = await tool.handler(payload)

    assert first["ok"] is True
    assert second["ok"] is False
    assert second["error"] == "AlreadyEmitted"
    # Only one CR write despite two tool calls.
    assert len(written) == 1


# ---------------------------------------------------------------------------
# nhd_name fallback path
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_missing_nhd_name_uses_find_by_case_id(monkeypatch) -> None:
    cr = _existing_cr("Diagnosing")

    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return cr

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    writer = _make_writer(getter, replacer)

    found_calls: list[str] = []

    async def fake_find(case_id: str) -> str:
        found_calls.append(case_id)
        return NHD_NAME

    monkeypatch.setattr(writer, "find_by_case_id", fake_find)

    case = _case()
    case.nhd_name = None
    tool = make_emit_report(case, writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload.json"))

    assert result["ok"] is True
    assert found_calls == [CASE_ID]


@pytest.mark.asyncio
async def test_nhd_name_unresolved_returns_cr_write_failed(monkeypatch) -> None:
    async def getter(_ns: str, _n: str) -> dict[str, Any]:
        return _existing_cr("Diagnosing")

    async def replacer(_ns: str, _n: str, body: dict[str, Any]) -> dict[str, Any]:
        return body

    writer = _make_writer(getter, replacer)

    async def fake_find(_case_id: str) -> str | None:
        return None

    monkeypatch.setattr(writer, "find_by_case_id", fake_find)

    case = _case()
    case.nhd_name = None
    tool = make_emit_report(case, writer, model_used="claude-opus-4-7")
    result = await tool.handler(_load("emit_report_payload.json"))

    assert result["ok"] is False
    assert result["error"] == "CRWriteFailed"
