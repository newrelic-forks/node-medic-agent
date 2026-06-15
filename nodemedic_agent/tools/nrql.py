"""Local in-process NRQL tool (mirrors nova's k8s-agent-claude-sdk pattern).

The NodeMedic agent originally wired the NR HTTP MCP at
``mcp.newrelic.com``, but that endpoint rejects staging-scoped NRAKs
with a 401. nova's k8s-agent-claude-sdk (running on stg-teeny-toes with
the same NRAK byte-for-byte as ours) sidesteps the HTTP MCP entirely:
it ships a local in-process MCP server that calls NerdGraph directly
with ``Api-Key: <NRAK>``. We use the same pattern here.

Tool surface exposed to the model:

  ``mcp__nodemedic__execute_nrql_query`` — args ``{nrql_query, account_id}``.

The runbook's NRQL probe instructions still reference
``mcp__nr__execute_nrql_query`` for backward compatibility with the HTTP
MCP wiring; the model will accept either name. The runbook is patched
in a follow-up commit to call ``mcp__nodemedic__execute_nrql_query``
explicitly.
"""
from __future__ import annotations

from typing import Any

import httpx
from claude_agent_sdk import SdkMcpTool, tool

from nodemedic_agent.logging import get_logger

log = get_logger(__name__)

# Nova uses 100 s for the HTTP request and 40 s for the wrapped NRQL
# timeout. Mirror those: a slow NRQL row is preferable to a tool-call
# failure that the model has to retry.
HTTP_TIMEOUT_SEC = 100.0
NRQL_QUERY_TIMEOUT_SEC = 40

# GraphQL query that wraps NRQL — copied verbatim from
# nova/k8s-agent-claude-sdk:src/tools/newrelic.py (verified 2026-06-14).
_NRQL_TO_NERDGRAPH_QUERY = """
    query ($account_id: Int!, $nrql_query: Nrql!, $timeout: Seconds) {
        actor {
            account(id: $account_id) {
                nrql(query: $nrql_query, timeout: $timeout) {
                    results
                }
            }
        }
    }
"""


_NRQL_INPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "required": ["nrql_query", "account_id"],
    "additionalProperties": False,
    "properties": {
        "nrql_query": {
            "type": "string",
            "minLength": 1,
            "description": (
                "NRQL query string. Example: 'SELECT count(*) FROM "
                "K8sNodeSample WHERE clusterName = ''cf1z'' SINCE 15 "
                "minutes ago'."
            ),
        },
        "account_id": {
            "type": "integer",
            "description": (
                "New Relic account id. The runbook pins this to 1 "
                "(staging) — the deployed NRAK is staging-scoped and "
                "any other account fails at the credential layer."
            ),
        },
    },
}


def make_execute_nrql_query(
    *,
    api_key: str,
    nerdgraph_url: str,
    case_id: str | None = None,
) -> SdkMcpTool[Any]:
    """Build the per-case ``execute_nrql_query`` tool.

    Args:
      api_key: New Relic API key (NRAK-...). Sent as ``Api-Key`` header.
      nerdgraph_url: NerdGraph endpoint. Defaults to
        ``https://staging-api.newrelic.com/graphql`` in the deployment
        values; mirrors nova's wiring.
      case_id: optional case id used to bind log lines for cross-case
        debugging. The structured logger already adds the bound case_id
        from the contextvar — this is just an additional hint for
        runtime traces.
    """

    @tool(
        name="execute_nrql_query",
        description=(
            "Execute an NRQL query against New Relic via NerdGraph. The "
            "runbook pins account_id=1 (staging). Use this for kubelet "
            "metric-drop probes, K8sNodeSample cardinality checks, and "
            "anything that requires NR telemetry. Do NOT echo the NRQL "
            "string via Bash — call this tool directly so the result "
            "lands in evidence[]."
        ),
        input_schema=_NRQL_INPUT_SCHEMA,
    )
    async def execute_nrql_query(args: dict[str, Any]) -> dict[str, Any]:
        nrql_query = str(args["nrql_query"])
        # The SDK may pass account_id as a string when the model fills
        # the slot from the runbook prose — coerce defensively, mirrors
        # the nova reference impl.
        try:
            account_id = int(args["account_id"])
        except (TypeError, ValueError):
            return {
                "content": [
                    {
                        "type": "text",
                        "text": (
                            f"error: account_id must be an integer; got "
                            f"{args.get('account_id')!r}"
                        ),
                    }
                ],
                "is_error": True,
            }

        log.info(
            "nrql_query_start",
            account_id=account_id,
            query_preview=nrql_query[:120],
        )

        headers = {
            "Api-Key": api_key,
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
        variables = {
            "account_id": account_id,
            "nrql_query": nrql_query,
            "timeout": NRQL_QUERY_TIMEOUT_SEC,
        }
        body = {"query": _NRQL_TO_NERDGRAPH_QUERY, "variables": variables}

        try:
            async with httpx.AsyncClient(timeout=HTTP_TIMEOUT_SEC) as client:
                response = await client.post(
                    nerdgraph_url, json=body, headers=headers
                )
        except httpx.HTTPError as exc:
            log.error("nrql_http_error", detail=str(exc)[:256])
            return {
                "content": [
                    {
                        "type": "text",
                        "text": f"error: NerdGraph HTTP error: {exc}",
                    }
                ],
                "is_error": True,
            }

        if response.status_code != 200:
            log.error(
                "nrql_http_status",
                status=response.status_code,
                body=response.text[:256],
            )
            return {
                "content": [
                    {
                        "type": "text",
                        "text": (
                            f"error: NerdGraph returned HTTP "
                            f"{response.status_code}: {response.text[:256]}"
                        ),
                    }
                ],
                "is_error": True,
            }

        try:
            payload = response.json()
        except ValueError:
            return {
                "content": [
                    {
                        "type": "text",
                        "text": "error: NerdGraph returned a non-JSON body",
                    }
                ],
                "is_error": True,
            }

        # Surface GraphQL errors but don't fail the tool — the model can
        # still decide whether the partial result is enough.
        errors = payload.get("errors")
        if errors:
            log.warning(
                "nrql_graphql_errors",
                errors=[str(e)[:200] for e in errors][:3],
            )

        # Walk defensively — any layer can be None when GraphQL returns
        # a partial result (e.g. NRQL-side error with `nrql: null`).
        nrql_block = (
            ((payload.get("data") or {}).get("actor") or {}).get("account") or {}
        ).get("nrql") or {}
        results = nrql_block.get("results") if isinstance(nrql_block, dict) else None

        log.info(
            "nrql_query_complete",
            account_id=account_id,
            row_count=len(results) if isinstance(results, list) else None,
        )

        return {
            "content": [
                {
                    "type": "text",
                    "text": str(payload),
                }
            ]
        }

    return execute_nrql_query
