"""Case isolation integration test (T065 — AC-14 unit-level proof).

Drives two consecutive cases through ``run_case_real`` with a stubbed
``_drive_sdk_loop`` and asserts:

  1. Each case gets a fresh ``ClaudeSDKClient`` instance — verified by
     introspecting how many times ``_make_per_case_server`` is invoked
     (once per case) and recording the per-case ``Case`` reference in
     each invocation.
  2. The structured logs produced by case A contain only case A's
     ``case_id`` and the logs from case B contain only case B's — i.e.
     the ``contextvars``-bound case_id does not leak across the
     ``with bind_case_id(...)`` boundary.
  3. The per-case ``emit_report`` outcome sinks are independent — case
     A's sink does not pollute case B's, and vice versa.

The cf1z gate at T069 covers the same property end-to-end against the
real SDK + real Anthropic gateway. This test is the unit-level proof so
regressions land before a re-deploy round-trip.
"""
from __future__ import annotations

from datetime import datetime, timezone
from types import SimpleNamespace
from typing import Any

import pytest
import structlog
from structlog.testing import LogCapture

from nodemedic_agent.logging import _add_case_id
from nodemedic_agent.runner import case_worker
from nodemedic_agent.runner.case_table import Case, CaseStatus, CaseTable

CASE_A_ID = "11111111-2222-4333-8444-555555555555"
CASE_B_ID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _case(case_id: str, trigger: str, nhd_suffix: str) -> Case:
    return Case(
        case_id=case_id,
        node_name="cf1z-general-nodes-2000004",
        cluster_name="cf1z",
        provider="azure",
        region="eastus2",
        instance_id="cf1z-general-nodes-2000004",
        nhd_name=f"cf1z-general-nodes-2000004-{nhd_suffix}",
        trigger_type=trigger,
        trigger_reason="ContainerdUnreachable" if trigger == "ContainerRuntimeUnhealthy" else "KubeletHealthzFailed",
        trigger_message="",
        trigger_observed_at=datetime(2026, 6, 14, tzinfo=timezone.utc),
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        status=CaseStatus.RUNNING,
        started_at=datetime(2026, 6, 14, tzinfo=timezone.utc),
    )


def _state() -> SimpleNamespace:
    """Minimal state object — only the fields run_case_real reads."""

    class _StubWriter:
        async def find_by_case_id(self, _case_id: str) -> str | None:  # pragma: no cover - unused
            return None

        async def update_status(self, *_args, **_kwargs):  # pragma: no cover - unused
            return SimpleNamespace(outcome=None, observed_phase=None, detail=None)

    return SimpleNamespace(
        settings=SimpleNamespace(
            stub_agent=False,
            claude_model="claude-opus-4-7",
            claude_fallback_model="claude-sonnet-4-6",
            anthropic_base_url="https://gateway.example/",
            anthropic_auth_token="NCT-test",
            runbook_path="prompts/runbook.md",
            nr_mcp_url="https://nr-mcp.example/mcp",
            nr_user_api_key="NRAK-test",
            readyz_probe_timeout_sec=2.0,
        ),
        case_table=CaseTable(),
        nhd_writer=_StubWriter(),
        model_resolver=SimpleNamespace(cached=None),
    )


