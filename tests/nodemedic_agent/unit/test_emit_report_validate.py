"""Unit tests for `EmitReportPayload` (T049).

Covers the validation rules in data-model.md §4 and contracts/emit-report-tool.md:

  - happy-path golden passes
  - thin (single-evidence) golden passes — FR-8 has no agent-side count gate
  - missing fields → ValidationError
  - confidence out of [0, 1] → ValidationError
  - bad rcaCategory enum → ValidationError
  - bad recommendation.action enum → ValidationError
  - oversized rootCause → ValidationError
  - oversized evidence[].result → caller truncates to 4 KB; payload that
    sneaks past the 4 KB cap raises ValidationError (model is the last gate)
  - blank recommendation.reason → ValidationError

The tests intentionally drive Pydantic directly. The runner-level truncation
behavior is exercised in tests/nodemedic_agent/integration/test_emit_report_writes_cr.py
once the runner exists (T053/T054).
"""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from pydantic import ValidationError

from nodemedic_agent.tools.emit_report import (
    EmitReportPayload,
    truncate_evidence_results,
)

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"


def _load(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES / name).read_text())


# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------


def test_happy_path_golden_validates() -> None:
    payload = _load("emit_report_payload.json")
    parsed = EmitReportPayload.model_validate(payload)
    assert parsed.confidence == 0.85
    assert parsed.rca_category == "Kernel"
    assert len(parsed.evidence) == 3
    assert parsed.recommendation.action == "Cordon"


def test_thin_golden_accepted_single_evidence() -> None:
    """AC-6: single-evidence payloads are valid; controller's gate decides."""
    payload = _load("emit_report_payload_thin.json")
    parsed = EmitReportPayload.model_validate(payload)
    assert len(parsed.evidence) == 1
    assert parsed.confidence == 0.4
    assert parsed.recommendation.action == "NoAction"


# ---------------------------------------------------------------------------
# Required-field validation
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "field",
    ["rootCause", "rcaCategory", "confidence", "evidence", "recommendation"],
)
def test_missing_required_field_raises(field: str) -> None:
    payload = _load("emit_report_payload.json")
    payload.pop(field)
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


def test_empty_evidence_list_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["evidence"] = []
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


# ---------------------------------------------------------------------------
# Confidence range
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("bad_confidence", [-0.1, 1.5, 2.0, -1.0])
def test_confidence_out_of_range_rejected(bad_confidence: float) -> None:
    payload = _load("emit_report_payload.json")
    payload["confidence"] = bad_confidence
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


@pytest.mark.parametrize("ok_confidence", [0.0, 0.5, 0.7, 1.0])
def test_confidence_inclusive_bounds_pass(ok_confidence: float) -> None:
    payload = _load("emit_report_payload.json")
    payload["confidence"] = ok_confidence
    parsed = EmitReportPayload.model_validate(payload)
    assert parsed.confidence == ok_confidence


# ---------------------------------------------------------------------------
# Enum validation
# ---------------------------------------------------------------------------


def test_bad_rca_category_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["rcaCategory"] = "WrongEnum"
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


def test_bad_recommendation_action_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["recommendation"]["action"] = "DropNode"
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


def test_bad_evidence_source_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["evidence"][0]["source"] = "psychic"
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


# ---------------------------------------------------------------------------
# Length / blank validation
# ---------------------------------------------------------------------------


def test_root_cause_too_long_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["rootCause"] = "x" * 4097
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


def test_blank_recommendation_reason_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["recommendation"]["reason"] = ""
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


def test_blank_root_cause_rejected() -> None:
    payload = _load("emit_report_payload.json")
    payload["rootCause"] = ""
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)


# ---------------------------------------------------------------------------
# Result truncation (runner-level, pre-validation)
# ---------------------------------------------------------------------------


def test_truncate_evidence_results_caps_at_4kb() -> None:
    """`truncate_evidence_results` is the runner's pre-validation lenient
    truncation step. Strings >4 KB are truncated; ≤4 KB pass through."""
    huge = "y" * 6000
    payload = _load("emit_report_payload.json")
    payload["evidence"][0]["result"] = huge
    truncated, hits = truncate_evidence_results(payload, limit=4096)
    assert hits == 1
    assert len(truncated["evidence"][0]["result"]) == 4096
    # Other evidence entries untouched.
    for entry in truncated["evidence"][1:]:
        assert entry["result"] == payload["evidence"][1]["result"] or len(
            entry["result"]
        ) <= 4096


def test_truncate_no_op_when_within_limit() -> None:
    payload = _load("emit_report_payload.json")
    truncated, hits = truncate_evidence_results(payload, limit=4096)
    assert hits == 0
    assert truncated == payload


def test_oversized_result_still_validates_after_truncation() -> None:
    """Pipeline check: truncate then validate."""
    huge = "z" * 5000
    payload = _load("emit_report_payload.json")
    payload["evidence"][0]["result"] = huge
    truncated, hits = truncate_evidence_results(payload, limit=4096)
    assert hits == 1
    parsed = EmitReportPayload.model_validate(truncated)
    assert len(parsed.evidence[0].result) == 4096


def test_oversized_result_rejected_without_truncation() -> None:
    """Defense in depth: if truncation is skipped, the model still rejects."""
    huge = "q" * 5000
    payload = _load("emit_report_payload.json")
    payload["evidence"][0]["result"] = huge
    with pytest.raises(ValidationError):
        EmitReportPayload.model_validate(payload)
