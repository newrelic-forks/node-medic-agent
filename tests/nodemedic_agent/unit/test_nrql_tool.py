"""Unit tests for the local NRQL tool (`tools/nrql.py`).

The HTTP MCP at mcp.newrelic.com rejects staging-scoped NRAKs with a
401, so the agent ships a local in-process MCP tool that calls
NerdGraph directly with `Api-Key: <NRAK>`. This test pins the tool's
contract: header shape, query shape, error envelope on HTTP failures,
and account_id coercion.
"""
from __future__ import annotations

from typing import Any

import httpx
import pytest
import respx

from nodemedic_agent.tools.nrql import make_execute_nrql_query

NERDGRAPH_URL = "https://staging-api.newrelic.com/graphql"


@respx.mock
@pytest.mark.asyncio
async def test_happy_path_posts_nerdgraph_with_api_key() -> None:
    captured: dict[str, Any] = {}

    def _capture(request: httpx.Request) -> httpx.Response:
        captured["headers"] = dict(request.headers)
        captured["json"] = request.read().decode()
        return httpx.Response(
            200,
            json={
                "data": {
                    "actor": {
                        "account": {
                            "nrql": {
                                "results": [{"count": 42}],
                            }
                        }
                    }
                }
            },
        )

    respx.post(NERDGRAPH_URL).mock(side_effect=_capture)

    tool = make_execute_nrql_query(
        api_key="NRAK-TESTKEY", nerdgraph_url=NERDGRAPH_URL, case_id="case-A"
    )
    result = await tool.handler(
        {"nrql_query": "SELECT count(*) FROM K8sNodeSample", "account_id": 1}
    )

    assert "is_error" not in result
    text = result["content"][0]["text"]
    assert "results" in text
    assert "42" in text

    # Auth + content negotiation match nova's pattern verbatim.
    assert captured["headers"]["api-key"] == "NRAK-TESTKEY"
    assert captured["headers"]["content-type"] == "application/json"
    assert "application/json" in captured["headers"]["accept"]

    # Variables were sent through the GraphQL wrapper.
    assert "K8sNodeSample" in captured["json"]
    assert "account_id" in captured["json"]


@respx.mock
@pytest.mark.asyncio
async def test_account_id_string_is_coerced() -> None:
    """Model sometimes passes account_id as a string — accept it."""
    respx.post(NERDGRAPH_URL).mock(
        return_value=httpx.Response(
            200,
            json={"data": {"actor": {"account": {"nrql": {"results": []}}}}},
        )
    )
    tool = make_execute_nrql_query(api_key="NRAK-X", nerdgraph_url=NERDGRAPH_URL)
    result = await tool.handler(
        {"nrql_query": "SELECT 1", "account_id": "1"}
    )
    assert "is_error" not in result


@respx.mock
@pytest.mark.asyncio
async def test_account_id_invalid_returns_error_envelope() -> None:
    tool = make_execute_nrql_query(api_key="NRAK-X", nerdgraph_url=NERDGRAPH_URL)
    result = await tool.handler({"nrql_query": "SELECT 1", "account_id": "abc"})
    assert result["is_error"] is True
    assert "account_id" in result["content"][0]["text"]


@respx.mock
@pytest.mark.asyncio
async def test_http_401_returns_error_envelope() -> None:
    respx.post(NERDGRAPH_URL).mock(
        return_value=httpx.Response(401, text="Unauthorized")
    )
    tool = make_execute_nrql_query(api_key="NRAK-X", nerdgraph_url=NERDGRAPH_URL)
    result = await tool.handler(
        {"nrql_query": "SELECT 1", "account_id": 1}
    )
    assert result["is_error"] is True
    assert "401" in result["content"][0]["text"]


@respx.mock
@pytest.mark.asyncio
async def test_transport_error_returns_error_envelope() -> None:
    respx.post(NERDGRAPH_URL).mock(side_effect=httpx.ConnectError("boom"))
    tool = make_execute_nrql_query(api_key="NRAK-X", nerdgraph_url=NERDGRAPH_URL)
    result = await tool.handler(
        {"nrql_query": "SELECT 1", "account_id": 1}
    )
    assert result["is_error"] is True
    assert "HTTP error" in result["content"][0]["text"]


@respx.mock
@pytest.mark.asyncio
async def test_graphql_errors_pass_through_as_text() -> None:
    """GraphQL-level errors don't fail the tool — model can decide."""
    respx.post(NERDGRAPH_URL).mock(
        return_value=httpx.Response(
            200,
            json={
                "errors": [{"message": "rate-limited"}],
                "data": {"actor": {"account": {"nrql": None}}},
            },
        )
    )
    tool = make_execute_nrql_query(api_key="NRAK-X", nerdgraph_url=NERDGRAPH_URL)
    result = await tool.handler(
        {"nrql_query": "SELECT 1", "account_id": 1}
    )
    # Tool returns the raw payload string so the model can read both
    # `errors` and `data.actor.account.nrql`. is_error stays False.
    assert "is_error" not in result
    assert "rate-limited" in result["content"][0]["text"]