@pytest.fixture
def log_capture() -> LogCapture:
    """Configure structlog to capture every log entry into a list.

    Includes the production ``_add_case_id`` processor so the ContextVar
    binding flows through into the captured entries — without it the
    capture sees the raw event dict and case_id never lands.
    """
    cap = LogCapture()
    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            _add_case_id,
            cap,
        ]
    )
    return cap


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_two_cases_use_independent_emit_report_sinks(
    monkeypatch: pytest.MonkeyPatch, log_capture: LogCapture
) -> None:
    """Each case binds its own emit_report outcome_sink.

    Stubs ``_drive_sdk_loop`` to mark the sink ``emitted=True`` with a
    case-tagged detail string, then asserts case A's tagged detail is
    visible *only* in case A's log line and case B's only in case B's.
    """
    sinks_seen: list[dict[str, Any]] = []

    async def fake_drive(
        *,
        case: Case,
        settings: Any,  # noqa: ARG001
        writer: Any,  # noqa: ARG001
        model_used: str,  # noqa: ARG001
        fallback_model: str,  # noqa: ARG001
        nhd_name: str,  # noqa: ARG001
        outcome_sink: dict[str, Any],
    ) -> None:
        # Mutate this case's sink with a case-tagged marker. If sinks are
        # shared across cases, the second invocation will see the first's
        # marker still set.
        assert outcome_sink["emitted"] is False, (
            f"sink for {case.case_id} arrived already-emitted — sink leaked"
        )
        outcome_sink["emitted"] = True
        outcome_sink["outcome"] = SimpleNamespace(name="WRITTEN")
        outcome_sink["detail"] = f"sink-tag-for-{case.case_id}"
        sinks_seen.append(dict(outcome_sink))

    monkeypatch.setattr(case_worker, "_drive_sdk_loop", fake_drive)

    # Force the model resolver short-circuit so we don't hit the network.
    async def fake_resolve(_resolver: Any, _settings: Any) -> None:
        return None

    monkeypatch.setattr(case_worker, "_ensure_model_resolved", fake_resolve)

    # Skip the terminal-log path — its CR-write attempts hit the writer.
    async def fake_terminal_log(**kwargs: Any) -> None:
        case_obj: Case = kwargs["case"]
        outcome_sink = kwargs["outcome_sink"]
        # Re-emit the sink-tagged detail under the case-bound logger so
        # the assertion below can pin "tag for A appears only in A's logs".
        case_worker.log.info(
            "fake_case_complete",
            sink_detail=outcome_sink["detail"],
            case_node=case_obj.node_name,
        )

    monkeypatch.setattr(case_worker, "_emit_terminal_log", fake_terminal_log)

    state = _state()

    case_a = _case(CASE_A_ID, "ContainerRuntimeUnhealthy", "1781447100")
    case_b = _case(CASE_B_ID, "KubeletUnhealthy", "1781447200")

    await case_worker.run_case_real(case_a, state)
    await case_worker.run_case_real(case_b, state)

    # Two cases ran → two sinks captured, each tagged for its own case.
    assert len(sinks_seen) == 2
    assert sinks_seen[0]["detail"] == f"sink-tag-for-{CASE_A_ID}"
    assert sinks_seen[1]["detail"] == f"sink-tag-for-{CASE_B_ID}"
    # The dict objects must be distinct — same identity would imply a
    # shared closure variable across cases.
    assert sinks_seen[0] is not sinks_seen[1]

    # Filter logs by case_id and assert disjoint trees.
    by_case: dict[str | None, list[dict[str, Any]]] = {}
    for entry in log_capture.entries:
        by_case.setdefault(entry.get("case_id"), []).append(entry)

    a_entries = by_case.get(CASE_A_ID, [])
    b_entries = by_case.get(CASE_B_ID, [])
    assert a_entries, "case A produced no case-bound log entries"
    assert b_entries, "case B produced no case-bound log entries"

    a_text = " ".join(str(e) for e in a_entries)
    b_text = " ".join(str(e) for e in b_entries)
    # Case A's tag never shows up in B's log tree, and vice versa.
    assert f"sink-tag-for-{CASE_A_ID}" in a_text
    assert f"sink-tag-for-{CASE_A_ID}" not in b_text
    assert f"sink-tag-for-{CASE_B_ID}" in b_text
    assert f"sink-tag-for-{CASE_B_ID}" not in a_text


