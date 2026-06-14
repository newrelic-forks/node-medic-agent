"""Integration tests for `/healthz` + `/readyz` (T032).

`/healthz` is always 200 if the process is up.
`/readyz` returns:
  - 200 + resolved model IDs once the gateway catalog probe succeeds.
  - 503 if the gateway returns no usable Claude model.

Uses `respx` to mock the gateway catalog endpoint.
"""
from __future__ import annotations

import httpx
import pytest
import respx
from fastapi.testclient import TestClient

from nodemedic_agent.app import create_app
from nodemedic_agent.config import Settings


@pytest.fixture
def settings(monkeypatch: pytest.MonkeyPatch) -> Settings:
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "tok-fake")
    monkeypatch.setenv("NR_MCP_URL", "https://mcp.example.test")
    monkeypatch.setenv("NR_MCP_TOKEN", "tok-nr-fake")
    monkeypatch.setenv("CLUSTER_NAME", "test-fake")
    monkeypatch.setenv("CLOUD_PROVIDER", "aws")
    monkeypatch.setenv("STUB_AGENT", "true")
    monkeypatch.setenv("ANTHROPIC_BASE_URL", "https://gateway.example.test")
    return Settings()


def test_healthz_always_returns_200(settings: Settings) -> None:
    with TestClient(create_app(settings)) as client:
        r = client.get("/healthz")
    assert r.status_code == 200
    assert r.json()["status"] == "ok"


@respx.mock
def test_readyz_200_when_models_resolve(settings: Settings) -> None:
    respx.get("https://gateway.example.test/v1/models").mock(
        return_value=httpx.Response(
            200,
            json={
                "data": [
                    {"id": "claude-opus-4-7"},
                    {"id": "claude-sonnet-4-6"},
                ]
            },
        )
    )
    respx.head("https://mcp.example.test").mock(return_value=httpx.Response(200))
    with TestClient(create_app(settings)) as client:
        r = client.get("/readyz")
    assert r.status_code == 200
    payload = r.json()
    assert payload["status"] == "ok"
    assert payload["model_primary"] == "claude-opus-4-7"
    assert payload["model_fallback"] == "claude-sonnet-4-6"


@respx.mock
def test_readyz_503_when_no_usable_claude(settings: Settings) -> None:
    respx.get("https://gateway.example.test/v1/models").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"id": "gpt-9000"}]},  # nothing Claude-ish
        )
    )
    respx.head("https://mcp.example.test").mock(return_value=httpx.Response(200))
    with TestClient(create_app(settings)) as client:
        r = client.get("/readyz")
    assert r.status_code == 503


@respx.mock
def test_readyz_200_when_mcp_returns_401(settings: Settings) -> None:
    """MCP probe is reachability-only — 4xx (auth required) means alive."""
    respx.get("https://gateway.example.test/v1/models").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"id": "claude-opus-4-7"}, {"id": "claude-sonnet-4-6"}]},
        )
    )
    respx.head("https://mcp.example.test").mock(return_value=httpx.Response(401))
    with TestClient(create_app(settings)) as client:
        r = client.get("/readyz")
    assert r.status_code == 200


@respx.mock
def test_readyz_503_when_mcp_returns_5xx(settings: Settings) -> None:
    """5xx from MCP means the endpoint is sick, not just authn-gated."""
    respx.get("https://gateway.example.test/v1/models").mock(
        return_value=httpx.Response(
            200,
            json={"data": [{"id": "claude-opus-4-7"}, {"id": "claude-sonnet-4-6"}]},
        )
    )
    respx.head("https://mcp.example.test").mock(return_value=httpx.Response(503))
    with TestClient(create_app(settings)) as client:
        r = client.get("/readyz")
    assert r.status_code == 503


@respx.mock
def test_readyz_200_with_nerd_completion_shape(settings: Settings) -> None:
    """Gateway returns a bare list with `value` keys, not `{"data": [{"id": ...}]}`."""
    respx.get("https://gateway.example.test/v1/models").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"value": "gpt-4o", "provider": "OpenAI"},
                {"value": "claude-opus-4-7", "provider": "Anthropic"},
                {"value": "claude-sonnet-4-6", "provider": "Anthropic"},
            ],
        )
    )
    respx.head("https://mcp.example.test").mock(return_value=httpx.Response(200))
    with TestClient(create_app(settings)) as client:
        r = client.get("/readyz")
    assert r.status_code == 200
    payload = r.json()
    assert payload["model_primary"] == "claude-opus-4-7"
    assert payload["model_fallback"] == "claude-sonnet-4-6"
