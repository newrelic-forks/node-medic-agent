"""Case worker — stub (T036) and real SDK loop (T057).

Two variants live side-by-side. ``run_case_stub`` is the Day-1 AM
deliverable per Constitution Article II.3 (sleep 30 s + canned
diagnosis). ``run_case_real`` is the headline-win SDK loop that the
Phase 4 demo runs against ``chaos-containerd-unhealthy``.

The dispatch lives in ``run_case``: when ``Settings.stub_agent`` is True
it routes to the stub (used by Phase 3 deploys + the integration test
suite); when False it routes to the real loop (Phase 4 cf1z deploy).

Real-loop terminal-outcome → FR-11 reason mapping:

  emit_report returned WRITTEN              → case_complete{
      write_outcome="written", final_phase="Diagnosed"}
  emit_report returned DEFERRED_PHASE_CONFLICT → case_complete{
      write_outcome="deferred_phase_conflict",
      observed_phase=<phase>}  (FR-12; agent does NOT self-Failed)
  emit_report returned WRITE_FAILED         → write Failed CR
      (best-effort) with reason=CRWriteFailed; emit case_complete{
      write_outcome="write_failed",
      failure_reason="CRWriteFailed"}
  Model halts WITHOUT calling emit_report   → write Failed CR with
      reason=ModelHalted; emit matching case_complete
  Anthropic API exception (5xx, transport)  → write Failed CR with
      reason=ModelError
  SDK tool-runtime exception                → write Failed CR with
      reason=ToolError

No agent-side termination ceiling per FR-7 / Constitution I —
no max_turns, no max_budget_usd, no wall-clock cap.
"""
from __future__ import annotations

import asyncio
from datetime import datetime, timezone
from typing import Any

import httpx

from nodemedic_agent.kube.nhd_writer import NHDWriter, WriteOutcome
from nodemedic_agent.logging import bind_case_id, get_logger
from nodemedic_agent.runner.case_table import Case, CaseTable
from nodemedic_agent.runner.hooks import ToolHookState, make_tool_log_hook
from nodemedic_agent.runner.model_resolver import ModelResolver, ResolvedModels
from nodemedic_agent.runner.nr_mcp import nr_mcp_config
from nodemedic_agent.runner.prompt import build_user_prompt, load_runbook
from nodemedic_agent.tools.emit_report import make_emit_report

log = get_logger(__name__)

STUB_SLEEP_SEC = 30.0


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _now_rfc3339() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def _resolve_model_id(state: Any) -> str:
    resolver = state.model_resolver
    cached = getattr(resolver, "cached", None)
    if cached is not None:
        return cached.primary_resolved
    return state.settings.claude_model


async def _resolve_nhd_name(case: Case, writer: NHDWriter) -> str | None:
    if case.nhd_name:
        return case.nhd_name
    return await writer.find_by_case_id(case.case_id)


# ---------------------------------------------------------------------------
# Top-level dispatcher (selects stub or real based on settings)
# ---------------------------------------------------------------------------


async def run_case(case: Case, state: Any) -> None:
    """Dispatch based on ``state.settings.stub_agent`` flag (T058)."""
    if state.settings.stub_agent:
        await run_case_stub(case, state)
        return
    await run_case_real(case, state)


# ---------------------------------------------------------------------------
# Stub variant (T036, kept for tests + Phase 3 fallback)
# ---------------------------------------------------------------------------


