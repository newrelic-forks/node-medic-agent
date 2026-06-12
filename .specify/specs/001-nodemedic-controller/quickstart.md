# Quickstart — NodeMedic Controller

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-12

A validation guide, not an implementation walkthrough. Each scenario maps to one or more acceptance criteria (AC-1..AC-10) in [`spec.md`](./spec.md) §8 and exists to prove the controller behaves as the spec promises **end-to-end on a real cluster**. Steps assume you have `kubectl` on `PATH`, EUP-approved kubeconfig contexts for the test clusters, and a running local Go toolchain.

> **Constitution Article I.9 reminder.** All steps run on `test-*` clusters only. Never on `stg-*`, `us-*`, or `eu-*`.

---

## Prerequisites

- Go 1.25.x (`go version`)
- `kubectl` 1.28+ with contexts configured: `test-odd-wire` (AWS/EKS) and `cf1z` (the Azure kubeadm test cluster — legacy CF naming, sister to jc1z / sk1z).
- `helm` v3.14+
- `setup-envtest` for envtest-based tests: `go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest`
- A Slack incoming webhook URL pointing at `#nodemedic-demo` (test channel; do NOT reuse a production webhook).
- A bearer token for the agent service (`nodemedic-agent-token`). For Day-1-AM stub testing, any non-empty string works.
- Read access to [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml) and [`contracts/post-diagnose.md`](./contracts/post-diagnose.md).

---

## Setup

### 1. Build the controller binary

The controller lives in this repo (the NPD fork). Build with the `nodemedic-*` Makefile targets so NPD's targets stay separate.

```sh
cd ~/Documents/container-fabric/node-medic-agent
git checkout hackathon-2026/scope2-controller
make nodemedic-generate          # controller-gen object + crd + rbac
make nodemedic-test              # unit + envtest
make nodemedic-docker-build IMG=ghcr.io/cf/nodemedic-controller:dev
```

(For local dev without a registry, `make nodemedic-build` produces `bin/nodemedic-controller`; mount it into a kind cluster or run out-of-cluster against a real apiserver via `KUBECONFIG`.)

### 2. Install the CRD

```sh
kubectl --context=test-odd-wire apply -f .specify/specs/001-nodemedic-controller/contracts/nhd-crd.yaml
kubectl --context=test-odd-wire get crd nodehealthdiagnosisais.nodemedic.cf.newrelic.com
```

This same CRD applies on the Azure cluster — Constitution Article II.7 (one binary, two clouds).

### 3. Install Helm chart on EKS test cluster

```sh
kubectl --context=test-odd-wire create namespace container-fabric --dry-run=client -o yaml | kubectl apply -f -

kubectl --context=test-odd-wire -n container-fabric create secret generic nodemedic-slack \
  --from-literal=webhook-url="$SLACK_WEBHOOK_URL"

kubectl --context=test-odd-wire -n container-fabric create secret generic nodemedic-agent-token \
  --from-literal=token="$AGENT_TOKEN"

helm --kube-context=test-odd-wire upgrade --install nodemedic-controller \
  ./deployment/helm/nodemedic-controller \
  -n container-fabric \
  -f deployment/helm/nodemedic-controller/values-eks.yaml \
  --set clusterName=test-odd-wire
```

The chart's `_helpers.tpl` will refuse to install if `clusterName` doesn't start with `test-`. (Constitution Article I.9 hard guard — see plan §"Constitution Check" I.9.)

### 4. Verify the controller is healthy

```sh
kubectl --context=test-odd-wire -n container-fabric get pods -l app=nodemedic-controller
kubectl --context=test-odd-wire -n container-fabric logs -l app=nodemedic-controller --tail=50
kubectl --context=test-odd-wire -n container-fabric port-forward svc/nodemedic-controller 9443:9443 &
curl -s localhost:9443/metrics | grep nodemedic_
```

Expected: 1 ready pod, log line `Starting Controller` for `NodeHealthDiagnosisAI`, and `nodemedic_*` metrics registered (NFR-3).

---

