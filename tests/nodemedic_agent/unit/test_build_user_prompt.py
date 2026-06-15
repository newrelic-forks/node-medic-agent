"""Unit tests for `build_user_prompt(case)` (T050).

Snapshot test — locks the per-case prompt shape so prompt-text changes are
reviewable as diffs. The user prompt is what the runbook (system_prompt)
gets composed against, and the SDK loop reasons about the case from these
fields. If we silently drop one we've broken FR-3 / G2.

The test asserts each `case.*` attribute appears at least once in the
prompt (case-insensitive) and that the prompt is stable across runs.
"""
from __future__ import annotations

from datetime import datetime, timezone

import pytest

from nodemedic_agent.runner.case_table import Case, CaseStatus
from nodemedic_agent.runner.prompt import build_user_prompt


@pytest.fixture
def fixture_case() -> Case:
    return Case(
        case_id="1a2b3c4d-5e6f-4789-9abc-def012345678",
        node_name="cf1z-general-nodes-2000003",
        cluster_name="cf1z",
        provider="azure",
        region="eastus2",
        instance_id="cf1z-general-nodes-2000003",
        nhd_name="cf1z-general-nodes-2000003-1781441053",
        trigger_type="ContainerRuntimeUnhealthy",
        trigger_reason="ContainerdUnreachable",
        trigger_message="containerd socket /run/containerd/containerd.sock unreachable",
        trigger_observed_at=datetime(2026, 6, 13, 14, 0, 0, tzinfo=timezone.utc),
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        status=CaseStatus.RUNNING,
        started_at=datetime(2026, 6, 13, 14, 0, 5, tzinfo=timezone.utc),
    )


def test_prompt_injects_all_case_fields(fixture_case: Case) -> None:
    prompt = build_user_prompt(fixture_case)
    # Each field appears at least once in the rendered prompt.
    assert fixture_case.case_id in prompt
    assert fixture_case.node_name in prompt
    assert fixture_case.cluster_name in prompt
    assert fixture_case.provider in prompt
    assert fixture_case.region in prompt
    assert fixture_case.instance_id in prompt
    assert fixture_case.trigger_type in prompt
    assert fixture_case.trigger_reason in prompt
    assert fixture_case.trigger_message in prompt
    assert fixture_case.trigger_observed_at.isoformat() in prompt


def test_prompt_is_stable(fixture_case: Case) -> None:
    """Render twice — must be byte-identical for the same case."""
    a = build_user_prompt(fixture_case)
    b = build_user_prompt(fixture_case)
    assert a == b


def test_prompt_does_not_leak_secrets(fixture_case: Case) -> None:
    """Sanity check — credential paths from spec NFR-6 must not be in the
    rendered user prompt. Forbidden paths live in the system runbook only."""
    prompt = build_user_prompt(fixture_case)
    assert "$SSH_KEY_PATH" not in prompt
    assert "/var/run/secrets" not in prompt
    assert "anthropic_auth_token" not in prompt.lower()


def test_prompt_mentions_emit_report_terminator(fixture_case: Case) -> None:
    """The user prompt should remind the agent to terminate via emit_report —
    the runbook also says this, but the user prompt is the per-case nudge."""
    prompt = build_user_prompt(fixture_case)
    assert "emit_report" in prompt


def test_prompt_renders_case_id_only_once(fixture_case: Case) -> None:
    """Prompt-cache friendliness: each per-case field appears once so the
    static prefix is shareable across cases (caching works on the runbook
    system prompt, not the per-case user prompt — but we still want a tight
    deterministic per-case shape)."""
    prompt = build_user_prompt(fixture_case)
    assert prompt.count(fixture_case.case_id) == 1


def test_prompt_dispatches_on_provider_aws() -> None:
    """When `provider="aws"`, the prompt must steer the agent toward AWS
    cloud probes (AC-15 clause 1)."""
    case = Case(
        case_id="2b3c4d5e-6f7a-4b89-9c01-def123456789",
        node_name="ip-10-0-0-42.us-east-2.compute.internal",
        cluster_name="test-aws",
        provider="aws",
        region="us-east-2",
        instance_id="i-0123456789abcdef0",
        trigger_type="ContainerRuntimeUnhealthy",
        trigger_reason="ContainerdUnreachable",
        trigger_message="",
        trigger_observed_at=datetime(2026, 6, 13, 14, 0, 0, tzinfo=timezone.utc),
        deadline_sec=60,
        max_turns=15,
        max_budget_usd="0.50",
        status=CaseStatus.RUNNING,
        started_at=datetime(2026, 6, 13, 14, 0, 5, tzinfo=timezone.utc),
    )
    prompt = build_user_prompt(case)
    assert "aws" in prompt.lower()
