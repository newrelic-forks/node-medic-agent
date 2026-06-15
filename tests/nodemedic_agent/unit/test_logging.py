"""Unit tests for `nodemedic_agent.logging` (T013).

Verifies:
- JSON-per-line shape carries `ts`, `level`, and bound `case_id` keys (NFR-3).
- `case_id` propagates across `await` boundaries via `contextvars` (so case
  workers don't have to thread it manually).
"""
from __future__ import annotations

import asyncio
import json
import re

import pytest

from nodemedic_agent.logging import (
    bind_case_id,
    configure_logging,
    current_case_id,
    get_logger,
)


@pytest.fixture(autouse=True)
def _reset_logging() -> None:
    configure_logging("info")


def _read_json_lines(captured: str) -> list[dict]:
    return [json.loads(line) for line in captured.splitlines() if line.strip()]


# ---------------------------------------------------------------------------
# Shape
# ---------------------------------------------------------------------------


def test_log_line_is_json_with_required_keys(capsys: pytest.CaptureFixture[str]) -> None:
    log = get_logger()
    log.info("startup_complete", listen_addr=":8080")
    out = capsys.readouterr().out
    [entry] = _read_json_lines(out)

    assert entry["event"] == "startup_complete"
    assert entry["listen_addr"] == ":8080"
    assert entry["level"] == "info"
    # RFC3339 with timezone — structlog ISO format uses Z suffix.
    assert re.match(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}", entry["ts"])
    assert "case_id" not in entry  # nothing bound


def test_bind_case_id_attaches_to_log_lines(capsys: pytest.CaptureFixture[str]) -> None:
    log = get_logger()
    case_id = "8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c"
    with bind_case_id(case_id):
        log.info("case_started")
    out = capsys.readouterr().out
    [entry] = _read_json_lines(out)
    assert entry["case_id"] == case_id


def test_case_id_clears_after_block(capsys: pytest.CaptureFixture[str]) -> None:
    log = get_logger()
    with bind_case_id("8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c"):
        log.info("during")
    log.info("after")
    out = capsys.readouterr().out
    entries = _read_json_lines(out)
    assert len(entries) == 2
    assert "case_id" in entries[0]
    assert "case_id" not in entries[1]


def test_warn_and_error_levels_render(capsys: pytest.CaptureFixture[str]) -> None:
    log = get_logger()
    log.warning("retrying", attempt=2)
    log.error("terminal", reason="ToolError")
    entries = _read_json_lines(capsys.readouterr().out)
    levels = {e["level"] for e in entries}
    assert {"warning", "error"}.issubset(levels)


# ---------------------------------------------------------------------------
# contextvars propagation
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_case_id_propagates_across_await(capsys: pytest.CaptureFixture[str]) -> None:
    log = get_logger()

    async def inner() -> None:
        # Awaits before logging — the bound case_id must survive the suspension.
        await asyncio.sleep(0)
        log.info("from_inner")

    case_id = "deadbeef-dead-4ead-8ead-deadbeefdead"
    with bind_case_id(case_id):
        await inner()

    out = capsys.readouterr().out
    [entry] = _read_json_lines(out)
    assert entry["case_id"] == case_id
    assert entry["event"] == "from_inner"


@pytest.mark.asyncio
async def test_concurrent_tasks_have_isolated_case_ids(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """Two tasks bind distinct case_ids; their log lines stay disjoint (FR-3)."""
    log = get_logger()
    case_a = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
    case_b = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

    async def worker(case_id: str, label: str) -> None:
        with bind_case_id(case_id):
            await asyncio.sleep(0)
            log.info(label)

    await asyncio.gather(worker(case_a, "task_a"), worker(case_b, "task_b"))

    entries = _read_json_lines(capsys.readouterr().out)
    by_event = {e["event"]: e for e in entries}
    assert by_event["task_a"]["case_id"] == case_a
    assert by_event["task_b"]["case_id"] == case_b


def test_current_case_id_outside_block_returns_none() -> None:
    assert current_case_id() is None


def test_current_case_id_inside_block_returns_value() -> None:
    case_id = "feedface-feed-4ace-8ace-feedfacefeed"
    with bind_case_id(case_id):
        assert current_case_id() == case_id
    assert current_case_id() is None
