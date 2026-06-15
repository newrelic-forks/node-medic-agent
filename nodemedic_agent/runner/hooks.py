"""Pre/PostToolUse hooks (T056).

Constitution Article I.4 — every tool call observable. A single
``tool_log_hook`` callable is registered for both ``PreToolUse`` and
``PostToolUse`` via the SDK's ``HookMatcher``. On Pre it emits a
``tool_pre`` line with the command/args; on Post it pairs the call by
``tool_use_id`` and emits a ``tool_post`` line with latency and (for
Bash) exit code + byte counts. NFR-3 stdout JSON shape.

The hook never blocks tool execution — returning ``{}`` lets the SDK
proceed without injecting decisions.

Truncation cap is 2 KB on the captured `command` / `args` per
data-model.md §5. The original tool input is NOT modified — the
hook only logs.
"""
from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Coroutine

from nodemedic_agent.logging import get_logger

log = get_logger(__name__)

# Hard cap on per-line command/args size in stdout structured logs.
LOG_FIELD_LIMIT = 2048


# ---------------------------------------------------------------------------
# Per-process state for Pre↔Post pairing
# ---------------------------------------------------------------------------


@dataclass
class ToolHookState:
    """Tracks Pre↔Post pairing by tool_use_id within a single process.

    A single instance covers all concurrent cases — pairings are unique
    per `tool_use_id`, which the SDK guarantees globally unique across
    cases. Memory grows by one entry per active in-flight tool call;
    Post drops the entry.
    """

    pending: dict[str, float] = field(default_factory=dict)


# Type alias matching the SDK's HookCallback signature.
HookFn = Callable[
    [dict[str, Any], str | None, dict[str, Any]],
    Coroutine[Any, Any, dict[str, Any]],
]


# ---------------------------------------------------------------------------
# Factory
# ---------------------------------------------------------------------------


def make_tool_log_hook(state: ToolHookState | None = None) -> HookFn:
    """Build the per-process hook callable.

    Returns a coroutine matching the SDK's ``HookCallback`` signature:
    ``(input, tool_use_id, context) -> HookJSONOutput``.
    """
    state = state or ToolHookState()

    async def tool_log_hook(
        hook_input: dict[str, Any],
        tool_use_id: str | None,
        _context: dict[str, Any],
    ) -> dict[str, Any]:
        event = hook_input.get("hook_event_name")
        if event == "PreToolUse":
            _emit_pre(state, hook_input, tool_use_id)
        elif event == "PostToolUse":
            _emit_post(state, hook_input, tool_use_id)
        # Ignore other events; hook is logging-only.
        return {}

    return tool_log_hook


# ---------------------------------------------------------------------------
# Pre / Post emitters
# ---------------------------------------------------------------------------


def _emit_pre(
    state: ToolHookState,
    hook_input: dict[str, Any],
    tool_use_id: str | None,
) -> None:
    tool_name = hook_input.get("tool_name", "")
    tool_input = hook_input.get("tool_input") or {}
    fields = {
        "tool": tool_name,
        "tool_use_id": tool_use_id or hook_input.get("tool_use_id"),
    }

    if tool_name == "Bash":
        command = tool_input.get("command", "")
        if not isinstance(command, str):
            command = str(command)
        fields["command"] = _truncate(command, LOG_FIELD_LIMIT)
    else:
        fields["args"] = _truncate_args(tool_input)

    if tool_use_id:
        state.pending[tool_use_id] = time.perf_counter()

    log.info("tool_pre", **fields)


def _emit_post(
    state: ToolHookState,
    hook_input: dict[str, Any],
    tool_use_id: str | None,
) -> None:
    tool_name = hook_input.get("tool_name", "")
    use_id = tool_use_id or hook_input.get("tool_use_id")
    fields: dict[str, Any] = {
        "tool": tool_name,
        "tool_use_id": use_id,
    }
    started = state.pending.pop(use_id, None) if use_id else None
    if started is not None:
        fields["latency_ms"] = max(0, int((time.perf_counter() - started) * 1000))

    if tool_name == "Bash":
        response = hook_input.get("tool_response") or {}
        # Bash response shape per SDK: {stdout, stderr, exit_code, interrupted}
        if isinstance(response, dict):
            exit_code = response.get("exit_code")
            stdout = response.get("stdout", "")
            stderr = response.get("stderr", "")
            if exit_code is not None:
                fields["exit_code"] = exit_code
            if isinstance(stdout, str):
                fields["stdout_bytes"] = len(stdout.encode("utf-8"))
            if isinstance(stderr, str):
                fields["stderr_bytes"] = len(stderr.encode("utf-8"))
            if response.get("interrupted"):
                fields["interrupted"] = True

    # Pick log level based on exit_code for Bash; INFO otherwise.
    level = "info"
    exit_code = fields.get("exit_code")
    if isinstance(exit_code, int) and exit_code != 0:
        level = "warn"
    getattr(log, "warning" if level == "warn" else "info")("tool_post", **fields)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _truncate(s: str, limit: int) -> str:
    return s if len(s) <= limit else s[:limit]


def _truncate_args(args: Any) -> Any:
    """Serialize args to a string and truncate to the log-field limit.

    The original args object is not mutated. A JSON-stringified shape is
    a stable contract for `kubectl logs … | jq` consumers.
    """
    try:
        rendered = json.dumps(args, default=str, sort_keys=True)
    except (TypeError, ValueError):
        rendered = str(args)
    return _truncate(rendered, LOG_FIELD_LIMIT)
