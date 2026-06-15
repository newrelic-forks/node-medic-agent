"""NHD status writer (R-13 / FR-8 / FR-12).

Wraps the raw ``CustomObjectsApi`` calls from ``client.py`` with:
  - the FR-12 phase-conflict pre-read (defers, never clobbers terminal phases);
  - the FR-8 retry policy (404 NotFound: 250 ms / 500 ms / 1 s × 3; 409
    Conflict: re-read + 1 s × 1; 5xx / network: 1 s × 1; 403 / 422: terminal);
  - the data-model.md §1.4 list+filter fallback when ``nhd_name`` is not
    provided in the request body.

All async. The kubernetes client is sync, so the runner calls this writer
via ``asyncio.to_thread(...)``; here we keep the surface async-clean so the
case worker can ``await`` it directly.
"""
from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Awaitable, Callable, Literal

from kubernetes.client.exceptions import ApiException

from nodemedic_agent.kube import client as kube_client
from nodemedic_agent.logging import get_logger

log = get_logger(__name__)


class WriteOutcome(str, Enum):
    """Three terminal shapes of a single NHD status write attempt."""

    WRITTEN = "written"
    DEFERRED_PHASE_CONFLICT = "deferred_phase_conflict"
    WRITE_FAILED = "write_failed"


@dataclass(slots=True)
class WriteResult:
    """Result of `update_status`. Either succeeded, deferred, or failed."""

    outcome: WriteOutcome
    observed_phase: str | None = None  # populated on DEFERRED_PHASE_CONFLICT
    failure_reason: str | None = None  # populated on WRITE_FAILED
    detail: str | None = None  # apiserver error message ≤512B


# Module-level retry schedule constants — tests override via
# `NHDWriter(notfound_backoff=...)` for fast-running cases.
_DEFAULT_NOTFOUND_BACKOFF: tuple[float, ...] = (0.25, 0.5, 1.0)
_DEFAULT_OTHER_BACKOFF: float = 1.0
_TERMINAL_HTTP = frozenset({403, 422})


def _now_rfc3339() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


# ---------------------------------------------------------------------------
# NHDWriter
# ---------------------------------------------------------------------------


