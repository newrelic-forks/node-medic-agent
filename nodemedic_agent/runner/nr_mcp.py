"""NR HTTP MCP wiring (T056 / R-6).

The NR MCP server is configured as a streaming-HTTP MCP server in the
SDK's ``ClaudeAgentOptions.mcp_servers`` dict. The NR token is injected
via the SDK's MCP server config ``headers`` field; the runbook prompt
carries the ``account_id=1`` instruction (the agent's NR token is
scoped to staging, so any other account fails at the credential layer).
"""
from __future__ import annotations

from typing import Any

from nodemedic_agent.config import Settings


def nr_mcp_config(settings: Settings) -> dict[str, Any]:
    """Build the NR HTTP MCP server config for ``ClaudeAgentOptions``.

    Returns a dict matching ``McpHttpServerConfig`` shape:
        {"type": "http", "url": ..., "headers": {"Authorization": "Bearer ..."}}.
    """
    return {
        "type": "http",
        "url": settings.nr_mcp_url,
        "headers": {
            "Authorization": f"Bearer {settings.nr_mcp_token}",
        },
    }
