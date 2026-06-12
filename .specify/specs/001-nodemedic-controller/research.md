# Phase 0 Research — NodeMedic Controller

**Plan**: [`plan.md`](./plan.md) · **Spec**: [`spec.md`](./spec.md) · **Date**: 2026-06-12

This document closes every NEEDS CLARIFICATION and pins down the dependency / pattern decisions that the plan calls "as researched." Format per skill outline:

> **Decision** — what was chosen.
> **Rationale** — why.
> **Alternatives** — what else was considered, and why rejected.

---

## R-1 Controller framework: controller-runtime via Kubebuilder

**Decision** Use `sigs.k8s.io/controller-runtime` v0.19.x, scaffolded with `kubebuilder init && kubebuilder create api --group nodemedic.cf.newrelic.com --version v1alpha1 --kind NodeHealthDiagnosisAI`.

**Rationale**
- Spec FR-1/FR-5 require informer + reconciler + workqueue + event recorder + manager-hosted metrics endpoint. controller-runtime gives all five out of the box.
- Vendored client-go versioning is solved automatically by kubebuilder (it pins `k8s.io/api`/`apimachinery` to compatible majors).
- It's the de-facto standard in Container Fabric controllers (matches existing CF webhooks/operators), so reviewers will not have to learn a new pattern.
- `envtest` package ships with controller-runtime — Phase 1 quickstart depends on it, and we get it for free.

**Alternatives**
- **`operator-sdk`**. Built on controller-runtime; adds an extra abstraction layer + Helm/Ansible operator scaffolds we don't need. Rejected for hackathon-grade thinness.
- **Hand-rolled `client-go` informers + workqueues.** Possible — NPD itself does this — but we'd write ~500 lines of plumbing (Informer factory, retry'd workqueue, leader election scaffolding, signal handling) that controller-runtime already provides. Rejected on time.
- **`metacontroller`.** Sidecar-style controllers via webhooks; not suitable for a closed-loop reconciler that needs to issue arbitrary patches.

**Citations** controller-runtime README · `kubebuilder.io/quick-start.html`

---

## R-2 CRD ownership and code-generation flow

**Decision** Scope 3 owns the schema (Constitution Article II.2). For Scope 2 we **vendor a frozen copy of the Go types** generated from `nodemedic-scope.md` §2.2 and check `zz_generated.deepcopy.go` into `api/v1alpha1/`. Schema changes by Scope 3 trigger a regen-and-bump PR coordinated across both repos.

**Rationale**
- Avoids both repos depending on a published CRD module we haven't set up yet.
- Keeps the contract test-able: the controller's golden NHD JSON in `contracts/post-diagnose.md` exercises the exact field shape Scope 3 also generates against.
- `controller-gen object` regenerates deepcopy locally; we don't need a separate CI job to publish a types module.

**Alternatives**
- **Publish `nodemedic-types` as a Go module from Scope 3 and import it.** Cleaner long-term; too much yak-shaving for a 2-day hackathon (`go mod proxy` setup, versioning policy, who owns the release tag). Note as post-hackathon follow-up.
- **OpenAPI → Go via `openapi-generator`.** Useful if the contract were external; here it's internal to the org, and we already have Go types from Kubebuilder.

**Citations** `nodemedic-scope.md` §2.2; `controller-gen` docs at `book.kubebuilder.io/reference/controller-gen.html`

---

## R-3 Helm chart hosting (closes spec §10 CL-1)

**Decision** Hackathon: ship as a tarball checked into the controller repo at `deploy/helm/nodemedic-controller-0.1.0.tgz` and document `helm install nodemedic-controller ./deploy/helm/nodemedic-controller -f values-eks.yaml` in the README. Post-hackathon: publish to Artifactory `cf-helm` per the existing CF charts repo workflow (referenced in `~/.go/src/source.datanerd.us/container-fabric/charts/`).

**Rationale**
- Demo deploys from a laptop with kubeconfig set; no need to wire up Artifactory creds during the hackathon.
- A tarball is reviewable in the same PR as the source — captains can read what's about to land on the cluster.
- Existing CF Artifactory `cf-helm` is the right long-term home but requires a publishing workflow we don't have for this repo yet.

