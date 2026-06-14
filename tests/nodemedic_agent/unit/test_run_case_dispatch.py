"""Dispatch tests for `run_case` (T058).

Confirms the runtime dispatcher routes to the stub or real worker based
on ``Settings.stub_agent`` without firing the SDK loop or sleeping.

The real worker is heavy (subprocess, MCP servers, Anthropic gateway
round-trip), so this test patches it with a recorder. The stub worker
sleep is also patched so the test runs in milliseconds.
"""
from __future__ import annotations

from datetime import datetime, timezone
from types import SimpleNamespace

import pytest

from nodemedic_agent.runner import case_worker
from nodemedic_agent.runner.case_table import Case, CaseStatus, CaseTable


def _case() -> Case:
    return Case(
        case_id="aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
        node_name="cf1z-test-node",
        cluster_name="cf1z",
        provider="azure",
        region="eastus2",
        instance_id="cf1z-test-node",
        nhd_name="cf1z-test-node-1",
        trigger_type="ContainerRuntimeUnhealthy",
        trigger_reason="ContainerdUnreachable",
        trigger_message="",
        trigger_observed_at=datetime(2026, 6, 13, tzinfo=timezone.utc),
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        status=CaseStatus.RUNNING,
        started_at=datetime(2026, 6, 13, tzinfo=timezone.utc),
    )


def _state(stub_agent: bool) -> SimpleNamespace:
    return SimpleNamespace(
        settings=SimpleNamespace(
            stub_agent=stub_agent,
            claude_model="claude-opus-4-7",
            claude_fallback_model="claude-sonnet-4-6",
        ),
        case_table=CaseTable(),
        nhd_writer=None,
        model_resolver=SimpleNamespace(cached=None),
    )


@pytest.mark.asyncio
async def test_dispatch_routes_to_stub_when_flag_true(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    stub_calls: list[Case] = []
    real_calls: list[Case] = []

    async def fake_stub(case: Case, _state) -> None:
        stub_calls.append(case)

    async def fake_real(case: Case, _state) -> None:
        real_calls.append(case)

    monkeypatch.setattr(case_worker, "run_case_stub", fake_stub)
    monkeypatch.setattr(case_worker, "run_case_real", fake_real)

    await case_worker.run_case(_case(), _state(stub_agent=True))
    assert len(stub_calls) == 1
    assert real_calls == []


@pytest.mark.asyncio
async def test_dispatch_routes_to_real_when_flag_false(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    stub_calls: list[Case] = []
    real_calls: list[Case] = []

    async def fake_stub(case: Case, _state) -> None:
        stub_calls.append(case)

    async def fake_real(case: Case, _state) -> None:
        real_calls.append(case)

    monkeypatch.setattr(case_worker, "run_case_stub", fake_stub)
    monkeypatch.setattr(case_worker, "run_case_real", fake_real)

    await case_worker.run_case(_case(), _state(stub_agent=False))
    assert stub_calls == []
    assert len(real_calls) == 1