async def run_case_stub(case: Case, state: Any) -> None:
    """Day-1 AM stub diagnosis (research R-11)."""
    case_table: CaseTable = state.case_table
    writer: NHDWriter = state.nhd_writer

    with bind_case_id(case.case_id):
        log.info(
            "case_started",
            mode="stub",
            node_name=case.node_name,
            cluster_name=case.cluster_name,
            provider=case.provider,
        )
        await asyncio.sleep(STUB_SLEEP_SEC)

        nhd_name = await _resolve_nhd_name(case, writer)
        if nhd_name is None:
            log.error("case_no_nhd_name", reason="caseId not found in namespace")
            await case_table.mark_complete(
                case.case_id, "Failed", failure_reason="CRWriteFailed"
            )
            return

        diagnosis = _stub_diagnosis(case, model_used=_resolve_model_id(state))
        result = await writer.update_status(
            nhd_name,
            status_diagnosis=diagnosis,
            final_phase="Diagnosed",
        )

        if result.outcome is WriteOutcome.WRITTEN:
            await case_table.mark_complete(case.case_id, "Diagnosed")
            log.info(
                "case_complete",
                write_outcome="written",
                final_phase="Diagnosed",
                duration_ms=int(STUB_SLEEP_SEC * 1000),
            )
        elif result.outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT:
            await case_table.mark_complete(case.case_id, "Diagnosed")
            log.info(
                "case_complete",
                write_outcome="deferred_phase_conflict",
                observed_phase=result.observed_phase,
            )
        else:
            await case_table.mark_complete(
                case.case_id, "Failed", failure_reason="CRWriteFailed"
            )
            log.error(
                "case_complete",
                write_outcome="write_failed",
                failure_reason="CRWriteFailed",
                detail=result.detail,
            )


def _stub_diagnosis(case: Case, model_used: str) -> dict[str, Any]:
    now = _now_rfc3339()
    return {
        "rootCause": "Day-1 stub diagnosis for integration testing",
        "rcaCategory": "Unknown",
        "confidence": 0.85,
        "evidence": [
            {
                "source": "kubectl",
                "ref": f"kubectl get node {case.node_name} -o yaml",
                "result": "stub-evidence: condition observed",
                "observedAt": now,
            },
            {
                "source": "nrql",
                "ref": (
                    "SELECT count(*) FROM K8sNodeSample "
                    f"WHERE clusterName = '{case.cluster_name}' "
                    f"AND nodeName = '{case.node_name}' SINCE 15 minutes ago"
                ),
                "result": "stub-evidence: 1 sample",
                "observedAt": now,
            },
        ],
        "recommendation": {
            "action": "Cordon",
            "reason": "Day-1 stub diagnosis — controller may proceed with confidence gate",
        },
        "modelUsed": model_used,
        "completedAt": now,
    }


# ---------------------------------------------------------------------------
# Real variant — Claude Agent SDK loop (T057)
# ---------------------------------------------------------------------------


async def run_case_real(case: Case, state: Any) -> None:
    """SDK-driven autonomous diagnosis loop (Phase 4 / US2 headline)."""
    settings = state.settings
    case_table: CaseTable = state.case_table
    writer: NHDWriter = state.nhd_writer
    resolver: ModelResolver = state.model_resolver

    with bind_case_id(case.case_id):
        started_at = datetime.now(timezone.utc)
        log.info(
            "case_started",
            mode="real",
            node_name=case.node_name,
            cluster_name=case.cluster_name,
            provider=case.provider,
            trigger_type=case.trigger_type,
            trigger_reason=case.trigger_reason,
        )

        resolved = await _ensure_model_resolved(resolver, settings)
        model_used = (
            resolved.primary_resolved if resolved is not None else settings.claude_model
        )
        fallback_model = (
            resolved.fallback_resolved
            if resolved is not None
            else settings.claude_fallback_model
        )

        # Resolve NHD name up-front so the tool factory can close over it
        # without paying the list+filter cost on every tool invocation.
        nhd_name = await _resolve_nhd_name(case, writer)
        if nhd_name is None:
            await _terminal_failure(
                case,
                state,
                started_at,
                model_used,
                reason="CRWriteFailed",
                detail=f"NHD with caseId={case.case_id} not found",
                attempt_failed_write=False,
            )
            return

        # Outcome sink the tool writes after a successful CR write so the
        # runner can map FR-11 reasons without re-parsing the message
        # stream for the tool result.
        outcome_sink: dict[str, Any] = {
            "emitted": False,
            "outcome": None,
            "observed_phase": None,
            "detail": None,
        }

        try:
            await _drive_sdk_loop(
                case=case,
                settings=settings,
                writer=writer,
                model_used=model_used,
                fallback_model=fallback_model,
                nhd_name=nhd_name,
                outcome_sink=outcome_sink,
            )
        except _ModelError as exc:
            await _terminal_failure(
                case,
                state,
                started_at,
                model_used,
                reason="ModelError",
                detail=str(exc)[:512],
                nhd_name=nhd_name,
            )
            return
        except _ToolError as exc:
            await _terminal_failure(
                case,
                state,
                started_at,
                model_used,
                reason="ToolError",
                detail=str(exc)[:512],
                nhd_name=nhd_name,
            )
            return
        except Exception as exc:  # noqa: BLE001 — surface unknown class as ToolError
            log.error("case_unhandled_loop_error", detail=str(exc)[:512])
            await _terminal_failure(
                case,
                state,
                started_at,
                model_used,
                reason="ToolError",
                detail=str(exc)[:512],
                nhd_name=nhd_name,
            )
            return

        await _emit_terminal_log(
            case=case,
            state=state,
            started_at=started_at,
            model_used=model_used,
            outcome_sink=outcome_sink,
            nhd_name=nhd_name,
        )


