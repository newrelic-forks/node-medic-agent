"""Process configuration.

All env vars listed here map 1:1 to spec NFR-6's table. Validation runs at
process start (`Settings()` raises ``ValidationError`` if anything is wrong);
uvicorn fails to launch rather than lazily blowing up on first request.

The ``cluster_name`` allowlist mirrors the Helm chart's ``requireTestCluster``
helper (Constitution Article I.5 belt-and-suspenders): cf1z, jc1z, sk1z, or
any ``test-*`` prefix. Anything else refuses to start.
"""
from __future__ import annotations

import re
from typing import Literal

from pydantic import Field, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

LogLevel = Literal["debug", "info", "warn", "error"]
CloudProvider = Literal["aws", "azure"]

_TEST_CLUSTER_PREFIX = re.compile(r"^test-[a-z0-9-]+$")
_LEGACY_LITERALS = frozenset({"cf1z", "jc1z", "sk1z"})


def _is_allowed_cluster(name: str) -> bool:
    return name in _LEGACY_LITERALS or bool(_TEST_CLUSTER_PREFIX.match(name))


class Settings(BaseSettings):
    """Validated runtime config. Raises ``ValidationError`` at construction."""

    # HTTP server
    agent_listen_addr: str = ":8080"

    # Concurrency cap (FR-1 / FR-10 / NFR-8)
    max_concurrent_cases: int = Field(default=32, ge=1)

    # Anthropic / nerd-completion gateway
    claude_model: str = "claude-opus-4-7"
    claude_fallback_model: str = "claude-sonnet-4-6"
    anthropic_auth_token: str = Field(min_length=1)
    anthropic_base_url: str = "https://nerd-completion.staging-service.nr-ops.net"

    # New Relic HTTP MCP
    nr_mcp_url: str = Field(min_length=1)
    nr_mcp_token: str = Field(min_length=1)
    nr_account_id: int = 1  # locked at staging account

    # Azure SP (only required on Azure installs; cloud_provider validator enforces)
    azure_tenant_id: str = ""
    azure_client_id: str = ""
    azure_client_secret: str = ""
    # Subscription is needed so `az` knows which scope to default to without
    # the model having to discover it on every probe. Empty = leave the SP's
    # default subscription active (whatever az resolves at login time).
    azure_subscription_id: str = ""

    # SSH key for worker-node probes
    ssh_key_path: str = "/etc/nodemedic/ssh/id_ed25519"

    # Logging / probes
    log_level: LogLevel = "info"
    readyz_probe_timeout_sec: int = Field(default=5, ge=1)

    # Runtime paths and topology
    runbook_path: str = "/app/prompts/runbook.md"
    cluster_name: str = Field(min_length=1)
    cloud_provider: CloudProvider = Field(default="azure")
    kube_namespace: str = "cf-monitoring"

    # Stub mode — Phase 3 worker. Production path flips to False in T058.
    stub_agent: bool = True

    model_config = SettingsConfigDict(
        env_prefix="",
        case_sensitive=False,
        extra="ignore",
    )

    # ------------------------------------------------------------------
    # Validators
    # ------------------------------------------------------------------

    @field_validator("cluster_name")
    @classmethod
    def _validate_cluster_allowlist(cls, value: str) -> str:
        """Constitution Article I.5 — agent only runs on non-prod clusters."""
        if not _is_allowed_cluster(value):
            raise ValueError(
                f"cluster_name={value!r} is not an allowed non-production cluster. "
                "Permitted values: cf1z, jc1z, sk1z, or any test-* cluster "
                "(Constitution Article I.5)."
            )
        return value

    @field_validator("agent_listen_addr")
    @classmethod
    def _validate_listen_addr(cls, value: str) -> str:
        """Accept ':8080' (host omitted) or 'host:port'."""
        _, sep, port = value.rpartition(":")
        if not sep or not port:
            raise ValueError(f"agent_listen_addr={value!r} must be 'host:port' or ':port'")
        try:
            port_int = int(port)
        except ValueError as exc:
            raise ValueError(f"agent_listen_addr={value!r} has non-integer port") from exc
        if not (1 <= port_int <= 65535):
            raise ValueError(f"agent_listen_addr={value!r} port out of range")
        return value
