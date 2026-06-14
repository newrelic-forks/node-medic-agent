"""Unit tests for `CaseTable` (T019).

FR-10 idempotency:
  - register-new → was_new=True (caller spawns worker, returns 202 queued)
  - re-register-same-id while QUEUED/RUNNING → was_new=False, no second worker
  - re-register-same-id while COMPLETE within 30 min → was_new=False
  - re-register after retention sweep at 30:01 → fresh QUEUED

Uses an injected clock instead of freezegun so the case table's own
``self._clock()`` calls advance deterministically.
"""
from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Callable

import pytest

from nodemedic_agent.runner.case_table import (
    Case,
    CaseStatus,
    CaseTable,
    RETENTION_SEC,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


T0 = datetime(2026, 6, 13, 14, 0, 0, tzinfo=timezone.utc)


class _MutableClock:
    def __init__(self) -> None:
        self.now = T0

    def __call__(self) -> datetime:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now = self.now + timedelta(seconds=seconds)


def _make_case(case_id: str = "1a2b3c4d-5e6f-4789-9abc-def012345678") -> Case:
    return Case(
        case_id=case_id,
        node_name="cf1z-general-nodes-2000002",
        cluster_name="cf1z",
        provider="azure",
        region="eastus2",
        instance_id="cf1z-general-nodes-2000002",
        trigger_type="ContainerRuntimeUnhealthy",
        trigger_reason="ContainerdUnreachable",
        trigger_message="containerd socket unreachable",
        trigger_observed_at=T0,
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        started_at=T0,
    )


# ---------------------------------------------------------------------------
# Idempotency truth table
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_first_register_is_new() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    stored, was_new = await table.try_register(case)
    assert was_new is True
    assert stored.case_id == case.case_id
    assert stored.status is CaseStatus.QUEUED


@pytest.mark.asyncio
async def test_duplicate_register_while_queued_returns_existing() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    second_attempt = _make_case()
    second_attempt.node_name = "ignored-by-second-attempt"
    stored, was_new = await table.try_register(second_attempt)
    assert was_new is False
    # First-write wins.
    assert stored.node_name == "cf1z-general-nodes-2000002"


@pytest.mark.asyncio
async def test_duplicate_register_while_running_returns_existing() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_running(case.case_id)
    _, was_new = await table.try_register(_make_case())
    assert was_new is False


@pytest.mark.asyncio
async def test_duplicate_register_while_complete_within_retention_returns_existing() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_complete(case.case_id, "Diagnosed")
    # 29:59 — still within retention.
    clock.advance(RETENTION_SEC - 1)
    stored, was_new = await table.try_register(_make_case())
    assert was_new is False
    assert stored.status is CaseStatus.COMPLETE
    assert stored.final_phase == "Diagnosed"


@pytest.mark.asyncio
async def test_register_after_retention_starts_fresh() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_complete(case.case_id, "Diagnosed")
    clock.advance(RETENTION_SEC + 1)  # 30:01

    stored, was_new = await table.try_register(_make_case())
    assert was_new is True
    assert stored.status is CaseStatus.QUEUED
    assert stored.final_phase is None  # fresh entry


# ---------------------------------------------------------------------------
# Lookup
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_lookup_returns_none_for_unknown_id() -> None:
    table = CaseTable()
    assert await table.lookup("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa") is None


@pytest.mark.asyncio
async def test_lookup_returns_none_for_expired_entry() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_complete(case.case_id, "Diagnosed")
    clock.advance(RETENTION_SEC + 1)
    assert await table.lookup(case.case_id) is None


# ---------------------------------------------------------------------------
# Failure reason propagation
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_mark_complete_records_failure_reason() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_complete(case.case_id, "Failed", failure_reason="ModelHalted")
    stored = await table.lookup(case.case_id)
    assert stored is not None
    assert stored.final_phase == "Failed"
    assert stored.failure_reason == "ModelHalted"


# ---------------------------------------------------------------------------
# Sweeper
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_purge_expired_drops_aged_entries() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_complete(case.case_id, "Diagnosed")

    assert await table.size() == 1
    clock.advance(RETENTION_SEC + 1)
    purged = await table.purge_expired()
    assert purged == 1
    assert await table.size() == 0


@pytest.mark.asyncio
async def test_purge_keeps_running_cases() -> None:
    clock = _MutableClock()
    table = CaseTable(clock=clock)
    case = _make_case()
    await table.try_register(case)
    await table.mark_running(case.case_id)
    clock.advance(RETENTION_SEC + 1)  # cannot expire, status != COMPLETE
    assert await table.purge_expired() == 0
    assert await table.size() == 1
