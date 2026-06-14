"""FastAPI app factory.

`create_app(settings)` is the single entry point. Wires:
  - structlog config (NFR-3)
  - in-cluster kube config (lazy — tests bypass it)
  - case table + retention sweeper (FR-10)
  - NHD writer (FR-8 / FR-12)
  - model resolver (FR-3)
  - concurrency semaphore (FR-1)
  - /diagnose, /healthz, /readyz routes
  - graceful shutdown (cancels in-flight worker tasks)

The default ``app`` instance at module level is what `Dockerfile.nodemedic-agent`
hands to uvicorn (`nodemedic_agent.app:app`); tests use ``create_app`` directly.
"""
from __future__ import annotations

import asyncio
import os
from contextlib import asynccontextmanager
from typing import Any

from fastapi import FastAPI, Request, Response

from nodemedic_agent.api.diagnose import (
    DiagnoseRequest,
    build_diagnose_router,
    handle_diagnose,
)
from nodemedic_agent.api.health import (
    build_health_router,
    handle_healthz,
    handle_readyz,
)
from nodemedic_agent.config import Settings
from nodemedic_agent.kube.nhd_writer import NHDWriter
from nodemedic_agent.logging import configure_logging, get_logger
from nodemedic_agent.runner import case_worker
from nodemedic_agent.runner.case_table import CaseTable
from nodemedic_agent.runner.model_resolver import ModelResolver

log = get_logger(__name__)


def create_app(settings: Settings | None = None) -> FastAPI:
    """Build the FastAPI app for ``settings`` (or the default-loaded env)."""
    settings = settings or Settings()
    configure_logging(settings.log_level)

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> Any:
        log.info(
            "startup_complete",
            cluster_name=settings.cluster_name,
            cloud_provider=settings.cloud_provider,
            stub_agent=settings.stub_agent,
            listen_addr=settings.agent_listen_addr,
        )
        case_table = CaseTable()
        await case_table.start_sweeper()

        # Real kube config only when not in test (env CLUSTER_NAME starts
        # with "test-" is fine; we still try to load in-cluster config but
        # tolerate failure so unit tests work without a kubeconfig).
        nhd_writer = NHDWriter(namespace=settings.kube_namespace)
        try:
            from nodemedic_agent.kube import client as kube_client
            kube_client.load_kube_config()
        except Exception as exc:  # noqa: BLE001
            log.warning(
                "in_cluster_config_unavailable",
                detail=str(exc)[:256],
                hint="expected outside a Pod; runtime path requires it",
            )

        app.state.settings = settings
        app.state.case_table = case_table
        app.state.case_semaphore = asyncio.Semaphore(settings.max_concurrent_cases)
        app.state.nhd_writer = nhd_writer
        app.state.model_resolver = ModelResolver(settings)
        app.state.readyz_cache = {}
        # Use the real worker; it dispatches to the stub when stub_agent=True.
        app.state.run_case = case_worker.run_case
        try:
            yield
        finally:
            await case_table.stop_sweeper()
            log.info("shutdown_complete")

    app = FastAPI(
        title="nodemedic-agent",
        version="0.1.0",
        lifespan=lifespan,
    )

    diagnose_router = build_diagnose_router()
    diagnose_router.add_api_route(
        "/diagnose",
        endpoint=_diagnose_endpoint,
        methods=["POST"],
        status_code=202,
        response_model=None,
    )
    app.include_router(diagnose_router)

    health_router = build_health_router()
    health_router.add_api_route(
        "/healthz",
        endpoint=handle_healthz,
        methods=["GET"],
        response_model=None,
    )
    health_router.add_api_route(
        "/readyz",
        endpoint=handle_readyz,
        methods=["GET"],
        response_model=None,
    )
    app.include_router(health_router)

    return app


async def _diagnose_endpoint(request: Request, body: DiagnoseRequest) -> Response:
    """Thin wrapper so FastAPI can resolve the dependency-injection chain."""
    return await handle_diagnose(request, body)


def run() -> None:
    """`pyproject.toml` script entrypoint — `nodemedic-agent` console command."""
    import uvicorn

    settings = Settings()
    host, port = _split_listen_addr(settings.agent_listen_addr)
    uvicorn.run(
        "nodemedic_agent.app:app",
        host=host,
        port=port,
        workers=1,
        log_config=None,
    )


def _split_listen_addr(addr: str) -> tuple[str, int]:
    host, _, port = addr.rpartition(":")
    return (host or "0.0.0.0", int(port))


# Module-level default app instance — the path the Dockerfile points at.
# Constructed eagerly so uvicorn's import side-effect runs the validators.
# Skipped during tests to avoid env-var leakage; tests call create_app
# directly with a fixture-built Settings.
if os.environ.get("PYTEST_CURRENT_TEST") is None and not os.environ.get(
    "NODEMEDIC_SKIP_DEFAULT_APP"
):
    try:
        app = create_app()
    except Exception as exc:  # noqa: BLE001
        # Don't crash on import in dev shells without env set; the
        # uvicorn entrypoint re-runs Settings() and will surface the real
        # error.
        log.warning("default_app_construction_skipped", detail=str(exc)[:256])
        app = None  # type: ignore[assignment]
