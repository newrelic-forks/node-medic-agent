"""Unit tests for `nodemedic_agent.config.Settings` (T012).

Spec NFR-6 + Constitution Article I.5 lock the validation rules; this file is
the executable contract.
"""
from __future__ import annotations

import pytest
from pydantic import ValidationError

from nodemedic_agent.config import Settings


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

VALID_ENV: dict[str, str] = {
    "ANTHROPIC_AUTH_TOKEN": "tok-anth-deadbeef",
    "NR_MCP_URL": "https://mcp.example.test",
    "NR_MCP_TOKEN": "tok-nr-deadbeef",
    "CLUSTER_NAME": "cf1z",
    "CLOUD_PROVIDER": "azure",
}


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Strip every env var Settings reads — tests opt back in via VALID_ENV."""
    for key in (
        "AGENT_LISTEN_ADDR",
        "MAX_CONCURRENT_CASES",
        "CLAUDE_MODEL",
        "CLAUDE_FALLBACK_MODEL",
        "ANTHROPIC_AUTH_TOKEN",
        "ANTHROPIC_BASE_URL",
        "NR_MCP_URL",
        "NR_MCP_TOKEN",
        "NR_ACCOUNT_ID",
        "AZURE_TENANT_ID",
        "AZURE_CLIENT_ID",
        "AZURE_CLIENT_SECRET",
        "SSH_KEY_PATH",
        "LOG_LEVEL",
        "READYZ_PROBE_TIMEOUT_SEC",
        "RUNBOOK_PATH",
        "CLUSTER_NAME",
        "CLOUD_PROVIDER",
        "KUBE_NAMESPACE",
        "STUB_AGENT",
    ):
        monkeypatch.delenv(key, raising=False)


def _set_env(monkeypatch: pytest.MonkeyPatch, **overrides: str) -> None:
    env = {**VALID_ENV, **overrides}
    for key, value in env.items():
        monkeypatch.setenv(key, value)


# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------


def test_minimal_valid_env_loads(monkeypatch: pytest.MonkeyPatch) -> None:
    _set_env(monkeypatch)
    settings = Settings()
    assert settings.cluster_name == "cf1z"
    assert settings.cloud_provider == "azure"
    assert settings.max_concurrent_cases == 32
    assert settings.nr_account_id == 1
    assert settings.log_level == "info"
    assert settings.runbook_path == "/app/prompts/runbook.md"
    assert settings.kube_namespace == "cf-monitoring"


# ---------------------------------------------------------------------------
# Required fields
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "missing",
    ["ANTHROPIC_AUTH_TOKEN", "NR_MCP_URL", "NR_MCP_TOKEN", "CLUSTER_NAME"],
)
def test_missing_required_env_raises(monkeypatch: pytest.MonkeyPatch, missing: str) -> None:
    _set_env(monkeypatch)
    monkeypatch.delenv(missing, raising=False)
    with pytest.raises(ValidationError) as excinfo:
        Settings()
    # Pydantic emits a per-field error; surface the field name in the assertion.
    assert missing.lower() in str(excinfo.value).lower()


# ---------------------------------------------------------------------------
# Cluster-name allowlist (Constitution Article I.5)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("name", ["cf1z", "jc1z", "sk1z", "test-foo", "test-bouncy-robot"])
def test_cluster_name_allowed(monkeypatch: pytest.MonkeyPatch, name: str) -> None:
    _set_env(monkeypatch, CLUSTER_NAME=name)
    settings = Settings()
    assert settings.cluster_name == name


@pytest.mark.parametrize(
    "name",
    [
        "stg-going-plaid",  # staging — forbidden
        "us-big-cone",      # US prod — forbidden
        "eu-lesser-forest", # EU prod — forbidden
        "interlinked",      # mgmt hub — forbidden
        "TEST-foo",         # case-sensitive
        "production",
        "",                 # empty — caught by min_length first
    ],
)
def test_cluster_name_rejected(monkeypatch: pytest.MonkeyPatch, name: str) -> None:
    _set_env(monkeypatch, CLUSTER_NAME=name)
    with pytest.raises(ValidationError):
        Settings()


# ---------------------------------------------------------------------------
# Enum validation
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("provider", ["aws", "azure"])
def test_cloud_provider_accepts_aws_and_azure(
    monkeypatch: pytest.MonkeyPatch, provider: str
) -> None:
    _set_env(monkeypatch, CLOUD_PROVIDER=provider)
    settings = Settings()
    assert settings.cloud_provider == provider


@pytest.mark.parametrize("provider", ["gcp", "oci", "kubernetes", ""])
def test_cloud_provider_rejects_others(
    monkeypatch: pytest.MonkeyPatch, provider: str
) -> None:
    _set_env(monkeypatch, CLOUD_PROVIDER=provider)
    with pytest.raises(ValidationError):
        Settings()


@pytest.mark.parametrize("level", ["debug", "info", "warn", "error"])
def test_log_level_accepts_documented_values(
    monkeypatch: pytest.MonkeyPatch, level: str
) -> None:
    _set_env(monkeypatch, LOG_LEVEL=level)
    settings = Settings()
    assert settings.log_level == level


@pytest.mark.parametrize("level", ["trace", "verbose", "warning", "INFO", ""])
def test_log_level_rejects_others(monkeypatch: pytest.MonkeyPatch, level: str) -> None:
    _set_env(monkeypatch, LOG_LEVEL=level)
    if level == "INFO":
        # Pydantic-settings normalises case for env vars but not for enum values
        # within Literal[...]; "INFO" should be rejected.
        pass
    with pytest.raises(ValidationError):
        Settings()


# ---------------------------------------------------------------------------
# Listen address
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("addr", [":8080", "0.0.0.0:8080", "127.0.0.1:18080"])
def test_listen_addr_accepts_valid_forms(
    monkeypatch: pytest.MonkeyPatch, addr: str
) -> None:
    _set_env(monkeypatch, AGENT_LISTEN_ADDR=addr)
    assert Settings().agent_listen_addr == addr


@pytest.mark.parametrize("addr", ["8080", "0.0.0.0:abc", "0.0.0.0:99999", ":0"])
def test_listen_addr_rejects_garbage(
    monkeypatch: pytest.MonkeyPatch, addr: str
) -> None:
    _set_env(monkeypatch, AGENT_LISTEN_ADDR=addr)
    with pytest.raises(ValidationError):
        Settings()


# ---------------------------------------------------------------------------
# Numeric ranges
# ---------------------------------------------------------------------------


def test_max_concurrent_cases_must_be_positive(monkeypatch: pytest.MonkeyPatch) -> None:
    _set_env(monkeypatch, MAX_CONCURRENT_CASES="0")
    with pytest.raises(ValidationError):
        Settings()


def test_readyz_probe_timeout_must_be_positive(monkeypatch: pytest.MonkeyPatch) -> None:
    _set_env(monkeypatch, READYZ_PROBE_TIMEOUT_SEC="0")
    with pytest.raises(ValidationError):
        Settings()