## Validation scenarios

### Scenario A — Happy path (AC-1, AC-2, AC-3)

Map to spec §4 "Walkthrough — happy path".

1. From the Scope 1 fault-injection harness on the same EKS cluster:
   ```sh
   make inject-conntrack CLUSTER=test-odd-wire NODE=<some-node-name>
   ```
2. Within **5 s**: an NHD CR appears.
   ```sh
   kubectl --context=test-odd-wire -n container-fabric get nhd
   # NAME                                    PHASE       NODE                       PROVIDER   ...
   ```
   Verify `spec.case.provider=aws`, `spec.case.region=us-east-2`, `spec.case.instanceId=i-...` are all populated (FR-2).
3. Within **~55 s**: the agent (or a stub agent that hand-writes the canned diagnosis from research R-5) writes `status.diagnosis` and `status.phase=Diagnosed`.
4. Within **5 s of step 3**: `kubectl get node <name> -o jsonpath='{.spec.unschedulable}'` is `true` and `kubectl get nhd <name> -o jsonpath='{.status.action.decision}'` is `Applied`.
5. A Slack message lands in `#nodemedic-demo` with the format defined in [data-model.md §8](./data-model.md#8-internal-entity-slack-message-envelope).

**Pass criteria**: end-to-end ≤ 65 s. NHD `Acted`. Node `SchedulingDisabled`. Slack post visible.

### Scenario B — Gate failure (AC-4)

Map to spec §4 "Walkthrough — gate failure".

1. Hand-edit a stub NHD's `status.diagnosis` to `confidence: 0.5, evidence: [{source: nrql}, {source: ssh}], recommendation: {action: Cordon}`:
   ```sh
   kubectl --context=test-odd-wire -n container-fabric edit nhd <name>
   # set status.phase=Diagnosed, fill status.diagnosis as above
   ```
2. Within **5 s**: controller's reconciler runs the gate, finds `confidence < 0.7`, sets `status.action.decision=HumanInLoop`, `phase=Acted`, posts a Slack message with the "needs human review" framing.
3. Verify: `kubectl get node <n> -o jsonpath='{.spec.unschedulable}'` is **empty** (or `false`). Node was NOT cordoned.

**Pass criteria**: HumanInLoop decision recorded, no cordon, distinct Slack styling.

### Scenario C — Agent timeout (AC-5)

Map to spec §4 "Walkthrough — agent timeout".

1. Configure the controller's `--agent-url` to point at a black-hole endpoint (e.g. a `nginx` Pod returning 202 then never updating any CR):
   ```sh
   helm --kube-context=test-odd-wire upgrade nodemedic-controller ... \
     --set agentUrl=http://blackhole.container-fabric.svc:8080/diagnose
   ```
2. Inject a fault as in Scenario A.
3. Wait for the deadline: `spec.budgets.deadline = observedAt + 60s`. After ~60 s, the controller marks the NHD `Failed{reason=DeadlineExceeded}` and re-issues `POST /diagnose` once with the same `caseId` (FR-7).
4. After the second attempt also fails: terminal `Failed`, Slack `Critical` framing posts.
5. Verify: Node is NOT cordoned at any point. Slack receives one `Critical` message. NHD `status.phase=Failed`.

**Pass criteria**: terminal `Failed`, single retry, no cordon.

### Scenario D — Debounce (AC-6)

Inject the same fault twice within 30 s on the same node. Verify only one NHD is created (FR-1, FR-3 idempotent name). The second injection is dropped by the debounce window.

```sh
make inject-conntrack CLUSTER=test-odd-wire NODE=node-a
sleep 5
make inject-conntrack CLUSTER=test-odd-wire NODE=node-a
kubectl --context=test-odd-wire -n container-fabric get nhd | grep node-a
# expect exactly one row
```

### Scenario E — Self-heal (AC-7)

Map to spec §FR-10.

1. Run Scenario A through `Acted` (node cordoned).
2. Manually flip the watched condition back to False (e.g. `kubectl patch node <n> --type=json -p='[{"op": "replace", "path": "/status/conditions/<idx>/status", "value": "False"}]'` or wait for NPD to clear it naturally).
3. Verify: controller emits a `ConditionCleared` Event on the Node, but does **NOT** uncordon and does **NOT** mutate the NHD.

```sh
kubectl --context=test-odd-wire describe node <n> | grep ConditionCleared
kubectl --context=test-odd-wire get node <n> -o jsonpath='{.spec.unschedulable}'
# still true
```

### Scenario F — Azure parity (AC-8)

Repeat Scenarios A through C on the Azure kubeadm test cluster:

```sh
helm --kube-context=cf1z upgrade --install nodemedic-controller \
  ./deployment/helm/nodemedic-controller \
  -n container-fabric \
  -f deployment/helm/nodemedic-controller/values-azure.yaml \
  --set clusterName=cf1z
```

Verify the NHD's `spec.case.provider=azure`, `spec.case.region=eastus2` (or your cluster's region), `spec.case.instanceId=<VM name>`. Same code path, different fixture. Constitution Article II.7 acceptance.

