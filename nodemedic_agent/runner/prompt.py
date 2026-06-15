"""System / user prompt builders for the SDK loop (T055).

`load_runbook(path)` reads `prompts/runbook.md` once at module import and
caches the contents. The runbook is the agent's system prompt; the SDK /
CLI keeps the static prefix warm in Anthropic's prompt cache across
cases (the user prompt below is the only per-case piece).

`build_user_prompt(case)` produces the per-case user prompt with all the
``case.*`` fields the runbook decision trees reason about. Pure
function — no side effects, deterministic output for the same Case.
"""
from __future__ import annotations

import functools
from pathlib import Path

from nodemedic_agent.runner.case_table import Case


@functools.lru_cache(maxsize=4)
def load_runbook(path: str | Path) -> str:
    """Read the runbook from `path` (cached for the process lifetime)."""
    return Path(path).read_text(encoding="utf-8")


def build_user_prompt(case: Case) -> str:
    """Render the per-case user prompt.

    The runbook (system prompt) carries the dispatch logic. The user
    prompt carries the case body — same shape every time, just different
    field values, so the static prefix of the system prompt stays
    cache-warm across cases.
    """
    observed_at = case.trigger_observed_at.isoformat()
    return _USER_PROMPT_TEMPLATE.format(
        case_id=case.case_id,
        node_name=case.node_name,
        cluster_name=case.cluster_name,
        provider=case.provider,
        region=case.region,
        instance_id=case.instance_id,
        trigger_type=case.trigger_type,
        trigger_reason=case.trigger_reason,
        trigger_message=case.trigger_message or "(no message)",
        trigger_observed_at=observed_at,
    )


_USER_PROMPT_TEMPLATE = """\
A new node-health case has been opened. Diagnose it per the runbook.

case:
  caseId: {case_id}
  nodeName: {node_name}
  clusterName: {cluster_name}
  provider: {provider}
  region: {region}
  instanceId: {instance_id}
  trigger:
    type: {trigger_type}
    reason: {trigger_reason}
    message: {trigger_message}
    observedAt: {trigger_observed_at}

Begin by reading the trigger fields above and selecting the matching
decision-tree section in the runbook. Run probes, gather evidence, then
finalize with `mcp__nodemedic__emit_report` exactly once. Do not call
any tool after `emit_report`.
"""
