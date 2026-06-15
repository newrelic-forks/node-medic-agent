"""Structured stdout logging (NFR-3).

The agent emits JSON-per-line to stdout — that is the entire audit surface
for the hackathon (Constitution Article I — ephemeral audit log). Every log
line carries an RFC3339 ``ts``, a ``level``, and (when bound) a ``case_id``
that flows across ``await`` boundaries via ``contextvars`` so per-case
worker tasks don't have to thread it manually.

Usage::

    from nodemedic_agent.logging import bind_case_id, get_logger

    log = get_logger()
    log.info("startup_complete", listen_addr=":8080")

    async def run_case(case):
        with bind_case_id(case.case_id):
            log.info("case_started", node=case.node_name)
            await do_work()  # case_id propagates through await
"""
from __future__ import annotations

import contextlib
import contextvars
import logging
import sys
from typing import Iterator

import structlog
from structlog.types import EventDict, WrappedLogger

# ContextVar so async tasks inherit the bound case_id without explicit threading.
_CASE_ID: contextvars.ContextVar[str | None] = contextvars.ContextVar("nodemedic_case_id", default=None)


def _add_case_id(_logger: WrappedLogger, _name: str, event_dict: EventDict) -> EventDict:
    case_id = _CASE_ID.get()
    if case_id is not None and "case_id" not in event_dict:
        event_dict["case_id"] = case_id
    return event_dict


_LEVEL_MAP = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


def configure_logging(level: str = "info") -> None:
    """Initialise structlog. Idempotent — safe to call from tests."""
    level_int = _LEVEL_MAP.get(level.lower(), logging.INFO)

    # Stdlib logging mirrors structlog's level so anything that imports
    # logging directly (uvicorn, kubernetes) ends up on the same stream.
    logging.basicConfig(
        level=level_int,
        stream=sys.stdout,
        format="%(message)s",
        force=True,
    )

    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            _add_case_id,
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso", utc=True, key="ts"),
            structlog.processors.StackInfoRenderer(),
            structlog.processors.format_exc_info,
            structlog.processors.JSONRenderer(sort_keys=True),
        ],
        wrapper_class=structlog.make_filtering_bound_logger(level_int),
        context_class=dict,
        logger_factory=structlog.PrintLoggerFactory(file=sys.stdout),
        cache_logger_on_first_use=True,
    )


def get_logger(name: str | None = None) -> structlog.stdlib.BoundLogger:
    return structlog.get_logger(name) if name else structlog.get_logger()


@contextlib.contextmanager
def bind_case_id(case_id: str) -> Iterator[None]:
    """Bind ``case_id`` for the duration of the block (and any awaited code)."""
    token = _CASE_ID.set(case_id)
    try:
        yield
    finally:
        _CASE_ID.reset(token)


def current_case_id() -> str | None:
    """Read the currently-bound case_id (None if outside a `bind_case_id` block)."""
    return _CASE_ID.get()
