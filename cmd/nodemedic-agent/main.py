"""NodeMedic agent entrypoint. Wires uvicorn around `nodemedic_agent.app`.

All wiring lives inside `nodemedic_agent.app.create_app`; this module is
intentionally minimal so the operational surface is one process, one port,
one worker. Multi-worker would break FR-10 (in-memory case-table coherence).
"""
from __future__ import annotations

import uvicorn

from nodemedic_agent.app import create_app
from nodemedic_agent.config import Settings


def main() -> None:
    settings = Settings()  # validates env on import; raises ValidationError on failure
    app = create_app(settings)
    host, port = _split_listen_addr(settings.agent_listen_addr)
    uvicorn.run(app, host=host, port=port, workers=1, log_config=None)


def _split_listen_addr(addr: str) -> tuple[str, int]:
    host, _, port = addr.rpartition(":")
    return (host or "0.0.0.0", int(port))


if __name__ == "__main__":
    main()
