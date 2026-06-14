"""Integration tests for `POST /diagnose` (T029).

Uses FastAPI's TestClient against `create_app(...)`. The Claude Agent SDK
loop is replaced with a no-op fake worker so tests don't talk to the
gateway.
"""
from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from typing import Any

import pytest
from fastapi.testclient import TestClient

from nodemedic_agent.app import create_app
from nodemedic_agent.config import Settings
from nodemedic_agent.runner.case_table import Case

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"


# ---------------------------------------------------------------------------
# Test app factory
# ---------------------------------------------------------------------------


@pytest.fixture
def settings(monkeypatch: pytest.MonkeyPatch) -> Settings:
    """Settings overrides for tests — pin everything to in-memory."""
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "tok-fake")
    monkeypatch.setenv("NR_MCP_URL", "https://mcp.example.test")
    monkeypatch.setenv("NR_MCP_TOKEN", "tok-nr-fake")
    monkeypatch.setenv("CLUSTER_NAME", "test-fake")
    monkeypatch.setenv("CLOUD_PROVIDER", "aws")
    monkeypatch.setenv("STUB_AGENT", "true")
    monkeypatch.setenv("LOG_LEVEL", "info")
    monkeypatch.setenv("MAX_CONCURRENT_CASES", "32")
    return Settings()


@pytest.fixture
def fake_worker_calls(monkeypatch: pytest.MonkeyPatch) -> list[Case]:
    """Replace the case worker with a no-op that records calls."""
    calls: list[Case] = []

    async def fake_run(case: Case, *_args: Any, **_kwargs: Any) -> None:
        calls.append(case)
        await asyncio.sleep(0)

    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case_stub", fake_run, raising=False
    )
    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case", fake_run, raising=False
    )
    return calls


@pytest.fixture
def client(settings: Settings, fake_worker_calls: list[Case]):
    app = create_app(settings)
    with TestClient(app) as c:
        yield c


def _load_fixture(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES / name).read_text())


# ---------------------------------------------------------------------------
# Happy-path 202s
# ---------------------------------------------------------------------------


def test_aws_golden_returns_202_queued(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    response = client.post("/diagnose", json=body)
    assert response.status_code == 202
    payload = response.json()
    assert payload["caseId"] == body["caseId"]
    assert payload["status"] == "queued"


def test_azure_golden_returns_202_queued(client: TestClient) -> None:
    body = _load_fixture("case_azure.json")
    response = client.post("/diagnose", json=body)
    assert response.status_code == 202
    payload = response.json()
    assert payload["caseId"] == body["caseId"]
    assert payload["status"] == "queued"


def test_authorization_header_ignored(client: TestClient) -> None:
    """FR-1: hackathon scope has no /diagnose auth — header ignored."""
    body = _load_fixture("case_azure.json")
    response = client.post(
        "/diagnose",
        json=body,
        headers={"Authorization": "Bearer junk"},
    )
    assert response.status_code == 202


# ---------------------------------------------------------------------------
# Validation 400s
# ---------------------------------------------------------------------------


def test_missing_required_field_returns_400(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    body.pop("nodeName")
    response = client.post("/diagnose", json=body)
    assert response.status_code == 422  # FastAPI validates request body → 422
    detail = response.json()["detail"]
    assert any("nodeName" in str(err.get("loc", "")) for err in detail)


def test_bad_provider_enum_returns_400(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    body["provider"] = "gcp"
    response = client.post("/diagnose", json=body)
    assert response.status_code == 422


def test_bad_uuid_returns_400(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    body["caseId"] = "not-a-uuid"
    response = client.post("/diagnose", json=body)
    assert response.status_code == 422


def test_bad_max_budget_usd_regex_returns_400(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    body["budgets"]["maxBudgetUSD"] = "0.5"  # missing trailing zero
    response = client.post("/diagnose", json=body)
    assert response.status_code == 422


def test_bad_max_turns_returns_400(client: TestClient) -> None:
    body = _load_fixture("case_aws.json")
    body["budgets"]["maxTurns"] = 0
    response = client.post("/diagnose", json=body)
    assert response.status_code == 422


# ---------------------------------------------------------------------------
# Idempotency surface (T030 covers in detail; this is the smoke check)
# ---------------------------------------------------------------------------


def test_duplicate_post_does_not_spawn_second_worker(
    client: TestClient,
    fake_worker_calls: list[Case],
) -> None:
    body = _load_fixture("case_aws.json")
    r1 = client.post("/diagnose", json=body)
    r2 = client.post("/diagnose", json=body)
    assert r1.status_code == 202
    assert r2.status_code == 202
    # Exactly one worker invocation despite two POSTs.
    assert len(fake_worker_calls) == 1
