# Quickstart — NodeMedic Agent

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-13

A validation guide, not an implementation walkthrough. Each scenario maps to one or more acceptance criteria (AC-1 through AC-15) in [`spec.md`](./spec.md) §8 and exists to prove the agent behaves as the spec promises **end-to-end on a real cluster**.

> **Constitution Article I.5 reminder.** All steps run on `cf1z`, `jc1z`, `sk1z`, or `test-*` clusters only. Never on `stg-*`, `us-*`, or `eu-*`.

---

## Prerequisites

### Tooling on the dev host
- Python 3.12.x (`python3.12 --version`).
- `uv` (`pip install uv` if missing — used per the user's Python preferences in CLAUDE.md).
- `kubectl` 1.33.x (matches cf1z's k8s 1.33.8); EUP-approved kubeconfig context for `cf1z`.
- `helm` v3.14+.
- **Colima** for image builds — Docker Desktop is blocked by NR's corp subscription posture on this machine. Bring up an arm64 VM with `colima start --arch aarch64 --vm-type vz --cpu 4 --memory 8 --disk 60`.
- Developer credentials for `cf-registry.nr-ops.net` (`docker login cf-registry.nr-ops.net` with email-as-username; verified empirically 2026-06-14).

### Cluster prerequisites (cf1z)
- `cf-monitoring` namespace exists.
- `nodemedic-controller` Deployment running with `--stub-agent=false` after the agent ships (currently `--stub-agent=true` per `deployment/helm/nodemedic-controller/values-azure.yaml`).
- `hack-node-problem-detector` DaemonSet running in `cf-monitoring` (verified live 2026-06-13/14).
- Canary node `cf1z-general-nodes-2000002` labeled `canary-chaos-test=true`.
- Two chaos cronjobs deployed in `default` namespace: `chaos-containerd-unhealthy` and `chaos-kubelet-unhealthy` (both verified end-to-end 2026-06-14).

### Secrets to apply once per cluster install
Per [`manifests/README.md`](./manifests/README.md) — patch placeholders before applying:

```sh
kubectl --context=cf1z apply -f .specify/specs/002-nodemedic-agent/manifests/secret-anthropic-token.yaml
kubectl --context=cf1z apply -f .specify/specs/002-nodemedic-agent/manifests/secret-nr-token.yaml
kubectl --context=cf1z apply -f .specify/specs/002-nodemedic-agent/manifests/secret-ssh-key.yaml
kubectl --context=cf1z apply -f .specify/specs/002-nodemedic-agent/manifests/secret-azure-creds.yaml   # cf1z is Azure
# (skip serviceaccount-irsa.yaml — that's AWS-cluster only)
```

Vault retrievals for the placeholders:
- `secret-anthropic-token.yaml` → `newrelic-vault us read -field=value containers/teams/nova/staging/nova-service/NERD_COMPLETION_API_TOKEN`.
- `secret-nr-token.yaml` → user-service NR API key for staging account `1` (Nova team's standard distribution path).
- `secret-ssh-key.yaml` → hackathon-only Ed25519 keypair (private key here; public half on cf1z worker AMIs out-of-band).
- `secret-azure-creds.yaml` → Service Principal client-secret tuple, Reader on cf1z's resource group.

---

## Setup

### 1. Build the agent image

The agent lives in this repo alongside NPD and the controller. Build via the `nodemedic-agent-*` Makefile targets (parallel to the controller's `nodemedic-*`):

```sh
cd ~/Downloads/node-medic-agent
git checkout hackathon-2026/cf1z-baseline

# Sync Python deps once
uv sync --all-extras --dev

# Build + push the agent image (Colima must be running)
SHA=$(git rev-parse --short=8 HEAD)
make nodemedic-agent-docker-build TAG=dev-cf1z-$SHA
make nodemedic-agent-docker-push  TAG=dev-cf1z-$SHA
```

**Checkpoint**: `docker images cf-registry.nr-ops.net/container-fabric/nodemedic-agent:dev-cf1z-$SHA` shows the image. Push completes without authentication prompts.

### 2. Run unit + integration tests locally

```sh
uv run pytest tests/nodemedic_agent/ -v
bash tests/nodemedic_agent/check_runbook.sh prompts/runbook.md
helm lint deployment/helm/nodemedic-agent --values deployment/helm/nodemedic-agent/values-azure.yaml --set clusterName=cf1z
```

**Checkpoint** (AC-15 build-time gate): `check_runbook.sh` exits 0; all seven required clauses present in `prompts/runbook.md`. If any clause is missing, `helm install` will also fail downstream — fix the runbook before continuing.

### 3. Install the chart on cf1z

The CRD is already installed by the controller's chart (`helm.sh/resource-policy: keep`). The agent chart does NOT install the CRD — only the controller does. The agent chart's runbook-lint pre-install Job is the AC-15 install-time gate.

```sh
helm --kube-context=cf1z upgrade --install nodemedic-agent \
  ./deployment/helm/nodemedic-agent \
  -n cf-monitoring \
  -f deployment/helm/nodemedic-agent/values-azure.yaml \
  --set clusterName=cf1z \
  --set image.tag=dev-cf1z-$SHA \
  --wait
```

The chart's `_helpers.tpl` will refuse to install if `clusterName` is not in `{cf1z, jc1z, sk1z}` or doesn't start with `test-`. Constitution Article I.5 hard guard.

**Checkpoint** (AC-1):

```sh
kubectl --context=cf1z -n cf-monitoring get deploy nodemedic-agent
# NAME              READY   UP-TO-DATE   AVAILABLE
# nodemedic-agent   1/1     1            1

kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --tail=20
# expect:
#   {"event":"model_resolved","primary":"claude-opus-4-7","fallback":"claude-sonnet-4-6","resolution_path":"exact"}
#   {"event":"startup_complete","listen_addr":":8080"}

kubectl --context=cf1z -n cf-monitoring port-forward svc/nodemedic-agent 8080:8080 &
curl -s localhost:8080/healthz   # {"status":"ok"}
curl -s localhost:8080/readyz    # {"status":"ok","model_primary":"claude-opus-4-7","model_fallback":"claude-sonnet-4-6"}
```

### 4. Flip the controller off stub mode

```sh
helm --kube-context=cf1z upgrade nodemedic-controller \
  ./deployment/helm/nodemedic-controller \
  -n cf-monitoring \
  -f deployment/helm/nodemedic-controller/values-azure.yaml \
  --set clusterName=cf1z \
  --set config.stubAgent=false
```

The controller now reaches the agent at `http://nodemedic-agent.cf-monitoring.svc:8080/diagnose` (already configured in `values-azure.yaml:21`).

---

## Validation scenarios

Each scenario maps explicitly to spec ACs. Pass criteria in **bold**.

### Scenario A — Receive contract (AC-2)

```sh
curl -s -X POST localhost:8080/diagnose \
  -H 'Content-Type: application/json' \
  -d @.specify/specs/002-nodemedic-agent/contracts/case_azure_inline.json | jq

# Missing field path
curl -s -X POST localhost:8080/diagnose \
  -H 'Content-Type: application/json' \
  -d '{"caseId":"8f3c1e2a-9b4d-4f7a-9c1e-2a9b4d4f7a9c"}' | jq

# Wrong provider enum
curl -s -X POST localhost:8080/diagnose \
  -H 'Content-Type: application/json' \
  -d '{"caseId":"...","provider":"gcp",...}' | jq

# Verify auth header is ignored (FR-1)
curl -s -X POST localhost:8080/diagnose \
  -H 'Authorization: Bearer junk' \
  -H 'Content-Type: application/json' \
  -d @case_azure_inline.json | jq
```

(Use the golden body from [`contracts/post-diagnose.md`](./contracts/post-diagnose.md) for valid request shapes.)

**Pass**: First curl returns `202 {caseId, status: "queued"}`. Missing-field returns `400` with Pydantic error JSON. Wrong-enum returns `400`. Auth-header curl behaves identically to the no-auth curl (header ignored).

### Scenario B — Containerd happy path (AC-3)

Mirrors spec §9 demo flow §1–§9.

```sh
# Trigger the deployed chaos cronjob — bind-mounts a regular file over the
# in-pod containerd socket on cf1z-general-nodes-2000002 for 90 s.
kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy \
  chaos-containerd-test-manual -n default

# Watch NPD detect the condition flip (~30 s probe interval).
kubectl --context=cf1z get node cf1z-general-nodes-2000002 \
  -o jsonpath='{.status.conditions[?(@.type=="ContainerRuntimeUnhealthy")]}' --watch

# Watch the agent's case lifecycle.
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent \
  --tail=200 -f | jq 'select(.case_id != null)'
```

In a separate terminal, watch the NHD:

```sh
kubectl --context=cf1z -n cf-monitoring get nhd -w
```

**Pass** (AC-3): the agent's `status.diagnosis` lands on the CR with:
- `confidence ≥ 0.7`
- `evidence` containing **at least 2 distinct sources** (typically `kubectl` + `ssh` + `nrql` + `cloud`)
- `recommendation.action = Cordon`
- `phase = Diagnosed`
- The diagnosis lands before the controller's first-attempt deadline (~60 s) — if it lands later, FR-12 governs and Scenario E covers that path.

### Scenario C — Kubelet happy path (AC-3b)

```sh
kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy \
  chaos-kubelet-test-manual -n default

# Same watch commands as Scenario B; trigger.type now KubeletUnhealthy.
```

**Pass** (AC-3b): same shape as Scenario B; diagnosis cites at least one NRQL row, one SSH probe (e.g. `journalctl -u kubelet --since "5 min ago"`), and one kube-side or cloud-side probe (e.g. `kubectl get events --field-selector involvedObject.name=cf1z-general-nodes-2000002`).

### Scenario D — Single-source evidence accepted (AC-6)

This is a controller-side gate-fail demo path. The agent's behavior is to accept the report — the gate is downstream.

Hand-craft a low-evidence diagnosis by patching the CRD directly during a case (or by running the agent with an environment variable that forces a thin emit_report):

```sh
# Option 1: post a request and let the agent gather minimum evidence by
# tightening its budget so the loop ends early. (Requires runbook tweak in tests.)

# Option 2: apply a hand-canned thin report to a fresh NHD via a Python helper
# that exercises the emit_report tool path with the gate-fail golden.
uv run python -m tests.nodemedic_agent.helpers.emit_thin_report \
  --case-id <id> --nhd-name <name>
```

**Pass** (AC-6): NHD `status.diagnosis` lands with `evidence` length 1, `confidence: 0.4`, `recommendation.action: NoAction`. Controller's confidence gate routes to `HumanInLoop` (no cordon). Slack message has the "needs human review" framing (controller-side check).

### Scenario E — Late-write race (AC-7)

```sh
# Start a real case via Scenario B's chaos trigger.
kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy \
  chaos-containerd-late-test -n default

# As soon as the agent logs `case_started`, hand-patch the CR's phase to Failed:
NHD_NAME=$(kubectl --context=cf1z -n cf-monitoring get nhd \
  -l ... -o jsonpath='{.items[-1].metadata.name}')
kubectl --context=cf1z -n cf-monitoring patch nhd $NHD_NAME \
  --subresource=status --type=merge \
  -p '{"status":{"phase":"Failed","conditions":[{"type":"ReportReady","status":"False","reason":"DeadlineExceeded","lastTransitionTime":"'"$(date -u +%FT%TZ)"'"}]}}'

# Wait for the agent to finish its loop. Watch the deferred-write log line.
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent \
  --tail=500 | jq 'select(.event == "deferred_write_phase_conflict")'
```

**Pass** (AC-7):
- The CR retains `phase=Failed` after the agent's eventual `emit_report`.
- Structured stdout log contains exactly one `deferred_write_phase_conflict` line with `case_id`, `observed_phase=Failed`, and the intended payload.
- No agent-side metric reports a write failure (the case-complete log line shows `final_phase=Diagnosed_deferred` or similar — exact field name in data-model.md §5).

### Scenario F — Idempotency (AC-8)

```sh
# Post the same body twice rapidly during an in-flight case.
curl -s -X POST localhost:8080/diagnose -H 'Content-Type: application/json' \
  -d @case_azure_inline.json &
curl -s -X POST localhost:8080/diagnose -H 'Content-Type: application/json' \
  -d @case_azure_inline.json

# Then count case_started log lines for that caseId.
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent \
  --tail=500 | jq 'select(.case_id == "1a2b3c4d-5e6f-4789-9abc-def012345678" and .event == "case_started")' | wc -l
```

**Pass** (AC-8): exactly one `case_started` line. Both POSTs return `202 {status: "queued"}`.

### Scenario G — Tool-call observability (AC-9)

After Scenario B or C completes, run:

```sh
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent \
  --since=5m | jq 'select(.case_id == "<the-case-id>")'
```

**Pass** (AC-9): every tool call from the loop is present — every `Bash` invocation has its `command` field, every NR MCP call has its `args`, every SDK built-in call (`Read`, `Write`, etc.) is logged. The `case_complete` log line appears last.

### Scenario H — Concurrency cap (AC-10)

Override `MAX_CONCURRENT_CASES` to 2 for a controlled test:

```sh
helm --kube-context=cf1z upgrade nodemedic-agent ./deployment/helm/nodemedic-agent \
  -n cf-monitoring \
  -f deployment/helm/nodemedic-agent/values-azure.yaml \
  --set clusterName=cf1z \
  --set image.tag=dev-cf1z-$SHA \
  --set config.maxConcurrentCases=2

# Post 3 cases simultaneously with distinct caseIds.
for i in 1 2 3; do
  jq --arg id "00000000-0000-4000-8000-00000000000$i" '.caseId=$id' case_azure_inline.json | \
    curl -s -X POST localhost:8080/diagnose -H 'Content-Type: application/json' -d @- &
done
wait
```

**Pass** (AC-10): exactly one of the three POSTs returns `429`. The other two return `202 {status: "queued"}`. After completion, restore `MAX_CONCURRENT_CASES=32`.

### Scenario I — Kube RBAC scoping (AC-11)

Verify the rendered Helm chart has no broader role than FR-14 specifies:

```sh
helm template nodemedic-agent ./deployment/helm/nodemedic-agent \
  -f deployment/helm/nodemedic-agent/values-azure.yaml \
  --set clusterName=cf1z | grep -A 30 'kind: ClusterRole'

# Confirm:
#   - rules include 'nodehealthdiagnosisais' (verbs: get/list/watch/update)
#   - rules include 'nodehealthdiagnosisais/status' (verb: update)
#   - rules include 'nodes' (verbs: get/list/watch only — NO patch)
#   - rules include 'pods' / 'events' (verbs: get/list/watch)
#   - NO 'create', 'delete', 'patch' on 'nodes'

# Confirm at runtime:
kubectl --context=cf1z -n cf-monitoring auth can-i patch nodes \
  --as=system:serviceaccount:cf-monitoring:nodemedic-agent
# expected: no
kubectl --context=cf1z -n cf-monitoring auth can-i update nodehealthdiagnosisais/status \
  --as=system:serviceaccount:cf-monitoring:nodemedic-agent
# expected: yes
```

**Pass** (AC-11): all `auth can-i` checks return the expected yes/no. No verb beyond FR-14 is granted.

### Scenario J — Cross-cloud probing prevented (AC-13)

cf1z is Azure, so `aws` CLI calls fail (no IRSA, no AWS credentials mounted):

```sh
kubectl --context=cf1z -n cf-monitoring exec deploy/nodemedic-agent -- \
  aws ec2 describe-instance-status --region us-east-2
# expected: "Unable to locate credentials" or similar
```

**Pass** (AC-13): the `aws` call fails. Verifies FR-6's credential-layer-only enforcement of cloud dispatch.

(For an EKS-side check, repeat on a `test-*` AWS install — `az` calls there fail symmetrically. cf1z exercises one side; the other side is covered by spec AC-4 once an EKS test cluster is wired.)

### Scenario K — Case isolation (AC-14)

```sh
# Run the containerd path to completion.
kubectl --context=cf1z create job --from=cronjob/chaos-containerd-unhealthy \
  chaos-A -n default
# wait for Diagnosed
sleep 60

# Run the kubelet path on the same canary node.
kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy \
  chaos-B -n default
sleep 60

# Filter logs by each caseId; assert they reference disjoint trees.
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-agent --tail=2000 \
  | jq -s 'group_by(.case_id) | map({case_id: .[0].case_id, calls: length, tools: [.[].tool] | unique})'
```

**Pass** (AC-14): Case A and Case B have disjoint `tools` lists (or overlapping by tool *name* but not by *result content*). Case B's `status.diagnosis.evidence[].result` strings reference only kubelet-fault content, no containerd-fault content from Case A.

### Scenario L — Runbook readiness (AC-15)

Already enforced at build time by `tests/nodemedic_agent/check_runbook.sh` (Scenario 2) and at install time by the chart's pre-install Job. To verify after install:

```sh
kubectl --context=cf1z -n cf-monitoring get jobs -l app.kubernetes.io/component=runbook-lint
kubectl --context=cf1z -n cf-monitoring logs job/nodemedic-agent-runbook-lint
```

**Pass** (AC-15): pre-install Job exited 0; log shows all seven clauses passed. If any failed, `helm install` itself failed and Scenarios A–K never ran.

---

## End-to-end demo flow

The full demo flow (operator-visible, single take) lives in spec §9. The mapping from §9 steps to this file's scenarios:

| Spec §9 step | Quickstart scenario |
|---|---|
| 1–4 (operator triggers chaos, NPD flips condition, controller POSTs /diagnose) | Setup + Scenario B start |
| 5 (live tail of agent log) | Scenario G |
| 6 (`emit_report` accepted, CR `Diagnosed`) | Scenario B pass criteria |
| 7 (controller cordons after gate) | controller's quickstart Scenario A — out of scope here |
| 8 (operator reads CR) | `kubectl get nhd <name> -o yaml` |
| 9 (read structured stdout logs) | Scenario G |
| 10 (auto-recovery — condition flips back; cordon NOT auto-removed) | controller territory; agent is bystander |
| Second condition demo (KubeletUnhealthy) | Scenario C |

---

## Cleanup

```sh
helm --kube-context=cf1z uninstall nodemedic-agent -n cf-monitoring
# CRD persists due to controller chart's helm.sh/resource-policy: keep — left in place.
# Manually uncordon any nodes the controller cordoned:
kubectl --context=cf1z uncordon cf1z-general-nodes-2000002

# Re-set controller to stub mode if you want to free the agent service path:
helm --kube-context=cf1z upgrade nodemedic-controller \
  ./deployment/helm/nodemedic-controller -n cf-monitoring \
  -f deployment/helm/nodemedic-controller/values-azure.yaml \
  --set clusterName=cf1z \
  --set config.stubAgent=true
```

Per-cluster Secrets (`nodemedic-anthropic-token`, `nodemedic-nr-token`, `nodemedic-ssh-key`, `nodemedic-azure-creds`) are intentionally left in place — they aren't owned by the chart, and they survive `helm uninstall` for the next install cycle.

---

## Pointers

- Acceptance criteria (the gate for "v1 ships"): [`spec.md`](./spec.md) §8 (AC-1 through AC-15)
- Cross-scope contracts: [`contracts/post-diagnose.md`](./contracts/post-diagnose.md), [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml), [`contracts/emit-report-tool.md`](./contracts/emit-report-tool.md)
- Internal types and field-level rules: [`data-model.md`](./data-model.md)
- Demo flow narrative: [`spec.md`](./spec.md) §9
- Constitution: [`.specify/memory/constitution.md`](../../memory/constitution.md)
- Plan: [`plan.md`](./plan.md)
- Phase 0 research: [`research.md`](./research.md)
- Per-cluster Secret manifests: [`manifests/`](./manifests/)
- Spec 001 (controller) quickstart for the upstream side of the contract: [`../001-nodemedic-controller/quickstart.md`](../001-nodemedic-controller/quickstart.md)
