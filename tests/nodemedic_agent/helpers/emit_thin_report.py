"""Spec 002 T072 / Spec 003 AC-2 helper — emit a thin-evidence diagnosis.

Creates a fresh NodeHealthDiagnosisAI on the current kube context with a
hand-canned thin diagnosis (confidence 0.4, single evidence source,
recommendation.action=NoAction, rcaCategory=Kubelet). The deployed
controller picks it up at phase=Diagnosed, runs its confidence gate,
classifies the case as GateSkip, and posts the Block Kit HumanInLoop
Slack message — i.e. exactly the AC-2 demo path.

Usage:

    uv run python -m tests.nodemedic_agent.helpers.emit_thin_report \\
      --case-id <uuid> --nhd-name <name> [--node-name <node>] \\
      [--namespace cf-monitoring] [--cluster-name cf1z]

The deviation from the original spec wording: the original Spec 002
T072 brief proposed an in-process uvicorn TestClient harness. cf1z's
agent is a deployed pod, not in-process, so the TestClient framing
doesn't fit. Talking to the apiserver directly via CustomObjectsApi is
the simplest correct shape — the controller doesn't care who wrote the
diagnosis, only that phase=Diagnosed and the diagnosis fields populate.

Pre-conditions:

* ``KUBECONFIG`` points at cf1z (or the test cluster of choice). The
  helper uses out-of-cluster config (load_kube_config), not
  in-cluster — it's a developer-laptop tool.
* The target node exists, but its name only feeds spec.case.nodeName;
  the controller's gate is not gated on Node existence.
* The controller is running a binary that knows about phase=Diagnosed
  (Spec 001 + Spec 003 Phase 3 — i.e. anything ≥ dev-cf1z-7aaf8cea).

The helper is idempotent in the sense that re-running it with the same
``--nhd-name`` will fail with a 409 Conflict on the create; pick a
fresh name (the script doesn't auto-generate one because predictable
names make demo-day correlation easier).
"""
from __future__ import annotations

import argparse
import sys
from datetime import datetime, timezone
from typing import Any

from kubernetes import client, config

NHD_GROUP = "nodemedic.cf.newrelic.com"
NHD_VERSION = "v1alpha1"
NHD_PLURAL = "nodehealthdiagnosisais"


def _build_spec(
    case_id: str,
    nhd_name: str,
    node_name: str,
    cluster_name: str,
    namespace: str,
    observed_at: str,
) -> dict[str, Any]:
    return {
        "apiVersion": f"{NHD_GROUP}/{NHD_VERSION}",
        "kind": "NodeHealthDiagnosisAI",
        "metadata": {"name": nhd_name, "namespace": namespace},
        "spec": {
            "budgets": {
                "deadline": "2099-12-31T23:59:59Z",
                "maxBudgetUSD": "0.50",
                "maxTurns": 15,
            },
            "case": {
                "caseId": case_id,
                "clusterName": cluster_name,
                "instanceId": "thin-report-helper",
                "nodeName": node_name,
                "provider": "azure",
                "region": "eastus",
                "trigger": {
                    "message": "thin-evidence fixture for AC-2",
                    "observedAt": observed_at,
                    "reason": "KubeletHealthzFailed",
                    "type": "KubeletUnhealthy",
                },
            },
        },
    }


def _build_status(node_name: str, completed_at: str) -> dict[str, Any]:
    return {
        "status": {
            "phase": "Diagnosed",
            "diagnosis": {
                "completedAt": completed_at,
                "confidence": 0.4,
                "modelUsed": "claude-opus-4-7",
                "rcaCategory": "Kubelet",
                "rootCause": (
                    "single-source diagnosis below confidence threshold; "
                    "cannot recommend autonomous action"
                ),
                "recommendation": {
                    "action": "NoAction",
                    "reason": "evidence too thin to act safely; needs human review",
                },
                "evidence": [
                    {
                        "observedAt": completed_at,
                        "ref": f"kubectl get node {node_name} -o yaml",
                        "result": (
                            "kubelet healthz probe failed; no other corroborating "
                            "signals available within budget"
                        ),
                        "source": "kubectl",
                    },
                ],
            },
            "conditions": [
                {
                    "type": "AgentInvoked",
                    "status": "True",
                    "reason": "Posted",
                    "message": "thin-evidence fixture for AC-2",
                    "lastTransitionTime": completed_at,
                },
                {
                    "type": "ReportReady",
                    "status": "True",
                    "reason": "EvidenceValid",
                    "message": "thin-evidence fixture for AC-2",
                    "lastTransitionTime": completed_at,
                },
            ],
        },
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="emit_thin_report",
        description=(
            "Hand-craft a low-confidence NHD that drives the controller's "
            "confidence gate to HumanInLoop (AC-2 demo path)."
        ),
    )
    p.add_argument("--case-id", required=True, help="UUID for spec.case.caseId.")
    p.add_argument("--nhd-name", required=True, help="metadata.name of the new NHD.")
    p.add_argument(
        "--node-name",
        default="cf1z-general-nodes-canary",
        help="spec.case.nodeName (default: cf1z-general-nodes-canary).",
    )
    p.add_argument("--namespace", default="cf-monitoring", help="metadata.namespace.")
    p.add_argument("--cluster-name", default="cf1z", help="spec.case.clusterName.")
    args = p.parse_args(argv)

    config.load_kube_config()
    api = client.CustomObjectsApi()

    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    body = _build_spec(
        case_id=args.case_id,
        nhd_name=args.nhd_name,
        node_name=args.node_name,
        cluster_name=args.cluster_name,
        namespace=args.namespace,
        observed_at=now,
    )

    try:
        api.create_namespaced_custom_object(
            group=NHD_GROUP,
            version=NHD_VERSION,
            namespace=args.namespace,
            plural=NHD_PLURAL,
            body=body,
        )
    except client.ApiException as exc:
        print(
            f"ERROR creating NHD {args.namespace}/{args.nhd_name}: "
            f"status={exc.status} reason={exc.reason} body={exc.body}",
            file=sys.stderr,
        )
        return 1

    api.patch_namespaced_custom_object_status(
        group=NHD_GROUP,
        version=NHD_VERSION,
        namespace=args.namespace,
        plural=NHD_PLURAL,
        name=args.nhd_name,
        body=_build_status(node_name=args.node_name, completed_at=now),
    )

    print(
        f"thin-report posted: namespace={args.namespace} nhd={args.nhd_name} "
        f"node={args.node_name} case_id={args.case_id} confidence=0.4 "
        f"action=NoAction — controller should route through humanInLoopPath "
        f"and post Block Kit HumanInLoop within ~1s."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
