"""Integration test for FR-10 idempotency on POST /diagnose (T030).

POSTing the same body twice rapidly while a fake worker holds the case in
RUNNING must produce exactly one worker spawn.
"""
from __future__ import annotations

import asyncio
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
    return Settings()


@pytest.fixture
def hold_event() -> asyncio.Event:
    return asyncio.Event()


@pytest.fixture
def started_count(monkeypatch: pytest.MonkeyPatch) -> dict[str, int]:
    """Fake worker that increments a counter and blocks until released."""
    counter = {"started": 0}

    async def fake_run(case: Case, *_args: Any, **_kwargs: Any) -> None:
        counter["started"] += 1
        # Long enough to overlap two POSTs but short enough to keep the
        # test fast.
        await asyncio.sleep(0.5)

    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case_stub", fake_run, raising=False
    )
    monkeypatch.setattr(
        "nodemedic_agent.runner.case_worker.run_case", fake_run, raising=False
    )
    return counter


def test_two_rapid_posts_spawn_one_worker(
    settings: Settings, started_count: dict[str, int]
) -> None:
    app = create_app(settings)
    body = json.loads((FIXTURES / "case_aws.json").read_text())

    with TestClient(app) as client:
        responses: list[int] = []

        def post_once() -> None:
            r = client.post("/diagnose", json=body)
            responses.append(r.status_code)

        # Two threads racing to POST the same body.
        t1 = threading.Thread(target=post_once)
        t2 = threading.Thread(target=post_once)
        t1.start()
        t2.start()
        t1.join()
        t2.join()

        assert responses == [202, 202]
        # Both POSTs returned 202 — but only one worker should have started.
        assert started_count["started"] == 1