# ---------------------------------------------------------------------------
# SDK loop (factored out so the surrounding error handling reads cleanly)
# ---------------------------------------------------------------------------


# Internal exception classes so `_drive_sdk_loop` can communicate FR-11
# reasons up to `run_case_real` without polluting the SDK exception hierarchy.
class _ModelError(RuntimeError):
    pass


class _ToolError(RuntimeError):
    pass


async def _drive_sdk_loop(
    *,
    case: Case,
    settings: Any,
    writer: NHDWriter,
    model_used: str,
    fallback_model: str,
    nhd_name: str,
    outcome_sink: dict[str, Any],
) -> None:
    """Run the per-case ``ClaudeSDKClient`` loop to completion.

    Raises:
      _ModelError: Anthropic API exception (5xx, network, transport).
      _ToolError: SDK tool-runtime exception (Bash crash, MCP transport).
    """
    # Lazy import — keeps the stub path importable without the SDK on
    # PYTHONPATH (matters for unit-test environments).
    from claude_agent_sdk import (
        ClaudeAgentOptions,
        ClaudeSDKClient,
        HookMatcher,
    )
    from claude_agent_sdk._errors import (
        CLIConnectionError,
        ProcessError,
    )

    from nodemedic_agent.tools.emit_report import make_emit_report_server  # noqa: WPS433

    # Per-case MCP server with a fresh emit_report closure (G2 / FR-3).
    nodemedic_server = _make_per_case_server(
        case, writer, model_used=model_used, nhd_name=nhd_name, outcome_sink=outcome_sink
    )

    hook_state = ToolHookState()
    tool_log = make_tool_log_hook(hook_state)
    matcher = HookMatcher(hooks=[tool_log])

    runbook = load_runbook(settings.runbook_path)

    # Capture CLI stderr so we can debug from kubectl logs when the loop
    # halts without firing tool calls (saw this on cf1z first run — the
    # model would say "ok" without invoking emit_report). The callback
    # pipes each line through the structured logger so it joins the
    # case_id stream.
    def _stderr_logger(line: str) -> None:
        line = line.rstrip()
        if not line:
            return
        log.info("sdk_stderr", line=line[:1024])

    options = ClaudeAgentOptions(
        system_prompt=runbook,
        permission_mode="bypassPermissions",
        # Explicit tool list — same as allowed_tools below. Keeps the
        # bundled CLI from injecting Edit/Write/Skill/etc. that would
        # mislead the model into thinking it's coding when it should be
        # diagnosing.
        tools=[
            "Bash",
            "Read",
            "Glob",
            "Grep",
            "WebFetch",
            "WebSearch",
        ],
        mcp_servers={
            "nodemedic": nodemedic_server,
            "nr": nr_mcp_config(settings),
        },
        allowed_tools=[
            "mcp__nodemedic__emit_report",
            "mcp__nr__execute_nrql_query",
            "mcp__nr__list_recent_logs",
            "mcp__nr__analyze_entity_logs",
            "mcp__nr__analyze_golden_metrics",
            "mcp__nr__lookup_entity",
            "mcp__nr__get_entity",
            "Bash",
            "Read",
            "Glob",
            "Grep",
            "WebFetch",
            "WebSearch",
        ],
        hooks={
            "PreToolUse": [matcher],
            "PostToolUse": [matcher],
        },
        model=model_used,
        fallback_model=fallback_model,
        # ANTHROPIC_API_KEY is the form the bundled Claude Code CLI
        # reads (the SDK forwards env to the subprocess). The gateway
        # also accepts the same NCT- token via x-api-key — verified
        # empirically on cf1z 2026-06-14.
        env={
            "ANTHROPIC_BASE_URL": settings.anthropic_base_url,
            "ANTHROPIC_API_KEY": settings.anthropic_auth_token,
            "ANTHROPIC_AUTH_TOKEN": settings.anthropic_auth_token,
        },
        # SDK isolation mode — don't load CLAUDE.md, skills, agents, or
        # any other filesystem state from the agent pod. The runbook IS
        # the system prompt; nothing else should leak in.
        setting_sources=[],
        stderr=_stderr_logger,
    )

    user_prompt = build_user_prompt(case)

    try:
        async with ClaudeSDKClient(options=options) as client:
            await client.query(user_prompt)
            async for msg in client.receive_response():
                _trace_message(msg)
                if outcome_sink["emitted"]:
                    # The tool already wrote (or attempted to write) the CR.
                    # We continue draining the stream so the loop's
                    # ResultMessage closes the iterator naturally — the
                    # runbook's calling discipline forbids further tool
                    # calls after emit_report.
                    continue
    except (CLIConnectionError, ProcessError) as exc:
        raise _ModelError(f"SDK transport / process error: {exc}") from exc
    except httpx.HTTPError as exc:
        raise _ModelError(f"HTTP error during SDK loop: {exc}") from exc
    except asyncio.CancelledError:
        raise
    except Exception as exc:  # noqa: BLE001
        raise _ToolError(f"SDK runtime error: {exc}") from exc


