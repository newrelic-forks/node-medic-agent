"""Structural check for the `## KubeletUnhealthy` runbook section (T064).

The build-time lint at ``tests/nodemedic_agent/check_runbook.sh`` only
asserts the heading exists. This unit test pins the *content* requirement
that AC-3b depends on:

  - the section is non-trivial (≥30 lines under that heading)
  - it names the four probe primitives the chaos-kubelet path needs the
    model to know about:
      * ``journalctl -u kubelet`` (host-side log probe)
      * ``kubectl get events`` (kube-side event probe)
      * ``127.0.0.1:10248`` (kubelet healthz endpoint, the chaos REJECT target)
      * ``iptables`` (mechanism the slam-dunk diagnosis names)

The test is intentionally a string-grep — it does not parse markdown. If
the runbook is restructured in a future revision the failure message
points at the missing primitive, not the formatter.
"""
from __future__ import annotations

from pathlib import Path

import pytest

RUNBOOK_PATH = (
    Path(__file__).resolve().parents[3] / "prompts" / "runbook.md"
)

REQUIRED_PRIMITIVES = (
    "journalctl -u kubelet",
    "kubectl get events",
    "127.0.0.1:10248",
    "iptables",
)

MIN_SECTION_LINES = 30


def _kubelet_section(text: str) -> list[str]:
    """Return the lines under `## KubeletUnhealthy` up to the next H2."""
    lines = text.splitlines()
    start: int | None = None
    for i, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith("## ") and "KubeletUnhealthy" in stripped:
            start = i + 1
            break
    if start is None:
        return []
    end = len(lines)
    for j in range(start, len(lines)):
        if lines[j].startswith("## "):
            end = j
            break
    return lines[start:end]


@pytest.fixture(scope="module")
def runbook_text() -> str:
    assert RUNBOOK_PATH.exists(), f"runbook not found at {RUNBOOK_PATH}"
    return RUNBOOK_PATH.read_text()


def test_kubelet_section_present(runbook_text: str) -> None:
    section = _kubelet_section(runbook_text)
    assert section, "## KubeletUnhealthy heading missing from runbook"


def test_kubelet_section_is_non_trivial(runbook_text: str) -> None:
    section = _kubelet_section(runbook_text)
    non_blank = [ln for ln in section if ln.strip()]
    assert len(non_blank) >= MIN_SECTION_LINES, (
        f"## KubeletUnhealthy section has {len(non_blank)} non-blank lines, "
        f"expected ≥{MIN_SECTION_LINES}"
    )


@pytest.mark.parametrize("primitive", REQUIRED_PRIMITIVES)
def test_kubelet_section_mentions_primitive(
    runbook_text: str, primitive: str
) -> None:
    section_text = "\n".join(_kubelet_section(runbook_text))
    assert primitive in section_text, (
        f"## KubeletUnhealthy section does not mention {primitive!r}; "
        "AC-3b decision tree requires it"
    )