**Alternatives**
- **Artifactory immediately.** Forces us to set up artifact promotion + creds; not worth the day.
- **OCI registry.** Same problem as Artifactory plus less familiar to CF.

**Citations** `~/.go/src/source.datanerd.us/container-fabric/charts/` (existing CF Helm chart library); CF CLAUDE.md "Configuration Flow Example"

---

## R-4 CRD installation in the chart (closes spec §10 CL-2)

**Decision** Scope 2's chart installs the CRD by default with `helm.sh/resource-policy: keep`. A `--set installCRD=false` toggle exists for environments where Scope 3's chart (or a manual `kubectl apply -f config/crd/bases/`) installed it first. README documents the handshake: "if both charts are installed, set `installCRD=false` on whichever runs second; either is acceptable."

**Rationale**
- Hackathon-grade: each captain can install their chart standalone on a fresh test cluster and have a working CRD.
- `keep` annotation prevents `helm uninstall` from yanking the CRD and orphaning live NHD objects.
- The toggle is one flag, not a separate sub-chart — simpler.

**Alternatives**
- **CRDs only in Scope 3's chart.** Requires Scope 3 to install first; brittle when test clusters are wiped.
- **Separate `nodemedic-crd` chart.** Three Helm charts to coordinate is too many for this hackathon.

**Citations** Helm CRD installation docs at `helm.sh/docs/chart_best_practices/custom_resource_definitions/` — confirms `crds/` directory installs once; we use a templated CRD instead so the toggle works.

---

## R-5 Idempotent `POST /diagnose` (closes spec §10 CL-3)

**Decision** Treat duplicate `caseId` as idempotent on Scope 3's side: a repeat POST returns `202 { "caseId": "...", "status": "queued" }` if the case is in-flight, or `202 { "caseId": "...", "status": "completed" }` if already written to the CR. The controller MUST tolerate either response.

**Rationale**
- This is what the FR-7 retry path needs: re-issuing the same `caseId` after a `DeadlineExceeded` must not create a second concurrent agent run.
- Aligns with `nodemedic-scope.md` §6.6 which flags this as Scope 3's open question — we commit to the contract on the controller side and ask Scope 3 to confirm by Day 1 PM.
- If Scope 3 returns 409 instead, the controller treats that identically to 202 (no re-retry, NHD stays in `Diagnosing`, deadline-driven).

**Alternatives**
- **New `caseId` on retry.** Pollutes audit log with two cases per failure; harder to reason about. Rejected.
- **Controller side-track to a "retry CR".** More schema, more state. Rejected.

**Citations** `nodemedic-scope.md` §3.1, §6.6; spec FR-4, FR-7

---

## R-6 Slack "View CR" link target (closes spec §10 CL-4)

**Decision** Skip a button entirely. The Slack message body for `Applied` and `HumanInLoop` includes a fenced markdown block with the exact `kubectl get` command:

```
kubectl --context=test-odd-wire -n cf-monitoring get nhd <name> -o yaml
```

The controller substitutes `<name>` and the cluster context name from `--cluster-name`. No external URL.

**Rationale**
- Test clusters don't have a stable public dashboard URL.
- A copy-pasteable command is *more* useful for the demo audience than a 404'd link.
- Fewer moving parts to break right before the demo.

**Alternatives**
- **Static page hosting the CR YAML.** Adds a deploy target; not worth it.
- **Argo CD link.** Argo CD doesn't render NHD CRs natively and isn't installed on the test clusters anyway.

**Citations** spec §10 Q4 (recommends option (a)); spec FR-9

---

## R-7 Multi-replica leader election (closes spec §10 CL-5)

**Decision** Single replica, no leader election in v1. Document the restart-window behavior already covered by FR-11 and accept it. controller-runtime's leader election can be enabled with `manager.Options{LeaderElection: true}` post-hackathon if we promote this past test clusters.

**Rationale**
- A pod restart during reconcile is the only failure mode and FR-11 makes it idempotent.
- Leader election needs a `coordination.k8s.io/leases` RBAC entry; we'd be widening RBAC for a v1 we don't need.
- Hackathon NFR-2 explicitly tags this as stretch.

**Alternatives**
- **Leader election from day 1.** Strictly safer but burns time we'd rather spend on cordon/Slack/Azure parity.