### Scenario G — Restart resilience (AC-9)

1. Start Scenario A. As soon as `phase=Diagnosing` is observed, kill the controller pod:
   ```sh
   kubectl --context=test-odd-wire -n container-fabric delete pod -l app=nodemedic-controller
   ```
2. The Deployment restarts. The new pod's reconciler picks up the existing NHD by informer cache rebuild.
3. Verify: case completes successfully (cordon + Slack), with at most one duplicate Slack post (acceptable per FR-11).

### Scenario H — Metrics surface (AC-10)

After running Scenarios A–G:

```sh
kubectl --context=test-odd-wire -n container-fabric port-forward svc/nodemedic-controller 9443:9443 &
curl -s localhost:9443/metrics | grep nodemedic_
```

Expect non-zero counters for:
- `nodemedic_cases_total{outcome="Applied"}`
- `nodemedic_cases_total{outcome="HumanInLoop"}`
- `nodemedic_cases_total{outcome="Failed"}`
- `nodemedic_phase_duration_seconds_bucket{...}` histogram
- `nodemedic_agent_post_total{result="ok"}`
- `nodemedic_cordon_total{result="ok"}`
- `nodemedic_slack_post_total{...}`

---

## Test commands (Day-1-AM stub deliverable, research R-14)

Before the agent integration is live, exercise the controller against canned NHDs:

```sh
# Apply a hand-canned NHD with phase=Diagnosed and a known-good diagnosis
kubectl --context=test-odd-wire -n container-fabric apply -f test/nodemedic/fixtures/nhd-applied.yaml

# Watch the reconciler
kubectl --context=test-odd-wire -n container-fabric get nhd -w

# Apply a known-bad diagnosis (gate fail)
kubectl --context=test-odd-wire -n container-fabric apply -f test/nodemedic/fixtures/nhd-humaninloop.yaml
```

This is the integration handshake with Scope 1 (fault injection) and Scope 3 (agent).

---

## Cleanup

```sh
helm --kube-context=test-odd-wire uninstall nodemedic-controller -n container-fabric
kubectl --context=test-odd-wire -n container-fabric delete nhd --all
# CRD persists due to helm.sh/resource-policy: keep — delete by hand if needed:
kubectl --context=test-odd-wire delete crd nodehealthdiagnosisais.nodemedic.cf.newrelic.com
# Manually uncordon any nodes left cordoned:
kubectl --context=test-odd-wire uncordon <node-name>
```

---

## Pointers

- Acceptance criteria (gate for "v1 ships"): [`spec.md`](./spec.md) §8
- Cross-scope contracts: [`contracts/nhd-crd.yaml`](./contracts/nhd-crd.yaml), [`contracts/post-diagnose.md`](./contracts/post-diagnose.md)
- Internal types and field-level rules: [`data-model.md`](./data-model.md)
- Demo flow narrative: `docs/cf/nodemedic.md` §4
- Constitution: `.specify/memory/constitution.md` v1.0
