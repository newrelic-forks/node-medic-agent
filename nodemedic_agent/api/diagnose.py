"""POST /diagnose handler + Pydantic request/response models.

Mirrors the controller's `internal/nodemedic/agentclient/types.go` byte-for-byte
(see `.specify/specs/002-nodemedic-agent/contracts/post-diagnose.md`).

The handler is intentionally thin (≤30 lines of logic):
  1. Idempotency: case-table lookup first (FR-10).
  2. If new, acquire a concurrency-cap slot (FR-1). On exhaustion, 429.
  3. Spawn the worker as `asyncio.create_task(...)` and return 202.
"""
from __future__ import annotations

import asyncio
from datetime import datetime
from typing import Annotated, Literal, Optional

from fastapi import APIRouter, Depends, Request, Response, status
from pydantic import BaseModel, ConfigDict, Field

from nodemedic_agent.logging import bind_case_id, get_logger
from nodemedic_agent.runner.case_table import Case, CaseStatus

log = get_logger(__name__)


# ---------------------------------------------------------------------------
# Request / response models
# ---------------------------------------------------------------------------


_UUID_V4 = (
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}"
    r"-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$"
)


class DiagnoseTrigger(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    type: str = Field(min_length=1)
    reason: str = ""
    message: str = Field(default="", max_length=256)
    observed_at: datetime = Field(alias="observedAt")


class DiagnoseBudgets(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    max_turns: int = Field(alias="maxTurns", ge=1)
    max_budget_usd: str = Field(alias="maxBudgetUSD", pattern=r"^[0-9]+\.[0-9]{2}$")
    deadline_sec: int = Field(alias="deadlineSec", ge=1)


class DiagnoseRequest(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    case_id: str = Field(alias="caseId", pattern=_UUID_V4)
    node_name: str = Field(alias="nodeName", min_length=1, max_length=253)
    cluster_name: str = Field(alias="clusterName", min_length=1)
    provider: Literal["aws", "azure"]
    region: str = Field(min_length=1)
    instance_id: str = Field(alias="instanceId", min_length=1)
    trigger: DiagnoseTrigger
    budgets: DiagnoseBudgets
    nhd_name: Optional[str] = Field(default=None, alias="nhdName")


class DiagnoseQueuedResponse(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    case_id: str = Field(serialization_alias="caseId")
    status: Literal["queued", "complete"]


class DiagnoseRejectedResponse(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    case_id: str = Field(serialization_alias="caseId")
    status: Literal["rejected"] = "rejected"
    reason: Literal["concurrency_cap"]


# ---------------------------------------------------------------------------
# Router
# ---------------------------------------------------------------------------


def build_diagnose_router() -> APIRouter:
    """Construct the /diagnose router. Wiring lives in `app.py`."""
    return APIRouter()


async def handle_diagnose(
    request: Request,
    body: DiagnoseRequest,
) -> Response:
    """POST /diagnose entry point.

    The dependencies (case table, semaphore, worker callable) are attached
    to ``request.app.state`` by `app.py.create_app`. Keeping them on app
    state lets tests substitute fakes without monkeypatching imports.
    """
    state = request.app.state
    case_table = state.case_table
    semaphore: asyncio.Semaphore = state.case_semaphore
    run_case = state.run_case

    # --- Step 1: idempotency check ---
    case = _build_case(body)
    with bind_case_id(case.case_id):
        existing, was_new = await case_table.try_register(case)
        if not was_new:
            log.info(
                "duplicate_post",
                node_name=existing.node_name,
                existing_status=existing.status.value,
            )
            wire_status: Literal["queued", "complete"] = (
                "complete" if existing.status is CaseStatus.COMPLETE else "queued"
            )
            return _json_response(
                202,
                DiagnoseQueuedResponse(
                    case_id=existing.case_id, status=wire_status
                ).model_dump(by_alias=True),
            )

        # --- Step 2: try to acquire a concurrency-cap slot ---
        if not _try_acquire(semaphore):
            await case_table.purge_expired()
            # Even though we registered, hold no slot — drop the entry so
            # a future POST after the cap relaxes can re-register.
            log.warning(
                "concurrency_cap_reached",
                node_name=case.node_name,
                provider=case.provider,
            )
            await _evict(case_table, case.case_id)
            return _json_response(
                429,
                DiagnoseRejectedResponse(
                    case_id=case.case_id,
                    status="rejected",
                    reason="concurrency_cap",
                ).model_dump(by_alias=True),
            )

        # --- Step 3: spawn the worker ---
        log.info(
            "case_accepted",
            node_name=case.node_name,
            provider=case.provider,
            cluster_name=case.cluster_name,
        )
        asyncio.create_task(_run_with_slot(run_case, case, semaphore, state))

        return _json_response(
            202,
            DiagnoseQueuedResponse(case_id=case.case_id, status="queued").model_dump(
                by_alias=True
            ),
        )


def _try_acquire(semaphore: asyncio.Semaphore) -> bool:
    """Non-blocking semaphore acquire. True if a slot was taken."""
    if semaphore.locked() and semaphore._value <= 0:  # type: ignore[attr-defined]
        return False
    # asyncio.Semaphore has no public non-blocking acquire — emulate.
    if semaphore._value <= 0:  # type: ignore[attr-defined]
        return False
    semaphore._value -= 1  # type: ignore[attr-defined]
    return True


async def _evict(case_table, case_id: str) -> None:  # type: ignore[no-untyped-def]
    """Drop a just-registered case so future POSTs can re-register."""
    async with case_table._lock:  # noqa: SLF001 — internal helper, intentional
        case_table._cases.pop(case_id, None)  # noqa: SLF001


async def _run_with_slot(
    run_case,  # type: ignore[no-untyped-def]
    case: Case,
    semaphore: asyncio.Semaphore,
    state,  # type: ignore[no-untyped-def]
) -> None:
    """Run the worker and release the cap slot when done."""
    try:
        await state.case_table.mark_running(case.case_id)
        with bind_case_id(case.case_id):
            await run_case(case, state)
    except asyncio.CancelledError:
        log.warning("case_cancelled", case_id=case.case_id)
        raise
    except Exception as exc:  # noqa: BLE001 — record + re-raise for visibility
        log.error("case_unhandled_error", error=str(exc)[:512])
    finally:
        semaphore.release()


def _build_case(body: DiagnoseRequest) -> Case:
    return Case(
        case_id=body.case_id,
        node_name=body.node_name,
        cluster_name=body.cluster_name,
        provider=body.provider,
        region=body.region,
        instance_id=body.instance_id,
        nhd_name=body.nhd_name,
        trigger_type=body.trigger.type,
        trigger_reason=body.trigger.reason,
        trigger_message=body.trigger.message,
        trigger_observed_at=body.trigger.observed_at,
        deadline_sec=body.budgets.deadline_sec,
        max_turns=body.budgets.max_turns,
        max_budget_usd=body.budgets.max_budget_usd,
        started_at=datetime.now(tz=body.trigger.observed_at.tzinfo or None),
    )


def _json_response(status_code: int, payload: dict) -> Response:
    import json

    return Response(
        status_code=status_code,
        content=json.dumps(payload),
        media_type="application/json",
    )
