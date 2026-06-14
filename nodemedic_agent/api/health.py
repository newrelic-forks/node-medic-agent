"""GET /healthz + /readyz handlers (FR-2).

`/healthz` is trivial — 200 if the process is up.
`/readyz` returns 200 only after model-resolution succeeds AND the NR MCP
URL responds to a HEAD probe within `READYZ_PROBE_TIMEOUT_SEC`. Result is
cached for 30 s so repeated kubelet probes don't hammer the gateway.
"""
from __future__ import annotations

import time
from typing import Any

import httpx
from fastapi import APIRouter, Request, Response

from nodemedic_agent.logging import get_logger

log = get_logger(__name__)

_READYZ_CACHE_TTL_SEC = 30.0


def build_health_router() -> APIRouter:
    return APIRouter()


async def handle_healthz() -> Response:
    return _json(200, {"status": "ok"})


async def handle_readyz(request: Request) -> Response:
    state = request.app.state
    settings = state.settings
    cache: dict[str, Any] = state.readyz_cache
    now = time.monotonic()
    cached_until = cache.get("expires_at", 0.0)
    if cached_until > now:
        return cache["response"]

    timeout_sec: float = float(settings.readyz_probe_timeout_sec)

    # Resolve models lazily on first probe (model_resolver caches).
    resolver = state.model_resolver

    async with httpx.AsyncClient(timeout=timeout_sec) as client:
        resolved = await resolver.resolve(client)
        if resolved is None:
            response = _json(503, {"status": "no_usable_model"})
        else:
            try:
                head = await client.head(settings.nr_mcp_url)
                head.raise_for_status()
            except Exception as exc:  # noqa: BLE001
                log.warning("readyz_mcp_probe_failed", error=str(exc)[:256])
                response = _json(
                    503,
                    {
                        "status": "mcp_probe_failed",
                        "model_primary": resolved.primary_resolved,
                        "model_fallback": resolved.fallback_resolved,
                    },
                )
            else:
                response = _json(
                    200,
                    {
                        "status": "ok",
                        "model_primary": resolved.primary_resolved,
                        "model_fallback": resolved.fallback_resolved,
                    },
                )

    cache["expires_at"] = now + _READYZ_CACHE_TTL_SEC
    cache["response"] = response
    return response


def _json(status_code: int, payload: dict) -> Response:
    import json

    return Response(
        status_code=status_code,
        content=json.dumps(payload),
        media_type="application/json",
    )
