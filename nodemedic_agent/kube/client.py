"""Kubernetes client wiring + the FR-12 phase-conflict pure helper.

The agent's kube footprint is intentionally small (R-3): one ``get`` + one
``replace_status`` per case (with the FR-12 pre-write phase re-read, that's
two ``get``s + one ``replace_status``). We use ``CustomObjectsApi`` directly
— no generated CRD types, no operator framework.
"""
from __future__ import annotations

from typing import Any

from kubernetes import client, config

NHD_GROUP = "nodemedic.cf.newrelic.com"
NHD_VERSION = "v1alpha1"
NHD_PLURAL = "nodehealthdiagnosisais"

# FR-12: phases the agent is allowed to overwrite. Anything else means
# another scope already finalised the case (or is mid-finalisation) and the
# agent must defer rather than clobber.
_OVERWRITABLE_PHASES = frozenset({"", "Pending", "Diagnosing"})

# Phases for which a defer is structurally expected (the controller has
# moved on or another scope wrote first). Listed for documentation; any
# unknown phase also defers.
_TERMINAL_PHASES = frozenset({"Diagnosed", "Acted", "Failed", "Evaluating", "Evaluated"})


def should_overwrite(observed_phase: str) -> bool:
    """Return True when the writer may proceed with a Status().Update.

    FR-12 phase-conflict matrix:
      - "" / "Pending" / "Diagnosing" → True
      - "Diagnosed" / "Acted" / "Failed" / "Evaluating" / "Evaluated" → False
      - any other unrecognised value → False (fail closed)
    """
    return observed_phase in _OVERWRITABLE_PHASES


_loaded = False


def load_kube_config() -> None:
    """Load in-cluster config (idempotent). No-op after the first call.

    Tests substitute the apiserver via ``respx``; this helper is for the
    real runtime path.
    """
    global _loaded
    if _loaded:
        return
    config.load_incluster_config()
    _loaded = True


def custom_objects_api() -> client.CustomObjectsApi:
    """Return a fresh ``CustomObjectsApi`` bound to the in-cluster config."""
    return client.CustomObjectsApi()


def nhd_get(namespace: str, name: str) -> dict[str, Any]:
    """Read the full NHD object (used for FR-12 phase re-read)."""
    api = custom_objects_api()
    return api.get_namespaced_custom_object(  # type: ignore[no-any-return]
        group=NHD_GROUP,
        version=NHD_VERSION,
        namespace=namespace,
        plural=NHD_PLURAL,
        name=name,
    )


def nhd_replace_status(namespace: str, name: str, body: dict[str, Any]) -> dict[str, Any]:
    """Write the full NHD ``status`` subresource (FR-8 / R-13).

    Caller is responsible for retry policy and FR-12 phase pre-check; this
    helper is the raw apiserver call.
    """
    api = custom_objects_api()
    return api.replace_namespaced_custom_object_status(  # type: ignore[no-any-return]
        group=NHD_GROUP,
        version=NHD_VERSION,
        namespace=namespace,
        plural=NHD_PLURAL,
        name=name,
        body=body,
    )
