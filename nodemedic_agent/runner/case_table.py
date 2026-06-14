"""In-memory case table (FR-10 idempotency).

Maps ``case_id`` → ``Case`` for the lifetime of the agent process. The
controller may re-POST the same ``caseId`` multiple times (Spec 001 FR-7
retry path); this table is the source of truth for "is this case already
running" so we never spawn a second worker for a duplicate POST.

Retention: 30 minutes after a case enters ``COMPLETE`` (FR-10). A
background sweeper task drops aged-out entries every 5 minutes; tests can
call ``CaseTable.purge_expired()`` directly with an injected clock.
"""
from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from enum import Enum
from typing import Callable, Optional

from pydantic import BaseModel, ConfigDict


class CaseStatus(str, Enum):
    QUEUED = "queued"
    RUNNING = "running"
    COMPLETE = "complete"


class Case(BaseModel):
    """Snapshot of a case as the agent saw it on POST /diagnose.

    Fields populated incrementally — at register time we know the request
    body; at completion we set ``final_phase`` / ``failure_reason``.
    """

    model_config = ConfigDict(arbitrary_types_allowed=True)

    case_id: str
    node_name: str
    cluster_name: str
    provider: str
    region: str
    instance_id: str
    nhd_name: Optional[str] = None
    trigger_type: str
    trigger_reason: str = ""
    trigger_message: str = ""
    trigger_observed_at: datetime
    deadline_sec: int
    max_turns: int
    max_budget_usd: str

    status: CaseStatus = CaseStatus.QUEUED
    started_at: datetime
    completed_at: Optional[datetime] = None
    final_phase: Optional[str] = None
    failure_reason: Optional[str] = None


# Retention is per FR-10: 30 minutes from completion.
RETENTION_SEC = 30 * 60
SWEEP_INTERVAL_SEC = 5 * 60


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


class CaseTable:
    """Thread-safe in-memory map of caseId → Case.

    The lock guards the register/lookup/mark-* path; the per-case worker
    lifecycle does not hold the lock.
    """

    def __init__(
        self,
        clock: Callable[[], datetime] = _utcnow,
        retention_sec: int = RETENTION_SEC,
    ) -> None:
        self._cases: dict[str, Case] = {}
        self._lock = asyncio.Lock()
        self._clock = clock
        self._retention = timedelta(seconds=retention_sec)
        self._sweeper_task: asyncio.Task[None] | None = None

    # ------------------------------------------------------------------
    # Register / lookup
    # ------------------------------------------------------------------

    async def try_register(self, case: Case) -> tuple[Case, bool]:
        """Insert if absent. Returns ``(case_in_table, was_new)``.

        - was_new=True → new entry inserted with status=QUEUED. Caller
          should spawn the worker.
        - was_new=False → an entry for this caseId already exists (in any
          state). Caller returns 202 with the existing status; no worker
          spawn.

        FR-10 retention: an entry that has aged past 30 min in COMPLETE is
        dropped before the insert decision so a re-POST after retention
        starts fresh.
        """
        async with self._lock:
            existing = self._cases.get(case.case_id)
            if existing is not None and not self._is_expired(existing):
                return existing, False
            self._cases[case.case_id] = case
            return case, True

    async def lookup(self, case_id: str) -> Case | None:
        async with self._lock:
            entry = self._cases.get(case_id)
            if entry is not None and self._is_expired(entry):
                # Expired entries don't exist for FR-10 purposes.
                return None
            return entry

    # ------------------------------------------------------------------
    # State transitions
    # ------------------------------------------------------------------

    async def mark_running(self, case_id: str) -> None:
        async with self._lock:
            entry = self._cases.get(case_id)
            if entry is not None:
                entry.status = CaseStatus.RUNNING

    async def mark_complete(
        self,
        case_id: str,
        final_phase: str,
        failure_reason: str | None = None,
    ) -> None:
        async with self._lock:
            entry = self._cases.get(case_id)
            if entry is not None:
                entry.status = CaseStatus.COMPLETE
                entry.completed_at = self._clock()
                entry.final_phase = final_phase
                entry.failure_reason = failure_reason

    # ------------------------------------------------------------------
    # Retention
    # ------------------------------------------------------------------

    def _is_expired(self, case: Case) -> bool:
        if case.status is not CaseStatus.COMPLETE or case.completed_at is None:
            return False
        return self._clock() - case.completed_at > self._retention

    async def purge_expired(self) -> int:
        """Drop COMPLETE cases past the retention window. Returns count."""
        async with self._lock:
            expired = [cid for cid, c in self._cases.items() if self._is_expired(c)]
            for cid in expired:
                del self._cases[cid]
            return len(expired)

    async def size(self) -> int:
        async with self._lock:
            return len(self._cases)

    # ------------------------------------------------------------------
    # Background sweeper
    # ------------------------------------------------------------------

    async def start_sweeper(self, interval_sec: float = SWEEP_INTERVAL_SEC) -> None:
        if self._sweeper_task is not None:
            return
        self._sweeper_task = asyncio.create_task(self._sweep_loop(interval_sec))

    async def stop_sweeper(self) -> None:
        if self._sweeper_task is None:
            return
        self._sweeper_task.cancel()
        try:
            await self._sweeper_task
        except asyncio.CancelledError:
            pass
        self._sweeper_task = None

    async def _sweep_loop(self, interval_sec: float) -> None:
        while True:
            await asyncio.sleep(interval_sec)
            await self.purge_expired()