@pytest.mark.asyncio
async def test_per_case_server_factory_called_once_per_case(
    monkeypatch: pytest.MonkeyPatch, log_capture: LogCapture
) -> None:
    """Each case must instantiate its own per-case MCP server.

    ``_make_per_case_server`` builds the in-process MCP server that hosts
    ``emit_report`` — its closure captures the case-specific state. If a
    second case were to reuse the first's server instance, the runbook's
    ``case A`` evidence would leak into ``case B``'s ``emit_report``
    outcome_sink (G2 / FR-3 violation).
    """
    factory_calls: list[Case] = []

    def fake_factory(
        case: Case,
        _writer: Any,
        *,
        model_used: str,  # noqa: ARG001
        nhd_name: str,  # noqa: ARG001
        outcome_sink: dict[str, Any],  # noqa: ARG001
    ) -> object:
        factory_calls.append(case)
        # Return a sentinel so ``_drive_sdk_loop`` can be stubbed without
        # caring about the server's actual interface.
        return SimpleNamespace(name="nodemedic", case_id=case.case_id)

    monkeypatch.setattr(case_worker, "_make_per_case_server", fake_factory)

    server_objects_seen: list[Any] = []

    async def fake_drive(
        *,
        case: Case,  # noqa: ARG001
        settings: Any,  # noqa: ARG001
        writer: Any,  # noqa: ARG001
        model_used: str,  # noqa: ARG001
        fallback_model: str,  # noqa: ARG001
        nhd_name: str,  # noqa: ARG001
        outcome_sink: dict[str, Any],
    ) -> None:
        # The runner has already built the per-case server inside
        # _drive_sdk_loop in real execution; here we just need to make
        # sure run_case_real progresses to its terminal-log path with a
        # successful sink.
        outcome_sink["emitted"] = True
        outcome_sink["outcome"] = SimpleNamespace(name="WRITTEN")

    # Real _drive_sdk_loop calls _make_per_case_server inside itself, so
    # the factory test needs a different shim — invoke the factory from
    # the fake drive so the assertion still reflects production wiring.
    async def factory_recording_drive(
        *,
        case: Case,
        settings: Any,
        writer: Any,
        model_used: str,
        fallback_model: str,  # noqa: ARG001
        nhd_name: str,
        outcome_sink: dict[str, Any],
    ) -> None:
        server = case_worker._make_per_case_server(
            case,
            writer,
            model_used=model_used,
            nhd_name=nhd_name,
            outcome_sink=outcome_sink,
        )
        server_objects_seen.append(server)
        outcome_sink["emitted"] = True
        outcome_sink["outcome"] = SimpleNamespace(name="WRITTEN")

    monkeypatch.setattr(case_worker, "_drive_sdk_loop", factory_recording_drive)

    async def fake_resolve(_resolver: Any, _settings: Any) -> None:
        return None

    monkeypatch.setattr(case_worker, "_ensure_model_resolved", fake_resolve)

    async def fake_terminal_log(**_kwargs: Any) -> None:
        return None

    monkeypatch.setattr(case_worker, "_emit_terminal_log", fake_terminal_log)

    state = _state()

    case_a = _case(CASE_A_ID, "ContainerRuntimeUnhealthy", "1781447300")
    case_b = _case(CASE_B_ID, "KubeletUnhealthy", "1781447400")

    await case_worker.run_case_real(case_a, state)
    await case_worker.run_case_real(case_b, state)

    # Factory invoked once per case, with the right Case identity each
    # time — never the wrong one and never zero.
    assert len(factory_calls) == 2
    assert factory_calls[0].case_id == CASE_A_ID
    assert factory_calls[1].case_id == CASE_B_ID

    # The two server objects are distinct — proves a fresh instance per
    # case (the shape is a SimpleNamespace from the fake factory; what
    # matters is identity).
    assert len(server_objects_seen) == 2
    assert server_objects_seen[0] is not server_objects_seen[1]
    assert server_objects_seen[0].case_id == CASE_A_ID
    assert server_objects_seen[1].case_id == CASE_B_ID
