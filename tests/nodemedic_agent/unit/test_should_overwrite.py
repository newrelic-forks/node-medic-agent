"""Truth table for FR-12 phase-conflict guard (T016).

The agent overwrites only when ``status.phase`` is "" / "Pending" /
"Diagnosing"; everything else means another scope has finalised the case
(or is mid-finalisation) and the agent must defer.
"""
from __future__ import annotations

import pytest

from nodemedic_agent.kube.client import should_overwrite


@pytest.mark.parametrize("phase", ["", "Pending", "Diagnosing"])
def test_overwritable_phases_return_true(phase: str) -> None:
    assert should_overwrite(phase) is True


@pytest.mark.parametrize(
    "phase",
    [
        "Diagnosed",
        "Acted",
        "Failed",
        "Evaluating",
        "Evaluated",
    ],
)
def test_terminal_phases_defer(phase: str) -> None:
    assert should_overwrite(phase) is False


@pytest.mark.parametrize(
    "phase",
    [
        "diagnosed",  # case-sensitive
        "DIAGNOSING",
        "unknown",
        "garbage",
        " ",
    ],
)
def test_unrecognised_phases_fail_closed(phase: str) -> None:
    """Unknown phase → False (fail closed; never overwrite by accident)."""
    assert should_overwrite(phase) is False
