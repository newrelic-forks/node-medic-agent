"""Integration test for the concurrency cap (T031).

FR-1: when ``MAX_CONCURRENT_CASES`` slots are full, additional POSTs return
429 ``{caseId, status: "rejected", reason: "concurrency_cap"}``. The
case-table check happens FIRST so duplicate POSTs to in-flight cases still
return 202 (idempotent), even when the cap is full.
"""
from __future__ import annotations

import asyncio
import copy
import json
import threading
from pathlib import Path
from typing import Any

import pytest
from fastapi.testclient import TestClient

from nodemedic_agent.app import create_app
from nodemedic_agent.config import Settings
from nodemedic_agent.runner.case_table import Case

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"


@pytest.fixture
def settings(monkeypatch: pytest.MonkeyPatch) -> Settings:
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "tok-fake")
    monkeypatch.setenv("NR_MCP_URL", "https://mcp.example.test")
    monkeypatch.setenv("NR_MCP_TOKEN", "tok-nr-fake")
    monkeypatch.setenv("CLUSTER_NAME", "test-fake")
    monkeypatch.setenv("CLOUD_PROVIDER", "aws")
    monkeypatch.setenv("STUB_AGENT", "true")
    monkeypatch.setenv("MAX_CONCURRENT_CASES", "2")
    return Settings()


@pytest.fixture
def slow_worker(monkeypatch: pytest.MonkeyPatch) -> dict[str, int]:
    """Worker that holds the slot for ~1s so the cap is observably full."""
    counter = {"started": 0, "completed": 0}

    async def fake_run(case: Case, *_args: Any, **_kwargs: Any) -> None:
        counter["started"] += 1
        try:
            await asyncio.sleep(1.0)
        finally:
            counter["completed"] += 1

    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case_stub", fake_run, raising=False
    )
    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case", fake_run, raising=False
    )
    return counter


def _make_body(case_id: str) -> dict[str, Any]:
    body = json.loads((FIXTURES / "case_aws.json").read_text())
    body["caseId"] = case_id
    return body


def test_three_distinct_posts_with_cap_two_yields_one_429(
    settings: Settings, slow_worker: dict[str, int]
) -> None:
    app = create_app(settings)
    case_ids = [
        "00000000-0000-4000-8000-000000000001",
        "00000000-0000-4000-8000-000000000002",
        "00000000-0000-4000-8000-000000000003",
    ]
    bodies = [_make_body(cid) for cid in case_ids]

    statuses: list[int] = []
    payloads: list[dict[str, Any]] = []
    lock = threading.Lock()

    with TestClient(app) as client:
        def post_one(body: dict[str, Any]) -> None:
            r = client.post("/diagnose", json=body)
            with lock:
                statuses.append(r.status_code)
                payloads.append(r.json())

        threads = [threading.Thread(target=post_one, args=(b,)) for b in bodies]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

    # Exactly one 429, two 202s.
    assert sorted(statuses) == [202, 202, 429]
    rejected = [p for p, s in zip(payloads, statuses) if s == 429]
    assert len(rejected) == 1
    assert rejected[0]["status"] == "rejected"
    assert rejected[0]["reason"] == "concurrency_cap"
