"""Unit tests for nodemedic_agent.cloud.azure_login.

The helper runs `az login --service-principal …` once at FastAPI
startup. We exercise the four important branches:

  - all four envs missing → skip silently, return False
  - login subprocess returns 0 → success, no `az account set` call
  - login + subscription set, both succeed → True
  - login subprocess returns non-zero → warn, return False
"""
from __future__ import annotations

from types import SimpleNamespace
from typing import Any, Sequence

import pytest

from nodemedic_agent.cloud import azure_login


def _settings(
    *,
    client_id: str = "",
    client_secret: str = "",
    tenant_id: str = "",
    subscription_id: str = "",
) -> Any:
    return SimpleNamespace(
        azure_client_id=client_id,
        azure_client_secret=client_secret,
        azure_tenant_id=tenant_id,
        azure_subscription_id=subscription_id,
    )


@pytest.mark.asyncio
async def test_skip_when_creds_unset(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[Sequence[str]] = []

    async def fake_run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
        calls.append(tuple(cmd))
        return 0, ""

    monkeypatch.setattr(azure_login, "_run", fake_run)

    ok = await azure_login.login_if_configured(_settings())
    assert ok is False
    assert calls == []  # no subprocess fired


@pytest.mark.asyncio
async def test_login_only_when_subscription_empty(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[Sequence[str]] = []

    async def fake_run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
        calls.append(tuple(cmd))
        return 0, ""

    monkeypatch.setattr(azure_login, "_run", fake_run)

    ok = await azure_login.login_if_configured(
        _settings(client_id="cid", client_secret="sec", tenant_id="tid")
    )
    assert ok is True
    assert len(calls) == 1
    cmd = calls[0]
    assert cmd[0:3] == ("az", "login", "--service-principal")
    assert "--username" in cmd and "cid" in cmd
    assert "--password" in cmd and "sec" in cmd
    assert "--tenant" in cmd and "tid" in cmd
    # Defensive: secret never appears in any other position than as the value.
    assert cmd.index("sec") == cmd.index("--password") + 1


@pytest.mark.asyncio
async def test_login_then_account_set_when_subscription_present(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[Sequence[str]] = []

    async def fake_run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
        calls.append(tuple(cmd))
        return 0, ""

    monkeypatch.setattr(azure_login, "_run", fake_run)

    ok = await azure_login.login_if_configured(
        _settings(
            client_id="cid",
            client_secret="sec",
            tenant_id="tid",
            subscription_id="sub-uuid",
        )
    )
    assert ok is True
    assert len(calls) == 2
    assert calls[1] == ("az", "account", "set", "--subscription", "sub-uuid")


@pytest.mark.asyncio
async def test_login_failure_returns_false_no_account_set(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls: list[Sequence[str]] = []

    async def fake_run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
        calls.append(tuple(cmd))
        return 2, "AADSTS70002: Error validating credentials."

    monkeypatch.setattr(azure_login, "_run", fake_run)

    ok = await azure_login.login_if_configured(
        _settings(
            client_id="cid",
            client_secret="bad",
            tenant_id="tid",
            subscription_id="sub-uuid",
        )
    )
    assert ok is False
    # Login attempted, but no `az account set` because login failed.
    assert len(calls) == 1


@pytest.mark.asyncio
async def test_account_set_failure_still_returns_true(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Login succeeded but subscription wasn't applied — model can fix on
    first probe via `az account set`. Don't black-mark the agent."""
    rcs = iter([0, 1])
    stderrs = iter(["", "subscription not found"])

    async def fake_run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
        return next(rcs), next(stderrs)

    monkeypatch.setattr(azure_login, "_run", fake_run)

    ok = await azure_login.login_if_configured(
        _settings(
            client_id="cid",
            client_secret="sec",
            tenant_id="tid",
            subscription_id="bad-sub",
        )
    )
    assert ok is True


def test_redact_tail_keeps_short_prefix() -> None:
    redacted = azure_login._redact_tail("aa4bcab1-aee1-4358-95b3-f17e51e44270")
    assert redacted.startswith("aa4bcab1")
    assert "..." in redacted
    assert "(36 chars)" in redacted
    assert azure_login._redact_tail("") == ""
