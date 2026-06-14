"""Integration tests for the agent Helm chart render (T026).

Asserts the chart's structural invariants:
  - Constitution Article I.2: ClusterRole has NO ``nodes/patch`` and NO
    ``*`` verbs on ``nodes``.
  - Deployment mounts the four expected secrets (anthropic, NR MCP, ssh,
    azure on Azure installs).
  - Constitution Article I.5: ``requireTestCluster`` rejects
    ``stg-going-plaid`` etc. and accepts cf1z/jc1z/sk1z/test-*.

Skipped at collect time if `helm` is not on PATH.
"""
from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest
import yaml

CHART_PATH = (
    Path(__file__).resolve().parents[3] / "deployment" / "helm" / "nodemedic-agent"
)
HAS_HELM = shutil.which("helm") is not None

pytestmark = pytest.mark.skipif(not HAS_HELM, reason="helm CLI not installed")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _render(
    cluster_name: str,
    *,
    extra_sets: dict[str, str] | None = None,
    values_files: list[str] | None = None,
    expect_fail: bool = False,
) -> tuple[int, str, str]:
    """Run `helm template` on the agent chart. Returns (rc, stdout, stderr)."""
    cmd = [
        "helm",
        "template",
        "nodemedic-agent",
        str(CHART_PATH),
        "--set", f"clusterName={cluster_name}",
        "--set", "image.tag=test",
        "--set", "cloud.provider=azure",
        "--set", "config.nrMcpUrl=https://mcp.example.test",
    ]
    for vf in values_files or []:
        cmd.extend(["--values", vf])
    for k, v in (extra_sets or {}).items():
        cmd.extend(["--set", f"{k}={v}"])

    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if not expect_fail and proc.returncode != 0:
        pytest.fail(f"helm template failed (rc={proc.returncode}): {proc.stderr}")
    return proc.returncode, proc.stdout, proc.stderr


def _parse_docs(stdout: str) -> list[dict]:
    return [d for d in yaml.safe_load_all(stdout) if d]


def _by_kind(docs: list[dict], kind: str) -> list[dict]:
    return [d for d in docs if d.get("kind") == kind]


# ---------------------------------------------------------------------------
# Cluster-name guard (Constitution Article I.5)
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("cluster", ["cf1z", "jc1z", "sk1z", "test-foo", "test-bouncy-robot"])
def test_chart_renders_for_allowed_clusters(cluster: str) -> None:
    rc, stdout, _ = _render(cluster)
    assert rc == 0
    docs = _parse_docs(stdout)
    deploy = _by_kind(docs, "Deployment")
    assert len(deploy) == 1


@pytest.mark.parametrize(
    "cluster",
    ["stg-going-plaid", "us-big-cone", "eu-lesser-forest", "interlinked", "production"],
)
def test_chart_rejects_disallowed_clusters(cluster: str) -> None:
    rc, _, stderr = _render(cluster, expect_fail=True)
    assert rc != 0
    assert "Constitution Article I.5" in stderr or "non-production" in stderr


def test_chart_requires_cluster_name() -> None:
    rc, _, stderr = _render("", expect_fail=True)
    assert rc != 0
    assert "clusterName" in stderr or "Article I.5" in stderr


# ---------------------------------------------------------------------------
# RBAC scoping (Constitution Article I.2 + FR-14)
# ---------------------------------------------------------------------------


def test_clusterrole_has_no_nodes_patch() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [role] = _by_kind(docs, "ClusterRole")
    for rule in role["rules"]:
        if "nodes" in rule.get("resources", []):
            verbs = set(rule.get("verbs", []))
            assert "patch" not in verbs, "Constitution Article I.2: NO nodes/patch"
            assert "update" not in verbs, "Constitution Article I.2: NO nodes/update"
            assert "create" not in verbs, "Constitution Article I.2: NO nodes/create"
            assert "delete" not in verbs, "Constitution Article I.2: NO nodes/delete"
            assert "*" not in verbs, "Constitution Article I.2: NO * on nodes"


def test_clusterrole_has_required_nhd_verbs() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [role] = _by_kind(docs, "ClusterRole")
    nhd_rules = [r for r in role["rules"] if r.get("resources") == ["nodehealthdiagnosisais"]]
    assert nhd_rules, "ClusterRole missing rule for nodehealthdiagnosisais"
    verbs = set()
    for rule in nhd_rules:
        verbs.update(rule.get("verbs", []))
    assert {"get", "list", "watch", "update"}.issubset(verbs)

    status_rules = [
        r for r in role["rules"] if r.get("resources") == ["nodehealthdiagnosisais/status"]
    ]
    assert status_rules, "ClusterRole missing rule for nodehealthdiagnosisais/status"
    status_verbs: set[str] = set()
    for rule in status_rules:
        status_verbs.update(rule.get("verbs", []))
    assert "update" in status_verbs


