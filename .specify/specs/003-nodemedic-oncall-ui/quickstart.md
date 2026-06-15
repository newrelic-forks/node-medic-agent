# Quickstart — NodeMedic On-Call UI + Slack Format Upgrade

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-14

A validation guide, not an implementation walkthrough. Each scenario maps to one or more acceptance criteria (AC-1 through AC-14, plus AC-14a/b/c) in [`spec.md`](./spec.md) §8 and exists to prove the feature behaves as the spec promises **end-to-end on cf1z**.

> **Constitution Article I.5 reminder.** All steps run on `cf1z`, `jc1z`, `sk1z`, or `test-*` clusters only. Never on `stg-*`, `us-*`, or `eu-*`.

---

## Prerequisites

### Tooling on the dev host
- Go 1.24.x (`go version`).
- `kubectl` 1.33.x with EUP-approved kubeconfig context for `cf1z`.
- `helm` v3.14+.
- **Colima** for image builds (Docker Desktop is blocked by NR's corp subscription posture). `colima start --arch aarch64 --vm-type vz --cpu 4 --memory 8 --disk 60`.
- Developer credentials for `cf-registry.nr-ops.net` (`docker login cf-registry.nr-ops.net` with email-as-username; verified empirically against the controller image push).

### Cluster prerequisites (cf1z)
Spec 001 (controller) and Spec 002 (agent) MUST already be deployed and stable. Verified live as of handoff (2026-06-14):

```sh
kubectl --context=cf1z -n cf-monitoring get deploy nodemedic-controller nodemedic-agent
# NAME                    READY   UP-TO-DATE   AVAILABLE
# nodemedic-controller    1/1     1            1
# nodemedic-agent         1/1     1            1
```

Other prerequisites already in place:
- `cf-monitoring` namespace exists.
- `nodemedic-controller` Deployment running with `--stub-agent=false`.
- `chaos-kubelet-unhealthy` cronjob deployed in `default` namespace (the case-generator the demo uses).
- `machine-lifecycle-controller` honors the `skipDeletion` annotation (image `pullrequest-466` per handoff).
- A Slack incoming webhook is configured in `Secret/nodemedic-slack-webhook` in `cf-monitoring`. Channel routing is in the secret; the display string flips via `config.slackChannel`.

### No new Secrets

Spec 003 introduces NO new credential surface. The UI's ServiceAccount is its only authentication input; everything goes through in-cluster RBAC. No Anthropic token, no NR API token, no Azure SP, no AWS IRSA. The Slack webhook secret is owned by the controller chart, not the UI chart.

---

## Setup

### 1. Build the UI image

```sh
cd ~/Downloads/node-medic-agent
git checkout hackathon-2026/cf1z-baseline

# Run unit + integration tests first
go test ./cmd/nodemedic-oncall-ui/... ./internal/oncall/... ./internal/nodemedic/notifier/... ./tests/oncall_ui/...

# Build + push the UI image (Colima must be running)
SHA=$(git rev-parse --short=8 HEAD)
make nodemedic-oncall-ui-docker-build TAG=dev-cf1z-$SHA
make nodemedic-oncall-ui-docker-push  TAG=dev-cf1z-$SHA
```

**Checkpoint**: `docker images cf-registry.nr-ops.net/container-fabric/nodemedic-oncall-ui:dev-cf1z-$SHA` shows the image; push completes without authentication prompts.

### 2. Lint the chart + RBAC guard

```sh
helm lint deployment/helm/nodemedic-oncall-ui --set clusterName=cf1z

# Constitution Article I.1 + I.2 floor — these MUST be silent (exit 0):
! grep -E 'nodes/(delete|create)|secrets|configmaps|/finalizers|\*' \
    deployment/helm/nodemedic-oncall-ui/templates/clusterrole.yaml
! grep 'pods/eviction' deployment/helm/nodemedic-controller/templates/clusterrole.yaml
! grep 'nodes/patch'   deployment/helm/nodemedic-agent/templates/clusterrole.yaml
```

**Checkpoint** (AC-11 + AC-12): chart lints clean; the three negated greps all return exit 0 (no forbidden verbs).

### 3. Install the UI chart on cf1z

```sh
helm --kube-context=cf1z upgrade --install nodemedic-oncall-ui \
  ./deployment/helm/nodemedic-oncall-ui \
  -n cf-monitoring \
  -f deployment/helm/nodemedic-oncall-ui/values-azure.yaml \
  --set clusterName=cf1z \
  --set image.tag=dev-cf1z-$SHA \
  --set config.uiBaseURL=http://localhost:8080 \
  --set config.slackChannel=#cf-oncall-alerts \
  --wait
```

The chart's `_helpers.tpl` will refuse to install if `clusterName` is not in `{cf1z, jc1z, sk1z}` or doesn't start with `test-` — Constitution Article I.5 hard guard. (AC-11.)

**Checkpoint** (Setup health):

```sh
kubectl --context=cf1z -n cf-monitoring get deploy nodemedic-oncall-ui
# NAME                    READY   UP-TO-DATE   AVAILABLE
# nodemedic-oncall-ui     1/1     1            1

kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-oncall-ui --tail=20
# expect:
#   {"event":"startup_complete","listen_addr":":8080","cluster_name":"cf1z","drain_concurrency":3,"audit_buffer_size":20}

kubectl --context=cf1z -n cf-monitoring port-forward svc/nodemedic-oncall-ui 8080:8080 &
curl -s localhost:8080/ | head -20            # HTML list-page output
curl -s localhost:8080/api/cases | jq         # JSON list shape
```

### 4. Flip the controller to Block Kit

Spec 003's Slack-format change ships behind a feature flag. Default is `true` once the new builders land; a flip back to `false` is the demo-day rollback.

```sh
helm --kube-context=cf1z upgrade nodemedic-controller \
  ./deployment/helm/nodemedic-controller \
  -n cf-monitoring \
  -f deployment/helm/nodemedic-controller/values-azure.yaml \
  --set clusterName=cf1z \
  --set config.useBlockKit=true \
  --set config.uiBaseURL=http://localhost:8080
```

The controller now produces Block Kit JSON instead of plain-text on every NHD `phase=Acted` / `phase=Failed` post.

---

## Validation scenarios

Each scenario maps explicitly to spec ACs. Pass criteria in **bold**.

### Scenario A — Block Kit Slack post on `Applied` (AC-1)

```sh
# Trigger a chaos run on cf1z. NPD flips KubeletUnhealthy after ~30 s.
kubectl --context=cf1z create job --from=cronjob/chaos-kubelet-unhealthy \
  chaos-kubelet-test-A -n default

# Watch the controller's notifier log line:
kubectl --context=cf1z -n cf-monitoring logs -l app.kubernetes.io/name=nodemedic-controller \
  --tail=200 -f | jq 'select(.event == "slack_post")'
```

**Pass** (AC-1): the configured Slack channel receives a Block Kit message with:
- Header `🚨 Cordoned: <node> (cf1z)`
- `attachment.color = "danger"` (red side bar)
- 4-field grid with Decision/Confidence/Trigger/rcaCategory
- Primary button "View full diagnosis" linking to `http://localhost:8080/cases/<nhd-name>`
- Top-level `text` fallback present (visible in the Slack unfurl preview if the message is quoted in another channel).

### Scenario B — Block Kit Slack post on `HumanInLoop` (AC-2)

Hand-craft a low-confidence diagnosis that fails the controller's gate:

```sh
# Use the agent's helper that exercises emit_report with thin evidence (Spec 002 quickstart Scenario D).
uv run python -m tests.nodemedic_agent.helpers.emit_thin_report \
  --case-id <new-uuid> --nhd-name <new-nhd-name>

# OR: directly create an NHD that the controller will route through HumanInLoop.
```

**Pass** (AC-2): Slack receives a message with:
- Header `⚠️ Needs review: <node> (cf1z)`
- `attachment.color = "warning"` (yellow side bar)
- Same 4-field grid + button as Scenario A.

### Scenario C — List view renders 24h of NHDs (AC-3)

Pre-populate cf1z with at least 3 NHDs (run the chaos job 3 times across an hour, or use Scenario A's case + two hand-crafted CRs).

```sh
# Port-forward should already be up from Setup step 3.
open http://localhost:8080/        # macOS — opens in default browser
```

**Pass** (AC-3):
- The table renders all NHDs from the last 24h, sorted newest first.
- Columns: Created, Node, Cluster, Trigger, Phase, Decision, Confidence, View.
- Wait 30 s on the open page — table re-renders without a full page reload (auto-refresh fired). Confirm via browser devtools Network tab: a `GET /api/cases` request lands every 30 s.
- Click "View" on any row → land on `/cases/<nhd-name>`.

### Scenario D — Per-case view from Slack click-through (AC-4)

From the Block Kit message produced in Scenario A:

1. Click the "View full diagnosis" button.
2. Browser opens `http://localhost:8080/cases/<nhd-name>` (port-forward must be up).

**Pass** (AC-4): the per-case page renders:
- Case metadata header (case_id, node, cluster, provider, region, instance_id)
- Trigger metadata (type, reason, message, observedAt)
- Agent diagnosis (rootCause, rcaCategory, confidence, modelUsed, completedAt, recommendation.action+reason)
- Evidence list — entries longer than 800 chars render collapsed by default; "show more" toggle expands them
- Action history with one row: `Cordon (controller, <ts>)`
- Three action buttons: Uncordon Node (enabled — node is cordoned), Drain Node (enabled), Clear MLC skipDeletion (enabled — controller stamped the annotation alongside cordon)

### Scenario E — Uncordon button round-trip (AC-5)

From the per-case page in Scenario D:

1. Click "Uncordon Node".
2. Confirmation modal opens, showing equivalent `kubectl uncordon <node>` and current node state.
3. Click "Confirm".

**Pass** (AC-5):
- The page refreshes; the node's `.spec.unschedulable` is now `false`.
- Verify with `kubectl --context=cf1z get node <node> -o jsonpath='{.spec.unschedulable}'` → empty / `false`.
- The action history section now has 2 rows: `Cordon (controller, ...)` and `Uncordon (UI: demo-anonymous, ...)`.
- The Uncordon button is now disabled with tooltip "Already uncordoned".
- Verify the audit annotation:

```sh
kubectl --context=cf1z -n cf-monitoring get nhd <nhd-name> \
  -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | jq
# expect: [{"ts":"...","action":"uncordon","actor":"demo-anonymous","result":"ok"}]
```

### Scenario F — Idempotent uncordon (AC-6)

On the same per-case page, with the button now disabled, hand-craft a POST:

```sh
curl -s -X POST localhost:8080/api/cases/<nhd-name>/actions/uncordon | jq
```

**Pass** (AC-6): response is `{result: "already uncordoned (no change)"}`. The audit annotation length is **unchanged** (no new entry appended for the no-op).

### Scenario G — Clear skipDeletion (AC-7)

From the per-case page (still has `skipDeletion=true` since Scenario E only uncordoned):

1. Click "Clear MLC skipDeletion".
2. Confirmation modal opens.
3. Click "Confirm".

**Pass** (AC-7):
- Annotation removed:

```sh
kubectl --context=cf1z get node <node> \
  -o jsonpath='{.metadata.annotations.machine-lifecycle\.newrelic\.com/skipDeletion}'
# expect: empty
```

- MLC's next reconcile reclaims the VM (~30 s):

```sh
kubectl --context=cf1z get node <node>
# expect: NotFound after MLC reclaims
```

- Audit annotation now has 2 entries (uncordon + clear-skip-deletion).

### Scenario H — Drain button respects PDBs (AC-8)

Pick a different canary node with at least 2 non-DaemonSet pods. (For the demo run, optionally schedule a `nginx` Deployment with `replicas: 3` and a tight PDB on cf1z.)

1. Open the NHD's per-case page.
2. Click "Drain Node".
3. Confirmation modal lists the eligible pods (excludes DaemonSet, mirror, system-node-critical).
4. Click "Confirm Drain".
5. Observe the SSE stream — per-pod result lines tick by in the UI.

**Pass** (AC-8):
- Each pod's outcome surfaces inline (`evicted`, `skipped`, `error`).
- A PDB-violating pod returns `error` with detail like "would violate PDB foo-pdb"; the loop continues with the next pod.
- Terminal summary: `evicted=N skipped=M errored=K`.
- Audit annotation has a single new entry with `action: "drain"`, `result: "ok"` or `"partial"` (if any errors), `detail: "evicted=N skipped=M errored=K"`.

### Scenario I — Combined action history (AC-9 + AC-10)

After Scenarios E + G + H on the same NHD:

1. Open the per-case page.
2. Verify the "Action history" section.

**Pass** (AC-9 + AC-10):
- Three rows in timestamp order: `Cordon (controller, ...)`, `Uncordon (UI: demo-anonymous, ...)`, `ClearSkipDeletion (UI: demo-anonymous, ...)`.
- (If Scenario H also ran on this NHD: a fourth `Drain (UI: demo-anonymous, ...)` row.)

```sh
kubectl --context=cf1z -n cf-monitoring get nhd <nhd-name> \
  -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | jq 'length'
# expect: matches the count of UI-initiated actions written
```

### Scenario J — Chart cluster-name guard (AC-11)

```sh
helm template ./deployment/helm/nodemedic-oncall-ui --set clusterName=stg-going-plaid 2>&1
# expect: "ERROR: .Values.clusterName=\"stg-going-plaid\" must be one of cf1z/jc1z/sk1z or start with test-"

helm template ./deployment/helm/nodemedic-oncall-ui --set clusterName=cf1z >/dev/null
# expect: silent success
```

**Pass** (AC-11): first command exits non-zero with the constitution error message; second exits 0.

### Scenario K — RBAC review (AC-12)

```sh
helm template nodemedic-oncall-ui ./deployment/helm/nodemedic-oncall-ui \
  --set clusterName=cf1z | yq '.kind == "ClusterRole"'

# Confirm rules (the four rule blocks from research R-9):
#   - apiGroups: [""],     resources: [nodes],                                verbs: [get, list, watch, patch]
#   - apiGroups: [""],     resources: [pods],                                 verbs: [get, list, watch]
#   - apiGroups: [""],     resources: [pods/eviction],                       verbs: [create]
#   - apiGroups: [...],    resources: [nodehealthdiagnosisais],               verbs: [get, list, watch, patch]
#   - NO delete, NO create on nodes, NO secrets, NO configmaps, NO finalizers, NO *

# Confirm at runtime:
kubectl --context=cf1z -n cf-monitoring auth can-i delete nodes \
  --as=system:serviceaccount:cf-monitoring:nodemedic-oncall-ui
# expected: no
kubectl --context=cf1z -n cf-monitoring auth can-i patch nodes \
  --as=system:serviceaccount:cf-monitoring:nodemedic-oncall-ui
# expected: yes
kubectl --context=cf1z -n cf-monitoring auth can-i create pods/eviction \
  --as=system:serviceaccount:cf-monitoring:nodemedic-oncall-ui
# expected: yes
kubectl --context=cf1z -n cf-monitoring auth can-i get secrets \
  --as=system:serviceaccount:cf-monitoring:nodemedic-oncall-ui
# expected: no
```

**Pass** (AC-12): all `auth can-i` checks return the expected yes/no.

### Scenario L — Structured action logs (AC-13)

After Scenarios E + G + H:

```sh
kubectl --context=cf1z -n cf-monitoring logs deployment/nodemedic-oncall-ui --since=10m \
  | jq 'select(.event == "ui_action")'
```

**Pass** (AC-13): at least 3 lines (one per UI action), each with `ts`, `action`, `nhd_name`, `node`, `actor: "demo-anonymous"`, `result`, `duration_ms`.

### Scenario M — Buttons gated by node state (AC-14a)

1. Trigger a chaos run, but BEFORE NPD flips and the agent runs, refresh the per-case page rapidly. The phase stays in `Pending` or `Diagnosing` for ~30 s.
2. With the page open during this window: verify the page shows a "Diagnosis in progress" banner.
3. The Uncordon and Clear-skipDeletion buttons remain enabled (the node is already cordoned + skipDeletion-stamped by the controller).

Then:
1. After AC-5 (uncordon), the Uncordon button is disabled with tooltip "Already uncordoned" — even though `phase=Acted` is unchanged.

**Pass** (AC-14a): button state follows live node state, not NHD phase.

### Scenario N — Action-history ring buffer cap at N=20 (AC-14b)

Drive 25 idempotent-violating UI actions against a single NHD (stage by alternately cordoning the node externally with `kubectl cordon` and then uncordoning via the UI):

```sh
for i in $(seq 1 25); do
  kubectl --context=cf1z cordon <node>            # Set unschedulable=true so uncordon writes
  curl -s -X POST localhost:8080/api/cases/<nhd-name>/actions/uncordon | jq
done

kubectl --context=cf1z -n cf-monitoring get nhd <nhd-name> \
  -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | jq 'length'
# expect: 20

kubectl --context=cf1z -n cf-monitoring get nhd <nhd-name> \
  -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | wc -c
# expect: < 8192
```

**Pass** (AC-14b): annotation contains exactly 20 entries (the 5 oldest were dropped FIFO); total payload < 8 KB.

### Scenario O — Action against reclaimed node (AC-14c)

```sh
# After Scenario G, MLC reclaims the node (~30 s). The NHD persists.
# Click Uncordon on the per-case page (or POST manually).
curl -s -X POST localhost:8080/api/cases/<nhd-name>/actions/uncordon | jq
# expect: {"result":"node no longer exists (already reclaimed)"}
```

**Pass** (AC-14c):
- Response is HTTP 200 with `{result: "node no longer exists (already reclaimed)"}`.
- Per-case page renders a "Node was reclaimed by MLC" banner.
- All three action buttons are disabled.
- Audit annotation has a single new entry with `action: "uncordon"`, `result: "reclaimed"`.

### Scenario P — Demo finale walkthrough (AC-14)

The composite gate. End-to-end timed:

1. **t=0** — Engineer is in `#cf-oncall-alerts`. Pre-canned chaos-kubelet-unhealthy job triggered (visible in side terminal).
2. **t=30s** — NPD flips KubeletUnhealthy. Controller fires `POST /diagnose`. Agent loops. NHD `phase=Acted` (`decision=Applied`, `operation=cordon`).
3. **t=~50s** — Block Kit Slack message lands. Header `🚨 Cordoned: cf1z-general-nodes-1000007 (cf1z)`. Engineer reads the fields grid; clicks "View full diagnosis".
4. **t=~52s** — Per-case page opens. Engineer skims rootCause + expands the iptables-REJECT evidence entry to confirm chaos.
5. **t=~58s** — Engineer clicks "Uncordon Node"; confirmation modal; confirm.
6. **t=~60s** — Page refreshes; node uncordoned; action history shows 2 rows. Engineer clicks "Clear MLC skipDeletion"; confirm.
7. **t=~65s** — Action history shows 3 rows. Side terminal: `kubectl get node` shows MLC reclaiming the VM.

**Pass** (AC-14): total wall-clock from t=30s (Slack arrival) to t=65s (action complete) ≤ 60 s. Every screen on the demo path renders without errors. The audit annotation on the NHD reflects the controller's cordon + the engineer's two UI actions.

---

## Cleanup

```sh
helm --kube-context=cf1z uninstall nodemedic-oncall-ui -n cf-monitoring

# Optional: roll the controller back to plain-text Slack while the UI is uninstalled
helm --kube-context=cf1z upgrade nodemedic-controller \
  ./deployment/helm/nodemedic-controller -n cf-monitoring \
  -f deployment/helm/nodemedic-controller/values-azure.yaml \
  --set clusterName=cf1z \
  --set config.useBlockKit=false

# Manually clean up any cordoned nodes still pinned via skipDeletion:
kubectl --context=cf1z get nodes -o jsonpath='{range .items[?(@.spec.unschedulable==true)]}{.metadata.name}{"\n"}{end}'
# For each: kubectl --context=cf1z uncordon <node>
#           kubectl --context=cf1z annotate node <node> machine-lifecycle.newrelic.com/skipDeletion-
```

The UI introduces no Secrets, no PVCs, and no CRDs — `helm uninstall` leaves no orphan state.

---

## Pointers

- Acceptance criteria (the gate for "v1 ships"): [`spec.md`](./spec.md) §8 (AC-1 through AC-14, AC-14a/b/c)
- New annotation schema: [`contracts/ui-action-history.schema.json`](./contracts/ui-action-history.schema.json)
- HTTP API: [`contracts/oncall-ui-api.yaml`](./contracts/oncall-ui-api.yaml)
- Slack Block Kit goldens: [`contracts/slack-block-kit.md`](./contracts/slack-block-kit.md)
- Internal types and field-level rules: [`data-model.md`](./data-model.md)
- Demo flow narrative: [`spec.md`](./spec.md) §9
- Constitution: [`.specify/memory/constitution.md`](../../memory/constitution.md)
- Plan: [`plan.md`](./plan.md)
- Phase 0 research: [`research.md`](./research.md)
- Sister specs:
  - Spec 001 (controller — owner of NHD CRD + Slack post path): [`../001-nodemedic-controller/spec.md`](../001-nodemedic-controller/spec.md)
  - Spec 002 (agent — owner of `status.diagnosis`): [`../002-nodemedic-agent/spec.md`](../002-nodemedic-agent/spec.md)