class NHDWriter:
    """Async wrapper over the FR-8 / FR-12 NHD status write path.

    The constructor accepts injectable ``getter`` and ``replacer`` callables
    so tests can substitute them with respx-mocked HTTP shims; production
    leaves them as None and the writer dispatches to ``kube_client``.
    """

    def __init__(
        self,
        namespace: str,
        getter: Callable[[str, str], Awaitable[dict[str, Any]]] | None = None,
        replacer: Callable[[str, str, dict[str, Any]], Awaitable[dict[str, Any]]] | None = None,
        notfound_backoff: tuple[float, ...] = _DEFAULT_NOTFOUND_BACKOFF,
        other_backoff: float = _DEFAULT_OTHER_BACKOFF,
    ) -> None:
        self.namespace = namespace
        self._getter = getter
        self._replacer = replacer
        self._notfound_backoff = notfound_backoff
        self._other_backoff = other_backoff

    # ---- raw apiserver dispatch -------------------------------------------

    async def _call_get(self, name: str) -> dict[str, Any]:
        if self._getter is not None:
            return await self._getter(self.namespace, name)
        return await asyncio.to_thread(kube_client.nhd_get, self.namespace, name)

    async def _call_replace(self, name: str, body: dict[str, Any]) -> dict[str, Any]:
        if self._replacer is not None:
            return await self._replacer(self.namespace, name, body)
        return await asyncio.to_thread(
            kube_client.nhd_replace_status, self.namespace, name, body
        )

    # ---- public API -------------------------------------------------------

    async def get(self, name: str) -> dict[str, Any]:
        """Read the full NHD object (FR-12 phase re-read)."""
        return await self._call_get(name)

    async def find_by_case_id(self, case_id: str) -> str | None:
        """List+filter fallback (data-model.md §1.4).

        Used when the controller doesn't pass ``nhdName`` in the request body.
        Calls list once and filters in Python — the volume is bounded by
        ``MAX_CONCURRENT_CASES`` (default 32) so the cost is trivial.
        """
        # The list call lives behind a sync client; do it in a thread.
        def _list() -> list[dict[str, Any]]:
            api = kube_client.custom_objects_api()
            response = api.list_namespaced_custom_object(
                group=kube_client.NHD_GROUP,
                version=kube_client.NHD_VERSION,
                namespace=self.namespace,
                plural=kube_client.NHD_PLURAL,
            )
            items = response.get("items", []) if isinstance(response, dict) else []
            return list(items)

        items = await asyncio.to_thread(_list)
        for item in items:
            spec_case_id = (
                item.get("spec", {}).get("case", {}).get("caseId")
            )
            if spec_case_id == case_id:
                name = item.get("metadata", {}).get("name")
                if isinstance(name, str):
                    return name
        return None

    async def update_status(
        self,
        name: str,
        status_diagnosis: dict[str, Any],
        final_phase: Literal["Diagnosed", "Failed"],
        failure_reason: str | None = None,
    ) -> WriteResult:
        """FR-8 + FR-12 status write with retries.

        Steps:
          1. Read the CR (FR-12 phase pre-check). On NotFound, retry per the
             404 schedule. On 5xx/network, retry once.
          2. If observed phase is terminal (Diagnosed/Acted/Failed/...) →
             ``WriteOutcome.DEFERRED_PHASE_CONFLICT``. No write.
          3. Build the new ``status`` body, call ``replace_status``. Apply
             retries per FR-8 (409 → re-read + 1 s × 1; 5xx → 1 s × 1).
          4. Return ``WriteOutcome.WRITTEN`` on success or
             ``WriteOutcome.WRITE_FAILED`` on terminal apiserver error.
        """
        # ---- Step 1: phase pre-read (with NotFound retries) ----
        existing = await self._read_with_notfound_retry(name)
        if existing is None:
            # Exhausted NotFound retries.
            return WriteResult(
                outcome=WriteOutcome.WRITE_FAILED,
                failure_reason="CRWriteFailed",
                detail=f"NHD {self.namespace}/{name} not found after retries",
            )

        observed_phase = (
            (existing.get("status") or {}).get("phase") or ""
        )

        # ---- Step 2: FR-12 phase guard ----
        if not kube_client.should_overwrite(observed_phase):
            log.info(
                "deferred_write_phase_conflict",
                nhd_name=name,
                observed_phase=observed_phase,
                intended_phase=final_phase,
            )
            return WriteResult(
                outcome=WriteOutcome.DEFERRED_PHASE_CONFLICT,
                observed_phase=observed_phase,
            )

        # ---- Step 3: build and write ----
        body = self._build_status_body(
            existing=existing,
            status_diagnosis=status_diagnosis,
            final_phase=final_phase,
            failure_reason=failure_reason,
        )

        # 409 may fire if a concurrent writer touched the CR between our
        # read and this write; the spec's policy is "re-read + 1s × 1".
        attempt = 0
        last_error: ApiException | None = None
        while attempt < 2:  # initial attempt + one re-read retry on 409
            try:
                await self._call_replace(name, body)
                return WriteResult(outcome=WriteOutcome.WRITTEN)
            except ApiException as exc:
                last_error = exc
                status = int(exc.status or 0)
                if status in _TERMINAL_HTTP:
                    log.error(
                        "nhd_write_terminal",
                        nhd_name=name,
                        http_status=status,
                        reason=exc.reason,
                    )
                    return WriteResult(
                        outcome=WriteOutcome.WRITE_FAILED,
                        failure_reason="CRWriteFailed",
                        detail=str(exc.reason)[:512],
                    )
                if status == 409:
                    # Re-read the CR and rebuild the body so resourceVersion
                    # matches before retrying.
                    refreshed = await self._read_with_other_retry(name)
                    if refreshed is None:
                        log.error("nhd_write_409_reread_failed", nhd_name=name)
                        return WriteResult(
                            outcome=WriteOutcome.WRITE_FAILED,
                            failure_reason="CRWriteFailed",
                            detail="re-read after 409 failed",
                        )
                    new_phase = (refreshed.get("status") or {}).get("phase") or ""
                    if not kube_client.should_overwrite(new_phase):
                        log.info(
                            "deferred_write_phase_conflict",
                            nhd_name=name,
                            observed_phase=new_phase,
                            intended_phase=final_phase,
                            stage="post_409_reread",
                        )
                        return WriteResult(
                            outcome=WriteOutcome.DEFERRED_PHASE_CONFLICT,
                            observed_phase=new_phase,
                        )
                    body = self._build_status_body(
                        existing=refreshed,
                        status_diagnosis=status_diagnosis,
                        final_phase=final_phase,
                        failure_reason=failure_reason,
                    )
                    log.warning("nhd_write_409_retrying", nhd_name=name)
                    await asyncio.sleep(self._other_backoff)
                    attempt += 1
                    continue
                # 5xx / network / unknown → 1s retry once.
                if attempt == 0:
                    log.warning(
                        "nhd_write_transient_retrying",
                        nhd_name=name,
                        http_status=status,
                    )
                    await asyncio.sleep(self._other_backoff)
                    attempt += 1
                    continue
                # Second transient — give up.
                break
            except Exception as exc:  # noqa: BLE001 — any non-API error is transient
                last_error = None  # type: ignore[assignment]
                if attempt == 0:
                    log.warning("nhd_write_unknown_retrying", error=str(exc)[:512])
                    await asyncio.sleep(self._other_backoff)
                    attempt += 1
                    continue
                log.error("nhd_write_unknown_terminal", error=str(exc)[:512])
                return WriteResult(
                    outcome=WriteOutcome.WRITE_FAILED,
                    failure_reason="CRWriteFailed",
                    detail=str(exc)[:512],
                )

        # Loop fell through — terminal failure.
        detail = (
            f"http {last_error.status} {last_error.reason}"
            if last_error is not None
            else "exhausted retries"
        )
        log.error("nhd_write_failed", nhd_name=name, detail=detail)
        return WriteResult(
            outcome=WriteOutcome.WRITE_FAILED,
            failure_reason="CRWriteFailed",
            detail=detail[:512],
        )

    # ---- internal helpers -------------------------------------------------

    async def _read_with_notfound_retry(self, name: str) -> dict[str, Any] | None:
        schedule = (0.0, *self._notfound_backoff)  # initial attempt + 3 retries
        for delay in schedule:
            if delay > 0:
                await asyncio.sleep(delay)
            try:
                return await self._call_get(name)
            except ApiException as exc:
                status = int(exc.status or 0)
                if status == 404:
                    continue
                if status in _TERMINAL_HTTP:
                    log.error(
                        "nhd_read_terminal", nhd_name=name, http_status=status
                    )
                    return None
                # Other 5xx / network — fall through to retry once via the
                # outer "other" path. We model that by trying again after
                # `other_backoff` and giving up if that also fails.
                await asyncio.sleep(self._other_backoff)
                try:
                    return await self._call_get(name)
                except Exception:  # noqa: BLE001
                    return None
            except Exception:  # noqa: BLE001
                # Network error → retry once at other_backoff.
                await asyncio.sleep(self._other_backoff)
                try:
                    return await self._call_get(name)
                except Exception:  # noqa: BLE001
                    return None
        return None

    async def _read_with_other_retry(self, name: str) -> dict[str, Any] | None:
        try:
            return await self._call_get(name)
        except Exception:  # noqa: BLE001
            await asyncio.sleep(self._other_backoff)
            try:
                return await self._call_get(name)
            except Exception:  # noqa: BLE001
                return None

    @staticmethod
    def _build_status_body(
        existing: dict[str, Any],
        status_diagnosis: dict[str, Any],
        final_phase: str,
        failure_reason: str | None,
    ) -> dict[str, Any]:
        """Compose the full body for ``replace_namespaced_custom_object_status``.

        Replace-style write needs the full object including
        ``metadata.resourceVersion``; we copy the existing object and only
        replace ``status``.
        """
        body = dict(existing)
        # Preserve metadata (apiserver uses resourceVersion for optimistic
        # concurrency on the status subresource).
        body["metadata"] = dict(existing.get("metadata", {}))
        prior_status = dict(existing.get("status") or {})

        condition_status: Literal["True", "False"]
        condition_reason: str
        condition_message: str
        if final_phase == "Diagnosed":
            condition_status = "True"
            condition_reason = "EvidenceValid"
            condition_message = "diagnosis written by agent"
        else:
            condition_status = "False"
            condition_reason = failure_reason or "ToolError"
            condition_message = (
                f"agent terminated with reason={failure_reason or 'ToolError'}"
            )

        ready_condition = {
            "type": "ReportReady",
            "status": condition_status,
            "reason": condition_reason,
            "message": condition_message,
            "lastTransitionTime": _now_rfc3339(),
        }
        # Replace any prior ReportReady; keep other condition types intact.
        prior_conditions = [
            c for c in (prior_status.get("conditions") or []) if c.get("type") != "ReportReady"
        ]
        prior_conditions.append(ready_condition)

        new_status = dict(prior_status)
        new_status["phase"] = final_phase
        new_status["conditions"] = prior_conditions
        if final_phase == "Diagnosed":
            new_status["diagnosis"] = status_diagnosis

        body["status"] = new_status
        return body
