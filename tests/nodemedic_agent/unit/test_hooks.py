"""Unit tests for `tool_log_hook` (T051).

Constitution Article I.4 — every tool call observable. The hook is wired
for both PreToolUse and PostToolUse via SDK's `HookMatcher`. Per data-model
§5 it emits a `ToolCallLog` line carrying:

  - `case_id` (from contextvars; T011 propagation)
  - `tool` name
  - `command` (Bash) or `args` (non-Bash), truncated to 2 KB
  - `latency_ms` on PostToolUse (computed from a per-tool_use_id timer)
  - `exit_code` + `stdout_bytes` + `stderr_bytes` for Bash on PostToolUse

The hook is async and adheres to the SDK's `HookCallback` signature
`(HookInput, tool_use_id, HookContext) -> HookJSONOutput`.

These tests use captured stdout (the structlog sink) since the runner
production path emits to stdout per NFR-3.
"""
from __future__ import annotations

import asyncio
import json
import re

import pytest

from nodemedic_agent.logging import bind_case_id, configure_logging
from nodemedic_agent.runner.hooks import (
    ToolHookState,
    make_tool_log_hook,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


@pytest.fixture(autouse=True)
def _logging_configured() -> None:
    configure_logging("info")


def _parse_log_lines(captured: str) -> list[dict]:
    out = []
    for line in captured.splitlines():
        line = line.strip()
        if not line or not line.startswith("{"):
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return out


def _events_named(lines: list[dict], name: str) -> list[dict]:
    return [l for l in lines if l.get("event") == name]


CASE_ID = "1a2b3c4d-5e6f-4789-9abc-def012345678"


def _pre_input(*, tool_name: str, tool_input: dict, tool_use_id: str) -> dict:
    return {
        "session_id": "session-1",
        "transcript_path": "/tmp/x.jsonl",
        "cwd": "/app",
        "hook_event_name": "PreToolUse",
        "tool_name": tool_name,
        "tool_input": tool_input,
        "tool_use_id": tool_use_id,
    }


def _post_input(
    *,
    tool_name: str,
    tool_input: dict,
    tool_response,
    tool_use_id: str,
) -> dict:
    return {
        "session_id": "session-1",
        "transcript_path": "/tmp/x.jsonl",
        "cwd": "/app",
        "hook_event_name": "PostToolUse",
        "tool_name": tool_name,
        "tool_input": tool_input,
        "tool_response": tool_response,
        "tool_use_id": tool_use_id,
    }


# ---------------------------------------------------------------------------
# Bash pre + post pairing
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_bash_pre_emits_command_and_case_id(
    capsys: pytest.CaptureFixture[str],
) -> None:
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    with bind_case_id(CASE_ID):
        await hook(
            _pre_input(
                tool_name="Bash",
                tool_input={"command": "kubectl get nodes -o name"},
                tool_use_id="tu-1",
            ),
            "tu-1",
            {"signal": None},
        )
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    pre_events = _events_named(lines, "tool_pre")
    assert len(pre_events) == 1
    pre = pre_events[0]
    assert pre["case_id"] == CASE_ID
    assert pre["tool"] == "Bash"
    assert pre["command"] == "kubectl get nodes -o name"
    assert pre["tool_use_id"] == "tu-1"


@pytest.mark.asyncio
async def test_bash_post_records_latency_and_exit_code(
    capsys: pytest.CaptureFixture[str],
) -> None:
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    with bind_case_id(CASE_ID):
        await hook(
            _pre_input(
                tool_name="Bash",
                tool_input={"command": "echo hi"},
                tool_use_id="tu-2",
            ),
            "tu-2",
            {"signal": None},
        )
        await asyncio.sleep(0.01)  # let latency advance past 0
        await hook(
            _post_input(
                tool_name="Bash",
                tool_input={"command": "echo hi"},
                tool_response={
                    "stdout": "hi\n",
                    "stderr": "",
                    "exit_code": 0,
                    "interrupted": False,
                },
                tool_use_id="tu-2",
            ),
            "tu-2",
            {"signal": None},
        )
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    posts = _events_named(lines, "tool_post")
    assert len(posts) == 1
    post = posts[0]
    assert post["case_id"] == CASE_ID
    assert post["tool"] == "Bash"
    assert post["exit_code"] == 0
    assert post["stdout_bytes"] == 3
    assert post["stderr_bytes"] == 0
    assert post["latency_ms"] >= 0
    assert post["tool_use_id"] == "tu-2"


# ---------------------------------------------------------------------------
# Non-Bash MCP tool: args (not command)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_non_bash_records_args_not_command(
    capsys: pytest.CaptureFixture[str],
) -> None:
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    args = {"nrql": "SELECT count(*) FROM K8sNodeSample", "account_id": 1}
    with bind_case_id(CASE_ID):
        await hook(
            _pre_input(
                tool_name="mcp__nr__execute_nrql_query",
                tool_input=args,
                tool_use_id="tu-3",
            ),
            "tu-3",
            {"signal": None},
        )
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    pre_events = _events_named(lines, "tool_pre")
    assert len(pre_events) == 1
    pre = pre_events[0]
    assert pre["tool"] == "mcp__nr__execute_nrql_query"
    assert "command" not in pre  # Bash-only field
    # `args` may be serialized as JSON string for log shape stability.
    assert pre["args"] is not None
    assert "K8sNodeSample" in json.dumps(pre["args"])