def test_clusterrole_has_no_create_pods() -> None:
    """Defense in depth — agent must not be able to create pods."""
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [role] = _by_kind(docs, "ClusterRole")
    for rule in role["rules"]:
        if "pods" in rule.get("resources", []):
            assert "create" not in rule.get("verbs", [])
            assert "delete" not in rule.get("verbs", [])
            assert "patch" not in rule.get("verbs", [])
            assert "update" not in rule.get("verbs", [])


# ---------------------------------------------------------------------------
# Deployment mounts the right secrets (NFR-5 chart-side enforcement)
# ---------------------------------------------------------------------------


def test_deployment_mounts_anthropic_token_secret() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    [container] = deploy["spec"]["template"]["spec"]["containers"]
    env = {e["name"]: e for e in container["env"]}
    assert env["ANTHROPIC_AUTH_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "nodemedic-anthropic-token"


def test_deployment_mounts_nr_token_secret() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    [container] = deploy["spec"]["template"]["spec"]["containers"]
    env = {e["name"]: e for e in container["env"]}
    assert env["NR_MCP_TOKEN"]["valueFrom"]["secretKeyRef"]["name"] == "nodemedic-nr-token"


def test_deployment_mounts_ssh_key_volume() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    pod = deploy["spec"]["template"]["spec"]
    volumes = {v["name"]: v for v in pod["volumes"]}
    assert volumes["ssh-key"]["secret"]["secretName"] == "nodemedic-ssh-key"


def test_azure_install_mounts_azure_creds_only() -> None:
    """values-azure.yaml — Azure SP env vars present, no AWS IRSA annotation."""
    rc, stdout, _ = _render(
        "cf1z",
        values_files=[str(CHART_PATH / "values-azure.yaml")],
    )
    assert rc == 0
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    [container] = deploy["spec"]["template"]["spec"]["containers"]
    env_names = {e["name"] for e in container["env"]}
    assert {"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET"}.issubset(env_names)

    # ServiceAccount carries no AWS annotation on Azure installs.
    [sa] = _by_kind(docs, "ServiceAccount")
    annotations = sa.get("metadata", {}).get("annotations") or {}
    assert "eks.amazonaws.com/role-arn" not in annotations


def test_aws_install_mounts_irsa_annotation_only() -> None:
    """values-eks.yaml — IRSA annotation present, no Azure SP envs."""
    rc, stdout, _ = _render(
        "test-rich-otter",
        values_files=[str(CHART_PATH / "values-eks.yaml")],
        extra_sets={
            # Override cloud.provider in case file ordering matters on
            # this helm version.
            "cloud.provider": "aws",
            "cloud.awsRoleArn": "arn:aws:iam::123456789012:role/nodemedic-agent",
        },
    )
    assert rc == 0
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    [container] = deploy["spec"]["template"]["spec"]["containers"]
    env_names = {e["name"] for e in container["env"]}
    assert "AZURE_TENANT_ID" not in env_names
    assert "AZURE_CLIENT_ID" not in env_names
    assert "AZURE_CLIENT_SECRET" not in env_names

    [sa] = _by_kind(docs, "ServiceAccount")
    annotations = sa["metadata"]["annotations"]
    assert annotations["eks.amazonaws.com/role-arn"].startswith("arn:aws:iam::")


# ---------------------------------------------------------------------------
# Runbook-lint Job (AC-15 install-time gate)
# ---------------------------------------------------------------------------


def test_runbook_lint_job_present() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    jobs = _by_kind(docs, "Job")
    assert len(jobs) == 1
    job = jobs[0]
    assert job["metadata"]["name"].endswith("-runbook-lint")
    assert job["metadata"]["annotations"]["helm.sh/hook"] == "pre-install,pre-upgrade"
    [container] = job["spec"]["template"]["spec"]["containers"]
    args = " ".join(container.get("args", []))
    assert "/app/tests/check_runbook.sh" in args
    assert "/app/prompts/runbook.md" in args


# ---------------------------------------------------------------------------
# Security context
# ---------------------------------------------------------------------------


def test_deployment_runs_as_nonroot() -> None:
    _, stdout, _ = _render("cf1z")
    docs = _parse_docs(stdout)
    [deploy] = _by_kind(docs, "Deployment")
    pod_sec = deploy["spec"]["template"]["spec"]["securityContext"]
    assert pod_sec["runAsNonRoot"] is True

    [container] = deploy["spec"]["template"]["spec"]["containers"]
    csec = container["securityContext"]
    assert csec["allowPrivilegeEscalation"] is False
    assert csec["readOnlyRootFilesystem"] is True
    assert csec["capabilities"]["drop"] == ["ALL"]