def _make_per_case_server(
    case: Case,
    writer: NHDWriter,
    *,
    model_used: str,
    nhd_name: str,
    outcome_sink: dict[str, Any],
):
    """Build the per-case in-process MCP server hosting `emit_report`.

    Wraps `make_emit_report` + `create_sdk_mcp_server` so the runner can
    swap implementations under test without importing from
    `claude_agent_sdk` here directly.
    """
    from claude_agent_sdk import create_sdk_mcp_server

    tool = make_emit_report(
        case,
        writer,
        model_used=model_used,
        nhd_name=nhd_name,
        outcome_sink=outcome_sink,
    )
    return create_sdk_mcp_server(name="nodemedic", version="1.0.0", tools=[tool])


def _trace_message(msg: Any) -> None:
    """Best-effort message trace — useful for cf1z log inspection.

    The SDK's hook surface already covers per-tool-call logging (Article
    I.4); this is a coarse-grained trace so the case-complete picture is
    reconstructable without parsing the SDK's internal messages.
    """
    msg_type = type(msg).__name__
    log.debug("sdk_message", message_type=msg_type)


# ---------------------------------------------------------------------------
# Terminal-state helpers (write Failed CR + emit case_complete log)
# ---------------------------------------------------------------------------


async def _terminal_failure(
    case: Case,
    state: Any,
    started_at: datetime,
    model_used: str,
    *,
    reason: str,
    detail: str,
    nhd_name: str | None = None,
    attempt_failed_write: bool = True,
) -> None:
    """Best-effort write of a Failed CR + case_complete log line."""
    case_table: CaseTable = state.case_table
    writer: NHDWriter = state.nhd_writer

    written_failed = False
    if attempt_failed_write and nhd_name is not None:
        try:
            result = await writer.update_status(
                nhd_name,
                status_diagnosis={},
                final_phase="Failed",
                failure_reason=reason,
            )
            written_failed = result.outcome is WriteOutcome.WRITTEN
        except Exception as exc:  # noqa: BLE001
            log.warning("failed_cr_write_error", detail=str(exc)[:256])

    duration_ms = int((datetime.now(timezone.utc) - started_at).total_seconds() * 1000)

    if written_failed:
        await case_table.mark_complete(case.case_id, "Failed", failure_reason=reason)
        log.error(
            "case_complete",
            write_outcome="written",
            final_phase="Failed",
            failure_reason=reason,
            detail=detail,
            duration_ms=duration_ms,
            model_resolved=model_used,
        )
    else:
        await case_table.mark_complete(case.case_id, "Failed", failure_reason=reason)
        log.error(
            "case_complete",
            write_outcome="write_failed",
            failure_reason=reason,
            detail=detail,
            duration_ms=duration_ms,
            model_resolved=model_used,
        )


