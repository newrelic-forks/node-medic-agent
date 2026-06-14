"""Startup model resolution against the nerd-completion gateway (FR-3).

Walks the exact → best-Opus → best-Sonnet → fail chain documented in
data-model.md §6. Caches the result for the process lifetime; `/readyz`
re-checks lazily by calling `resolve(...)` again — the resolver returns
the cached value if it already succeeded once.
"""
from __future__ import annotations

import asyncio
import re
from datetime import datetime, timezone
from typing import Literal, Optional

import httpx
from pydantic import BaseModel, ConfigDict

from nodemedic_agent.config import Settings
from nodemedic_agent.logging import get_logger

log = get_logger(__name__)


_OPUS_RE = re.compile(r"claude-opus-(\d+)[\.-]?(\d+)?", re.IGNORECASE)
_SONNET_RE = re.compile(r"claude-sonnet-(\d+)[\.-]?(\d+)?", re.IGNORECASE)


class ResolvedModels(BaseModel):
    model_config = ConfigDict(arbitrary_types_allowed=True)

    primary_requested: str
    primary_resolved: str
    fallback_requested: str
    fallback_resolved: str
    resolution_path: Literal["exact", "best-opus", "best-sonnet"]
    resolved_at: datetime


class ModelResolver:
    """Per-process model resolver. Caches the result on first success."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._cached: ResolvedModels | None = None
        self._lock = asyncio.Lock()

    @property
    def cached(self) -> ResolvedModels | None:
        return self._cached

    async def resolve(self, client: httpx.AsyncClient) -> ResolvedModels | None:
        async with self._lock:
            if self._cached is not None:
                return self._cached
            catalog = await _fetch_catalog(client, self._settings)
            primary, primary_path = _choose(
                requested=self._settings.claude_model,
                catalog=catalog,
                family_re=_OPUS_RE,
            )
            fallback, _fallback_path = _choose(
                requested=self._settings.claude_fallback_model,
                catalog=catalog,
                family_re=_SONNET_RE,
            )
            if primary is None or fallback is None:
                log.error(
                    "model_resolution_failed",
                    primary_requested=self._settings.claude_model,
                    fallback_requested=self._settings.claude_fallback_model,
                    catalog=catalog,
                )
                return None
            resolved = ResolvedModels(
                primary_requested=self._settings.claude_model,
                primary_resolved=primary,
                fallback_requested=self._settings.claude_fallback_model,
                fallback_resolved=fallback,
                resolution_path=primary_path,
                resolved_at=datetime.now(timezone.utc),
            )
            self._cached = resolved
            log.info(
                "model_resolved",
                primary=resolved.primary_resolved,
                fallback=resolved.fallback_resolved,
                resolution_path=resolved.resolution_path,
            )
            return resolved


async def _fetch_catalog(client: httpx.AsyncClient, settings: Settings) -> list[str]:
    """Read the gateway's model catalog. Returns a list of model IDs.

    On 404 (catalog endpoint not implemented) returns an empty list — the
    chooser then falls through to the family heuristic, and `/readyz` will
    fail loudly if no model can be picked.
    """
    url = settings.anthropic_base_url.rstrip("/") + "/v1/models"
    try:
        response = await client.get(
            url,
            headers={"Authorization": f"Bearer {settings.anthropic_auth_token}"},
        )
    except Exception as exc:  # noqa: BLE001
        log.warning("model_catalog_unreachable", url=url, error=str(exc)[:256])
        return []
    if response.status_code == 404:
        return []
    try:
        response.raise_for_status()
    except httpx.HTTPStatusError as exc:
        log.warning(
            "model_catalog_error",
            url=url,
            http_status=exc.response.status_code,
        )
        return []
    payload = response.json()
    items = payload.get("data") if isinstance(payload, dict) else payload
    if not isinstance(items, list):
        return []
    ids: list[str] = []
    for item in items:
        if isinstance(item, dict) and "id" in item:
            ids.append(str(item["id"]))
        elif isinstance(item, str):
            ids.append(item)
    return ids


def _choose(
    requested: str,
    catalog: list[str],
    family_re: re.Pattern[str],
) -> tuple[Optional[str], Literal["exact", "best-opus", "best-sonnet"]]:
    """Resolve a single model ID against the catalog."""
    label: Literal["exact", "best-opus", "best-sonnet"] = (
        "best-opus" if family_re is _OPUS_RE else "best-sonnet"
    )
    if requested in catalog:
        return requested, "exact"
    candidates = [c for c in catalog if family_re.search(c)]
    if not candidates:
        return None, label

    def _version_key(model_id: str) -> tuple[int, int]:
        m = family_re.search(model_id)
        if not m:
            return (-1, -1)
        major = int(m.group(1))
        minor = int(m.group(2)) if m.group(2) else 0
        return (major, minor)

    best = sorted(candidates, key=_version_key, reverse=True)[0]
    return best, label