**Citations** spec NFR-2; controller-runtime `manager.Options.LeaderElection`

---

## R-8 NPD-signal data path (verifies FR-2 Node-side resolution)

**Decision** Read `provider`, `region`, `instanceId` off the Node object exclusively, per spec §7.3 and FR-2's resolution table. Do not parse Events, do not parse the Condition's `Message`, do not call cloud APIs.

**Rationale**
- Verified directly against the NPD source on this branch:
  - `pkg/types/types.go:55-68` — internal Condition struct has only `Type/Status/Transition/Reason/Message`. No cloud fields.
  - `pkg/util/convert.go:29-37` — apiserver-side `v1.NodeCondition` is `{Type, Status, LastTransitionTime, LastHeartbeatTime, Reason, Message}`. No cloud fields.
  - `pkg/exporters/k8sexporter/problemclient/problem_client.go:138-145` — patch body is `{"status":{"conditions":[...]}}` only. NPD never patches `metadata.labels` or `spec.providerID`.
  - `pkg/exporters/k8sexporter/problemclient/problem_client.go:122-160` — Events have only `(Component, Host=nodeName)` source and a Node ref. No cloud metadata.
  - `pkg/exporters/stackdriver/` — opt-in, GCE-only; not used by us.
- Conclusion in spec §7.3 stands. No further research needed.

**Rationale (resolution rules)**
- `provider`: prefer `cf.newrelic.com/cloud-provider` label (set by argo-webhook per CF CLAUDE.md "Configuration Flow Example"). Fall back to parsing `Node.spec.providerID` URI scheme.
- `region`: GA label `topology.kubernetes.io/region` (set by both EKS cloud-controller-manager and the Azure cloud-controller-manager on kubeadm clusters).
- `instanceId` AWS: last segment of `aws:///<az>/<instance-id>`. Confirmed by `Node.spec.providerID` shape on EKS managed nodes.
- `instanceId` Azure: VM name from `azure:///subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/virtualMachines/<vm>`. Confirmed by `cloud-provider-azure` source (`pkg/provider/azure_managedDiskController.go` URI builders).

**Alternatives**
- **Parse from Event message text.** Brittle and out-of-spec.
- **Call cloud APIs from the controller to enrich.** Violates Constitution Article II.7's spirit (cloud knowledge stays below the FR-2 line) and adds a network dependency at trigger time.

**Citations** spec §7.3 (NPD source pinpoints); CF CLAUDE.md "Configuration Flow Example" (argo-webhook injects `cloudProvider`); cloud-provider-aws Node Authoritative Manager docs; cloud-provider-azure providerID format

---

## R-9 Debounce strategy

**Decision** In-memory debounce keyed by `(nodeName, conditionType)` with a 30 s sliding window, stored in the controller process. Implementation: `sync.Map[debounceKey]time.Time` updated on every "saw transition to True"; on each event, drop if `now - lastSeen < 30s`. No persistent dedup.

**Rationale**
- Hackathon-grade; spec FR-1 + FR-3 already say `Create` is idempotent at the apiserver level (deterministic NHD name from `<node>-<unix-ts>` rounded to a window), so a missed debounce only causes a no-op `Create`.
- Avoids any CR-or-CM-backed state.

**Alternatives**
- **Use the NHD existence check as the debounce.** Possible — name-by-window means a duplicate trigger maps to the same NHD name. We do this anyway in FR-3, but the in-memory window saves a wasted apiserver round-trip per duplicate trigger.
- **ConfigMap-backed dedup.** Overkill for a single-replica controller.

**Citations** spec FR-1, FR-3

---

## R-10 Confidence gate as a pure function

**Decision** Implement `func confidenceGate(d v1alpha1.Diagnosis, minConfidence float64, minSources int) (Decision, string)` returning `(Pass | Fail, humanReason)`. Pure, no I/O, no clock. Tested with table-driven tests covering each clause individually.