# ---------------------------------------------------------------------------
# Truncation at 2 KB
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_pre_command_truncated_at_2kb(
    capsys: pytest.CaptureFixture[str],
) -> None:
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    huge = "echo " + ("x" * 5000)
    with bind_case_id(CASE_ID):
        await hook(
            _pre_input(
                tool_name="Bash",
                tool_input={"command": huge},
                tool_use_id="tu-4",
            ),
            "tu-4",
            {"signal": None},
        )
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    pre_events = _events_named(lines, "tool_pre")
    assert len(pre_events) == 1
    pre = pre_events[0]
    assert len(pre["command"]) <= 2048


@pytest.mark.asyncio
async def test_pre_args_truncated_at_2kb(
    capsys: pytest.CaptureFixture[str],
) -> None:
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    huge_args = {"big": "y" * 5000}
    with bind_case_id(CASE_ID):
        await hook(
            _pre_input(
                tool_name="mcp__nr__execute_nrql_query",
                tool_input=huge_args,
                tool_use_id="tu-5",
            ),
            "tu-5",
            {"signal": None},
        )
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    pre = _events_named(lines, "tool_pre")[0]
    # args is serialized to a string for log shape; truncate to ≤ 2 KB.
    args_repr = pre["args"] if isinstance(pre["args"], str) else json.dumps(pre["args"])
    assert len(args_repr) <= 2048


# ---------------------------------------------------------------------------
# Case-id propagation (T011 / Article I.4)
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_case_id_propagates_across_await(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """Two cases run concurrently; each line carries the right case_id."""
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    case_a = "11111111-1111-4111-8111-111111111111"
    case_b = "22222222-2222-4222-8222-222222222222"

    async def case_run(cid: str, label: str) -> None:
        with bind_case_id(cid):
            await hook(
                _pre_input(
                    tool_name="Bash",
                    tool_input={"command": f"echo {label}"},
                    tool_use_id=f"tu-{label}",
                ),
                f"tu-{label}",
                {"signal": None},
            )
            await asyncio.sleep(0.005)

    await asyncio.gather(case_run(case_a, "a"), case_run(case_b, "b"))
    captured = capsys.readouterr()
    lines = _parse_log_lines(captured.out)
    pre_events = _events_named(lines, "tool_pre")
    a_lines = [l for l in pre_events if l.get("case_id") == case_a]
    b_lines = [l for l in pre_events if l.get("case_id") == case_b]
    assert len(a_lines) == 1 and len(b_lines) == 1
    assert "echo a" in a_lines[0]["command"]
    assert "echo b" in b_lines[0]["command"]


# ---------------------------------------------------------------------------
# Hook return shape — must satisfy SDK's HookJSONOutput
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_hook_returns_empty_dict_for_no_op() -> None:
    """Logging-only hook never blocks or modifies — returns ``{}`` so the
    SDK proceeds without injecting decisions."""
    state = ToolHookState()
    hook = make_tool_log_hook(state)
    with bind_case_id(CASE_ID):
        result = await hook(
            _pre_input(
                tool_name="Bash",
                tool_input={"command": "true"},
                tool_use_id="tu-6",
            ),
            "tu-6",
            {"signal": None},
        )
    assert result == {}
