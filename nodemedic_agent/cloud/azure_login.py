"""One-shot `az login --service-principal` at agent startup.

The `az` CLI does NOT honor `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` /
`AZURE_TENANT_ID` env vars on its own — those are Terraform / SDK
conventions, not CLI conventions. The CLI requires an explicit `az
login --service-principal …` to populate `~/.azure/`. After login,
subsequent `az ...` invocations from the SDK loop's `Bash` tool inherit
the cached token without the model having to authenticate.

We run this once at FastAPI startup. If the four env vars aren't all
set, we skip silently — Azure probes are optional, and AWS-only
deployments shouldn't be blocked. If the login attempt fails (bad
credentials, transient network), we log a warning and continue — the
runbook already tells the model to fall back to kubectl + ssh + NRQL
evidence when Azure probes return credential errors.

Subscription handling: if `AZURE_SUBSCRIPTION_ID` is set, we follow the
login with `az account set --subscription <id>` so probes default to
the right scope without the model running discovery on every case.
"""
from __future__ import annotations

import asyncio
import os
from typing import Sequence

from nodemedic_agent.config import Settings
from nodemedic_agent.logging import get_logger

log = get_logger(__name__)

LOGIN_TIMEOUT_SEC = 30.0
ACCOUNT_SET_TIMEOUT_SEC = 10.0


async def login_if_configured(settings: Settings) -> bool:
    """Run `az login` + optional `az account set` based on settings.

    Returns True if a login call ran and succeeded. Returns False if any
    of (a) creds aren't all set, (b) login failed, (c) `az` binary
    missing — the caller treats False as "Azure probes are not
    available; runbook fallback applies".
    """
    if not _all_set(
        settings.azure_client_id,
        settings.azure_client_secret,
        settings.azure_tenant_id,
    ):
        log.info(
            "azure_login_skipped",
            reason="one or more of AZURE_CLIENT_ID/CLIENT_SECRET/TENANT_ID is empty",
        )
        return False

    home = os.environ.get("HOME") or "/tmp"
    log.info(
        "azure_login_start",
        tenant=_redact_tail(settings.azure_tenant_id),
        client_id=_redact_tail(settings.azure_client_id),
        subscription=_redact_tail(settings.azure_subscription_id),
        home=home,
    )

    cmd = [
        "az",
        "login",
        "--service-principal",
        "--username", settings.azure_client_id,
        "--password", settings.azure_client_secret,
        "--tenant", settings.azure_tenant_id,
        "--allow-no-subscriptions",
        "--output", "none",
    ]
    rc, stderr = await _run(cmd, timeout=LOGIN_TIMEOUT_SEC)
    if rc != 0:
        log.warning(
            "azure_login_failed",
            return_code=rc,
            stderr=stderr[:512],
            hint="agent will continue; Azure probes may fail",
        )
        return False

    if settings.azure_subscription_id:
        rc, stderr = await _run(
            ["az", "account", "set", "--subscription", settings.azure_subscription_id],
            timeout=ACCOUNT_SET_TIMEOUT_SEC,
        )
        if rc != 0:
            log.warning(
                "azure_account_set_failed",
                return_code=rc,
                stderr=stderr[:512],
                subscription=_redact_tail(settings.azure_subscription_id),
                hint="login succeeded but subscription wasn't applied",
            )
            # Login itself worked — return True so the caller knows the SP
            # is usable; the model can `az account set` on first probe if
            # the default subscription is wrong for the case.
            return True

    log.info("azure_login_complete")
    return True


def _all_set(*values: str) -> bool:
    return all(bool(v) for v in values)


def _redact_tail(s: str) -> str:
    """Show first 8 chars + length so logs can confirm wiring without leaking."""
    if not s:
        return ""
    return f"{s[:8]}...({len(s)} chars)"


async def _run(cmd: Sequence[str], *, timeout: float) -> tuple[int, str]:
    """Run cmd; return (rc, combined stderr+stdout). Captures both for debug."""
    proc = await asyncio.create_subprocess_exec(
        *cmd,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    try:
        stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=timeout)
    except asyncio.TimeoutError:
        proc.kill()
        await proc.wait()
        return 124, f"timeout after {timeout}s"

    output = (stdout or b"") + (stderr or b"")
    return proc.returncode if proc.returncode is not None else -1, output.decode("utf-8", errors="replace")