async def _emit_terminal_log(
    *,
    case: Case,
    state: Any,
    started_at: datetime,
    model_used: str,
    outcome_sink: dict[str, Any],
    nhd_name: str,
) -> None:
    """After the SDK loop ends, emit the right case_complete shape."""
    case_table: CaseTable = state.case_table
    writer: NHDWriter = state.nhd_writer
    duration_ms = int((datetime.now(timezone.utc) - started_at).total_seconds() * 1000)

    if not outcome_sink["emitted"]:
        # Model halted without calling emit_report — write a Failed CR.
        await _terminal_failure(
            case,
            state,
            started_at,
            model_used,
            reason="ModelHalted",
            detail="model halted without invoking emit_report",
            nhd_name=nhd_name,
        )
        return

    outcome: WriteOutcome | None = outcome_sink["outcome"]
    if outcome is WriteOutcome.WRITTEN:
        await case_table.mark_complete(case.case_id, "Diagnosed")
        log.info(
            "case_complete",
            write_outcome="written",
            final_phase="Diagnosed",
            duration_ms=duration_ms,
            model_resolved=model_used,
        )
        return

    if outcome is WriteOutcome.DEFERRED_PHASE_CONFLICT:
        # Per FR-12 the agent does NOT self-Failed; the CR keeps whatever
        # phase the controller stamped. Mark the case complete locally so
        # idempotency works, but don't try to overwrite the CR.
        await case_table.mark_complete(case.case_id, "Diagnosed")
        log.info(
            "case_complete",
            write_outcome="deferred_phase_conflict",
            observed_phase=outcome_sink["observed_phase"],
            duration_ms=duration_ms,
            model_resolved=model_used,
        )
        return

    # WriteOutcome.WRITE_FAILED — emit_report attempted the write and the
    # apiserver said no. Try a best-effort Failed CR with reason
    # CRWriteFailed (the original write target was Diagnosed; we now mark
    # Failed so the controller doesn't keep waiting forever).
    await _terminal_failure(
        case,
        state,
        started_at,
        model_used,
        reason="CRWriteFailed",
        detail=outcome_sink.get("detail") or "apiserver rejected status write",
        nhd_name=nhd_name,
    )


async def _ensure_model_resolved(
    resolver: ModelResolver, settings: Any
) -> ResolvedModels | None:
    """Force model resolution before the SDK loop starts (FR-3)."""
    if resolver.cached is not None:
        return resolver.cached
    try:
        async with httpx.AsyncClient(
            timeout=settings.readyz_probe_timeout_sec
        ) as client:
            return await resolver.resolve(client)
    except Exception as exc:  # noqa: BLE001
        log.warning("model_resolve_failed", detail=str(exc)[:256])
        return None
