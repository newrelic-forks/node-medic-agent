# NodeMedic — AI Diagnostic Agent for Sick Kubernetes Nodes

**Container Fabric · AFA 2026 hackathon**
**Status:** pre-meeting draft · **Audience:** team kickoff
**Diagrams** (PlantUML, render with `plantuml *.puml` or paste into Confluence):
- `diagrams/nodemedic-arch.puml` — NodeMedic system architecture (read this first)
- `diagrams/nodemedic-sequence.puml` — End-to-end controller ↔ agent ↔ CR ↔ cordon flow
- `diagrams/npd-block.puml` — NPD per-node block diagram
- `diagrams/npd-sequence.puml` — NPD detection → apiserver propagation
- `diagrams/npd-config-flow.puml` — NPD config files → runtime daemons → effects

---

## 1. The pitch (60 seconds)

We have had **seven DRI tickets** for unhealthy worker nodes. Today: an engineer notices, finds the right context, SSHes in before the cloud reaper kills the box, and walks dmesg / journald / kubelet logs from memory. MTTR is hours. Only CF can do it (we're the only team with SSH).

**NodeMedic** turns that runbook into a 60-second autonomous agent:

1. **Trigger:** node-problem-detector (NPD) flips a `NodeCondition` (or a custom plugin we ship raises one). A controller watching `Node.status.conditions[]` wakes the agent.
2. **Diagnose:** Claude (Agent SDK) calls bounded tools — read-only `kubectl`, whitelisted SSH probes, NRQL on infra + APM telemetry, AWS Health / EC2 status — picking the next probe based on the last result.
3. **Act:** emits a ranked root cause with cited evidence + auto-cordons the node.

**Why this works as a hackathon:** NPD already does the "watch the kernel/syslog/health checks" half. The novel part is the **AI loop** that closes the gap between "node has problem X" and "here's why, here's the fix, here's the cordon." That's a single demo flow.

---

## 2. Why NPD as the trigger source

**Verified facts about NPD** (file:line citations from `/Users/smandyashankar/Downloads/node-problem-detector`):

- It's already a Go DaemonSet — runs as a privileged container, mounts `/dev/kmsg`, `/var/log`, `/etc/localtime` (`deployment/node-problem-detector.yaml:48-62`).
- Four problem-daemon types, all goroutines in one binary:
  - **SystemLogMonitor** — pluggable input (`kmsg` | `journald` | `filelog`), regex rules → `Event` (transient) or `NodeCondition` (permanent). Built-in coverage of `OOMKilling`, `KernelOops`, `Ext4Error`, `KernelDeadlock` (DockerHung), `XfsShutdown`, `CperHardwareErrorFatal` (`config/kernel-monitor.json:25-99`).
  - **SystemStatsMonitor** — emits **only Prometheus metrics, no NodeConditions** (`pkg/systemstatsmonitor/system_stats_monitor.go:111`).
  - **CustomPluginMonitor** — exec a binary/script; **exit 0=OK, 1=NonOK, 2=Unknown**; stdout (truncated to 80B by default) becomes the message (`pkg/custompluginmonitor/types/types.go:27-31`, `pkg/custompluginmonitor/custom_plugin_monitor.go:194,289-297`).
  - **HealthChecker** is *not* a separate daemon — it's a CustomPluginMonitor config that execs the bundled `health-checker` binary against kubelet (`/healthz`), docker (`docker ps`), or containerd (`crictl pods`) (`config/health-checker-kubelet.json:2,18-32`).
- All output reaches the apiserver as **Node.status.conditions[]** (JSON merge patch) and **Event objects** (Source.Component = monitor's `source` field). Condition resync ticks every 10 s, forced heartbeat every 5 min (`pkg/exporters/k8sexporter/condition/manager.go:36-38`, configurable via `--k8s-exporter-heartbeat-period`).
- **No CRDs.** No custom resources. Standard `client-go` patch + `EventRecorder`. Watching is trivial (informer on Node + Event).
- **RBAC is minimal:** the canonical manifest binds the *built-in* `system:node-problem-detector` ClusterRole — patch nodes/status, create events, get nodes. (`deployment/rbac.yaml:7-19`)

**Block diagram:** see `diagrams/npd-block.puml`.

**What NPD does NOT cover today** (relevant gaps for our fault list):

| Fault we care about | Built-in? | Gap → our action |
|---|---|---|
| OOM-kill (kernel) | ✅ `OOMKilling` event | Use as-is |
| Kernel deadlock / docker hung | ✅ `KernelDeadlock` condition | Use as-is |
| Frequent kubelet restart | ✅ `FrequentKubeletRestart` (counter plugin) | Use as-is |
| Kubelet `/healthz` down | ✅ `KubeletUnhealthy` (HealthChecker) | Use as-is |
| **Conntrack saturation** | ❌ | **Custom plugin** (read `/proc/sys/net/netfilter/nf_conntrack_count` vs `_max`) |
| **PID / FD exhaustion** | ❌ | **Custom plugin** |
| **Inode exhaustion** | ❌ (only `disk/percent_used`) | **Custom plugin** (`df -i`) |
| **Disk fill (bytes)** | ⚠️ Ext4Error events only, no Condition | **Custom plugin** (`df` + threshold → permanent Condition) |
| **DNS partition** | ❌ | **Custom plugin** (resolve a sentinel name with timeout) |
| **IMDS throttle** | ❌ | **Custom plugin** (curl IMDS with timeout, count 429s) |

Each custom plugin is a **shell script + JSON entry** — the bar is genuinely that low (`config/custom-plugin-monitor.json` shows the full shape; `config/plugin/check_ntp.sh` is the canonical 8-line example). Plugin cadence is set by `pluginConfig.invoke_interval` (canonical example uses `30s`); for the demo we'll tune to `10s` so detection isn't the bottleneck on the 60 s budget.

---

## 3. NodeMedic architecture

**See:** `diagrams/nodemedic-arch.puml` (block) and `diagrams/npd-sequence.puml` (NPD-side detection sequence).

```
┌──── Target cluster — EKS (AWS) | kubeadm (Azure) ─────────────┐
│                                                               │
│  Worker node N                                                │
│   ┌────────────────────────────────────────┐                  │
│   │ node-problem-detector DaemonSet        │                  │
│   │  · stock plugins (kernel, healthcheck) │                  │
│   │  · CF custom plugins                   │                  │
│   │     conntrack │ fd │ pid │ inode       │                  │
│   │     disk-fill │ dns │ imds             │                  │
│   └─────────────────┬──────────────────────┘                  │
│                     │ NodeCondition flips True                │
│                     │ (and Event written)                     │
└─────────────────────┼─────────────────────────────────────────┘
                      │ kube-apiserver
                      │
                      ▼
            ┌──────────────────────────┐
            │ NodeMedic Controller     │ informer on Node + Event;
            │ (Go, controller-runtime) │ resolves provider/region/
            │ runs in the target       │ instanceId from Node labels
            │ cluster or interlinked   │ + spec.providerID; on a
            │ hub                      │ watched Condition flip,
            │                          │ POSTs case → agent runtime
            └──────────┬───────────────┘
                       │
                       ▼
            ┌────────────────────────────────────┐
            │ NodeMedic Agent runtime (Python)   │ single deployment
            │  Claude Agent SDK                  │ serves both clouds.
            │   system_prompt = runbook (cached) │ Cloud-Info MCP
            │   model: opus 4.7                  │ dispatches to AWS
            │   fallback: sonnet 4.6             │ or Azure backend
            │   mcp_servers:                     │ based on the
            │     · node-ops    (in-proc, SDK)   │ case.provider tag.
            │     · cloud-info  (in-proc, SDK)   │
            │       ├─ aws backend   (boto3)     │
            │       └─ azure backend (mgmt SDK)  │
            │     · nr-mcp      (HTTP)           │
            │   permission_mode = 'default'      │
            │   can_use_tool = allow-list        │
            │   PreToolUse hook = audit log      │
            └──────────┬─────────────────────────┘
                       │ writes status.diagnosis on NHD CR
                       ▼
            ┌──────────────────────────┐    ┌────────────────────────────┐
            │ Cordon executor          │───▶│ kubectl cordon node N      │
            │ (controller-side,        │    │ (in-cluster RBAC)          │
            │ confidence-gated)        │    └────────────────────────────┘
            └──────────────────────────┘
```

### 3.1 Components we actually build

| # | Component | Lang | Effort | Why |
|---|---|---|---|---|
| 1 | **NPD custom plugins** (7 scripts: conntrack, fd, pid, inode, disk-fill, dns, imds) + ConfigMap entry per script | bash + JSON | S | Closes the fault-class gap NPD doesn't cover |
| 2 | **NodeMedic controller** — informer on Node, fires when watched Condition flips True; posts `{node, condition, reason, ts, cluster_meta}` to the agent | Go (controller-runtime) | S–M | One file, one informer; nothing fancy |
| 3 | **Node-Ops MCP server** (SDK in-proc via `create_sdk_mcp_server` + `@tool`) — bounded tools: `kubectl_get`, `kubectl_describe`, `ssh_run` (allow-list of commands), `read_proc_file` | Python | M | Bounded surface = safe + auditable |
| 4 | **NodeMedic agent runner** — Claude Agent SDK driving the loop, `mcp_servers={node-ops: sdk, nr: http, aws-info: sdk}`, `system_prompt=<runbook>`, hooks for audit | Python | M | The brains |
| 5 | **Fault-injection harness** — chaos-mesh manifests for the network/disk faults + node-level scripts for conntrack/IMDS/DNS | YAML + bash | S | No demo without this |
| 6 | **Cordon executor** (separate, *not* a tool the LLM directly calls) — small Python wrapper that the agent's final tool call triggers, behind a confidence gate | Python | XS | Keeps the cordon decision auditable |

S=hours, M=half-day, L=day+. Roughly **4 person-days** for the thin slice + **2** for hardening and the demo deck. Hackathon budget is 2 days × 5 people = 10 person-days, so we have headroom — use it on rehearsal, not extra fault classes.

### 3.2 Why these specific tools (and not others)

- **NR MCP server** is already wired and authenticated for the team — `execute_nrql_query`, `analyze_entity_logs`, `analyze_golden_metrics`, `list_recent_logs`, `list_change_events`, `get_entity` are the high-value calls. **Default account: 1 (staging).**
- **Node-Ops MCP** with the **`@tool` decorator + `create_sdk_mcp_server`** is preferred over a real HTTP MCP server: it runs in the same Python process, so SSH credentials never leave the agent process, and tool definitions live next to their implementation. (Confirmed in installed `claude_agent_sdk` — `McpSdkServerConfig` is a first-class option.)
- **Cloud-Info MCP** stays minimal: `describe_instance_status`, `describe_health_events`, `describe_scheduled_events`. The MCP server's tools take a `provider` arg and dispatch to either an AWS backend (boto3 — `ec2:DescribeInstanceStatus`, `health:DescribeEvents`) or an Azure backend (`azure-mgmt-compute` for VM `instanceView`, Resource Health for availability status, Scheduled Events metadata API). Read-only IAM role / Reader role only.
- **Permission mode = `'default'`** (not `bypassPermissions`). The `can_use_tool` callback enforces an allow-list of SSH command prefixes. PreToolUse hook logs every call for the demo replay.

### 3.3 Diagnostic loop (target: <60 s)

The 60 s budget is wall-clock from "node enters bad state" to "cordon executed." Two parts:

- **NPD detection latency** = `pluginConfig.invoke_interval` (we'll tune to 10 s for demo plugins; for kernel signals via `kmsg`, sub-second).
- **Agent loop** = ~50 s budget. Below is an *illustrative* sequence — the agent picks the actual order.

```
t=0    NodeCondition flips True → controller posts case
t≈1s   Agent boots, system prompt + cluster topology cached
       (cache hit on second+ invocation; first run pays once)
t≈5s   Probe 1: kubectl_get pod -A --field-selector spec.nodeName=N
t≈10s  Probe 2: NRQL — K8sNodeSample / K8sPodSample last 15m
t≈15s  Probe 3: NRQL — Log filtered by host=N, container=kubelet|kube-proxy
t≈20s  Probe 4: ssh_run dmesg --since "15 min ago" (whitelisted)
t≈25s  Probe 5: ssh_run cat /proc/sys/net/netfilter/nf_conntrack_count and _max
t≈35s  Probe 6: aws-info.describe_instance_status (instance impaired? scheduled events?)
t≈45s  Probe 7: NRQL — Transaction errors from APM apps with hostname=N
t≈55s  emit_report(rca, evidence, recommendation)
t≈58s  cordon_executor invoked → kubectl cordon N
```

The agent **picks** which probes to run; the order above is illustrative. The system prompt encodes our runbook (decision tree, evidence-first answers, confidence threshold for cordon).

---

## 4. Demo flow

**Target clusters:** primary demo on `test-odd-wire` (EKS), then a back-to-back rerun on a kubeadm test cluster on Azure (e.g. `test-aks-cf-1`) to prove the same controller + agent works on both providers without code changes. One fault class **demonstrated** on each, two more fault classes **rehearsed back-to-back** as the stretch.

> Why test clusters: blast radius contained, and per CF kubeconfig conventions test clusters use a **single context** (no `-rw` suffix) — so the cordon executor and any kubectl tool the agent invokes share the same context: `kubectl --context=<cluster> ...`. No EUP request needed for write ops. Sync contexts locally before day 1: `eup kube sync test-odd-wire`, plus the Azure kubeadm context per CF runbook.

1. Operator runs `make inject-conntrack CLUSTER=test-odd-wire` (chaos-mesh applies a sysctl + flooders).
2. ~15 s later: our `conntrack-saturation` custom plugin exits 1 → NPD raises `ConntrackSaturated=True`.
3. NodeMedic controller fires the agent.
4. Live log streams the agent's tool calls (PreToolUse hook → stdout).
5. Final report renders in <60 s with cited NRQL query, dmesg lines, and `/proc/sys/net/netfilter/nf_conntrack_count` vs `_max`. Cloud-Info MCP tool call hits the **AWS** backend.
6. `kubectl get node` shows it cordoned.
7. Repeat on Azure: `make inject-conntrack CLUSTER=test-aks-cf-1`. Same controller, same agent, same flow — Cloud-Info MCP now dispatches to the **Azure** backend (VM instance view + Resource Health). End report cites Azure-side evidence.

**Success criteria** (matches the PDF, refined):

- Correct failure class.
- Every claim in the report has a citation (which NRQL row, which log line, which `/proc` value, which AWS/Azure API response).
- Recommended remediation matches what oncall would have chosen.
- Cordon happens automatically, before the failure cascades.
- **Cloud parity:** same conntrack flow runs end-to-end on both EKS and the kubeadm/Azure cluster.
- **Stretch:** same agent prompt + tool set handles `dns_partition`, `imds_throttle`, `disk_fill` without code changes.

---

## 5. Day plan (2-day hackathon)

**Day 1 morning — parallel tracks**
- Track A (1 person): NPD custom plugins + ConfigMap + redeploy NPD on the demo cluster
- Track B (1 person): NodeMedic controller skeleton, watch a known Condition, log to stdout
- Track C (1 person): Node-Ops MCP server with 3 tools + Claude Agent SDK hello-world calling them
- Track D (1 person): chaos-mesh + node-level fault scripts; one fault class repeatable end-to-end

**Day 1 afternoon — first end-to-end slice**
- Agent prompt v1 (system prompt = our runbook) wired up to NR MCP + Node-Ops MCP
- Single fault (conntrack) demo: detection → agent → report → cordon

**Day 2 morning — generalize + harden**
- Add the other fault classes' custom plugins
- Tune system prompt: confidence gate for cordon, evidence-citation requirement
- Add audit hook output to a file the demo deck can show

**Day 2 afternoon — demo polish**
- Three faults back-to-back rehearsal
- Failure-mode rehearsal: agent gets contradictory signals, what does it do?
- Demo deck

**Anti-goal:** do not let any single track block another. If chaos-mesh setup hits a snag, fall back to plain `bash` injection scripts for the demo.

---

## 6. Risks & open questions for the kickoff

1. **SSH credentials on both clouds.** EUP isn't a blocker on test clusters, but the agent still needs an SSH key and route to a worker node on **both** EKS and Azure kubeadm. Confirm: AWS path = bastion / SSM, Azure path = Azure Bastion / hackathon-only key. **Decision needed before day 1.**
2. **Cloud read-only credentials.** AWS: read-only IAM (`ec2:DescribeInstanceStatus`, `health:DescribeEvents`). Azure: Reader role on the test cluster's resource group + Resource Health read; preferably workload-identity-bound to the agent pod. Ask infosec early on both.
3. **NR MCP rate limits.** Agent will burst-query. Default account is staging (`1`); confirm we won't trip a throttle during back-to-back demo runs.
4. **Confidence gating for auto-cordon.** Proposal: agent must cite ≥2 evidence sources from distinct modalities (e.g., one NRQL row + one ssh probe output, or one log line + one cloud-event) and a self-reported confidence ≥ 0.7 before the cordon executor fires. Below threshold → report-only, no cordon. Open for debate.
5. **NPD deployment on both test clusters.** Verify whether each cluster already runs NPD (likely yes on EKS via the addon path or our cluster-spec charts; less certain on the Azure kubeadm test cluster). If yes → we layer a custom-plugin ConfigMap on top (separate `--config.custom-plugin-monitor=` path), avoiding any change to the existing DaemonSet. If no → ship our own DaemonSet pinned to `v0.8.19`. **First action of Track A:** `kubectl --context=<cluster> -n kube-system get ds | grep node-problem` on each.
6. **What goes in the system prompt vs the tools.** Operator runbook in the prompt (cached); cluster topology + provider in a tool / case payload (changes per node). I want a half-hour at the kickoff to align on this.
7. **Out of scope (confirming PDF):** multi-region, predictive scoring, drain (only cordon), production rollout, training corpus from historical incidents. Azure/kubeadm parity is **in scope** for v1 (this is CF — we run both clouds in prod).

---

## 7. What I want from the kickoff meeting

- [ ] Sign off on **NPD as the trigger source** (vs hand-rolled detector or NR alert webhook).
- [ ] Sign off on **Claude Agent SDK** (Python) as the agent runtime — gives us loop + MCP + hooks + permission modes for free.
- [x] Demo clusters: **`test-odd-wire` (EKS)** + one Azure kubeadm test cluster (e.g. `test-aks-cf-1`). → Confirm SSH plan (bastion / SSM / Azure Bastion / hackathon key) on **both** before day 1.
- [ ] Pick the **primary fault class** for day-1 (recommend: conntrack — easiest to inject, most visible).
- [ ] Volunteer ownership for tracks A–D.
- [ ] Agree on the **confidence gate** for auto-cordon.

---

## Appendix A — NPD facts the team should know

- One binary, multiple problem daemons, all goroutines (`pkg/problemdetector/problem_detector.go:48-89` is the full main loop — 40 lines).
- Custom plugins are **trivially extensible**: shell script + JSON entry. No recompile.
- `Status` (the internal type) carries `Events[]` (transient → K8s Events) and `Conditions[]` (permanent → patched into `Node.status.conditions[]`) — `pkg/types/types.go:82-92`.
- Default ports: `:20256` (HTTP, `/conditions` JSON for debug), `:20257` (Prometheus `/metrics`). Both configurable.
- Build tags: `disable_system_log_monitor`, `disable_system_stats_monitor`, `disable_custom_plugin_monitor`, `disable_stackdriver_exporter` — set via `BUILD_TAGS=...` to `make`.
- The DaemonSet is **privileged**, but **not** `hostNetwork` and **not** `hostPID` (`deployment/node-problem-detector.yaml:41-42`).

## Appendix B — Why not just NR alerts as the trigger?

NR alerts on `K8sNodeSample` / `K8sContainerSample` are a fine *secondary* trigger and we can wire one in trivially. But NPD wins as the primary trigger because:

- NPD sees kernel-side signals (kmsg) **before** they show up as a metric drop in NR.
- NPD's HealthChecker hits `kubelet :10248/healthz` directly — faster than NR scraping kubelet's `/metrics`.
- The condition stays `True` on the Node object until something flips it back, so the agent has a stable trigger and we don't have to debounce alert flapping ourselves.

NR alerts stay as the trigger for **app-level** symptoms (APM error rate jump on app A pinned to node N) — that's a separate, future flow.