**Rationale**
- Constitution Article I.3 calls this rule binding — the controller has to be auditable in its decision. A pure function with explicit inputs is the easiest thing to defend.
- Threshold values come from flags (NFR-6) but the *structure* (three clauses, AND'd) is hard-coded. Loosening the structure requires a code change, not a flag flip — making the gate visible in code review.

**Alternatives**
- **OPA / Rego.** Way too much machinery for one if-statement.
- **Configurable expression language.** Same — and dangerous because it lets the gate be weakened at runtime.

**Citations** spec FR-6; Constitution Article I.3

---

## R-11 Slack notifier transport

**Decision** stdlib `net/http.Client` with a 5 s per-attempt timeout, 3 attempts at 1 s / 2 s / 4 s backoff. Block Kit JSON hand-built (no `slack-go/slack` dependency). Webhook URL from env var sourced from `Secret/nodemedic-slack`.

**Rationale**
- One outbound HTTP call to a documented JSON contract — a third-party SDK is over-engineered.
- Avoids dependency drift.
- Easier to golden-file test (we control the JSON shape).

**Alternatives**
- **`github.com/slack-go/slack`.** Adds dep + larger surface than we use.
- **Webhook → bot user.** Bot tokens carry workspace-scoped powers; webhook is read-only-write-to-channel. Use the lower-privilege option.

**Citations** spec FR-9, NFR-3; Slack Block Kit reference at `api.slack.com/block-kit`

---

## R-12 Metrics surface

**Decision** Register Prometheus collectors against the controller-runtime manager's built-in registry (`metricsserver.Options{BindAddress: ":9443"}`). Emit the 5 metrics in spec NFR-3 plus controller-runtime's default `controller_runtime_*` metrics for free.

**Rationale**
- Manager's metrics server already serves `/metrics` on the configured port.
- Single port reduces Helm chart surface (one Service, one port).

**Alternatives**
- **Separate `prometheus.DefaultRegisterer` on a different port.** Two listeners, two Service ports. No upside.

**Citations** spec NFR-3; controller-runtime `pkg/metrics/server`

---

## R-13 envtest as the integration boundary

**Decision** Use `sigs.k8s.io/controller-runtime/pkg/envtest` with `ENVTEST_K8S_VERSION=1.31.x` for the reconciler tests. Tests start a real `kube-apiserver` + `etcd` from `setup-envtest` and let the reconciler issue real Patches. The agent and Slack are mocked at the HTTP transport layer (`http.RoundTripper`).

**Rationale**
- Real apiserver gives us real status-subresource semantics, real watch behavior, real OptimisticConcurrencyError handling — none of which can be faked accurately.
- Mocking the agent client and Slack at the `RoundTripper` level keeps the controller's HTTP code path identical to production.
- envtest is the standard Kubebuilder pattern; reviewers will recognize it.

**Alternatives**
- **Fake client (`sigs.k8s.io/controller-runtime/pkg/client/fake`).** Faster but doesn't model status-subresource auth checks or real watch ordering. Use only for the gate function (which is pure anyway).
- **Full kind/k3d cluster.** Slower (~30 s setup); worth it only for E2E, which we'll run by hand.

**Citations** controller-runtime envtest docs; `setup-envtest` from kubebuilder

---

## R-14 Day-1-AM stub deliverable (Constitution II.3 / III.2)

**Decision** First milestone is a "stub controller" that:
1. Watches `Node` Conditions, debounces, creates NHD CRs with populated `spec.case`.
2. Skips the agent call entirely. Instead, hand-canned `status.diagnosis` is written into a fixture NHD by the test harness or by a kubectl one-liner.
3. Runs the gate, cordons (or routes to HumanInLoop), and posts to a stub Slack endpoint.

This is what we ship by Day 1 12:00 so Scope 1 (fault injection) and Scope 3 (agent) can integrate against a working controller.

**Rationale**
- Constitution Article II.3 is non-negotiable: each scope ships a stub.
- Lets Scope 3 develop its agent against a CR shape that's already settled.
- Lets Scope 1 verify the demo flow front-to-back without the agent finished.

**Alternatives**
- **Wait until everything is built then integrate.** Constitution III.2 explicitly forbids this pattern.

**Citations** Constitution Article II.3, III.2; `nodemedic-scope.md` §7

---

## Open items not yet resolved (non-blocking)

None. All NEEDS CLARIFICATION items from the plan's Technical Context are resolved above (CL-1→R-3, CL-2→R-4, CL-3→R-5, CL-4→R-6, CL-5→R-7).

Phase 1 may proceed.
