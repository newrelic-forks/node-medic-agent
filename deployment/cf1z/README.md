# cf1z baseline manifests — `hack-node-problem-detector`

Consolidated Node Problem Detector for cf1z (kubeadm + Cluster API on Azure VMs;
Ubuntu 24.04 / kernel 6.17-azure / containerd 1.7.24 / k8s 1.33).

After consolidation (June 2026) this DaemonSet **replaces** the helm-managed
`node-problem-detector-dev-cf1z`. It carries every NodeCondition and Event the
helm DaemonSet emitted, plus the cf1z-specific ones we added on top.

## Conditions and events emitted

| Source config | NodeConditions | Events |
|---|---|---|
| `/config/kernel-monitor.json` (image-bundled) | `KernelDeadlock`, `XfsShutdown`, `CperHardwareErrorFatal` | `OOMKilling`, `KernelOops`, `Ext4Error`, `TaskHung` |
| `/config/readonly-monitor.json` (image-bundled) | `ReadonlyFilesystem` | — |
| `/config/systemd-monitor.json` (image-bundled) | — | journald rule matches (kubelet/containerd start, etc.) |
| `cf-kernel-monitor.json` (in our ConfigMap) | — | `SYNFlood`, `VFRemoved`, `VFRegistered`, `DiskIOError` |
| `check-kubelet.json` (custom plugin) | `KubeletUnhealthy` | — |
| `check-containerd.json` (custom plugin) | `ContainerRuntimeUnhealthy` | — |
| `system-stats-monitor.json` | — | Prometheus metrics on `:20267` |

## Why these choices

| Choice | Why |
|---|---|
| Single DaemonSet (post-consolidation) | One pod per node, one image pull, one set of metrics, no risk of two NPDs fighting on a NodeCondition |
| Named `hack-node-problem-detector` | Distinguishes our hackathon-managed copy from any future return of a helm-managed NPD; resource name matches the workload |
| Namespace `cf-monitoring` (existing) | Co-locates with NR license-key secrets and other CF monitoring agents |
| Image from `cf-registry.nr-ops.net` | Same image the helm chart used; reuses already-pulled layers; no external pull |
| `cf-kernel-monitor.json` absorbed verbatim minus OOMKilling | The helm copy's OOMKilling rule used the legacy `Out of memory: Kill process` pattern that doesn't match modern (≥4.x) kernels. The image-bundled `kernel-monitor.json` carries the canonical rule with the modern `Out of memory: Killed process …` pattern. We let that one own OOM emission to avoid double events. |
| Resources 50m/200Mi req, 200m/400Mi lim | Realistic headroom; the helm chart's defaults were unset |
| `--k8s-exporter-heartbeat-period=5m0s` | Matches the helm NPD's prior cadence; downstream watchers (Spec 001 controller, Spec 002 emitter) see fresh transitionTimes |
| Prometheus on `:20267` (host-network) | Offset from upstream's 20257; the original helm NPD used 20257 on pod-network. Keeping our offset means anything that scraped the legacy port now hits nothing instead of accidentally hitting our service. |
| Headless Service exposing `:20266` + `:20267` | Pod-DNS targets for Prometheus scrape and `kubectl port-forward` |
| `system-node-critical` priorityClass | Matches the helm NPD; resists eviction |
| Tolerations: `NoSchedule` + `NoExecute` Exists | Schedule on control-plane too |

## Cutover (run once on cf1z)

The merge is a 3-step cut. **Order matters** — uninstall helm first to free the
NodeConditions, then apply, then verify the conditions are still being emitted
by our DaemonSet.

```sh
# 1. Remove the helm-managed NPD (releases the Kernel/Xfs/Cper conditions).
helm -n cf-monitoring list | grep node-problem-detector
helm -n cf-monitoring uninstall node-problem-detector-dev-cf1z

# Confirm the helm DaemonSet is gone
kubectl -n cf-monitoring get ds | grep -i node-problem  # should be empty

# 2. Apply the consolidated manifests.
kubectl apply -f deployment/cf1z/
kubectl -n cf-monitoring rollout status ds/hack-node-problem-detector

# 3. Verify all conditions still flip on the apiserver. Within ~5 min the
#    KernelDeadlock/Xfs/Cper conditions should reappear (status=False on a
#    healthy node), now sourced from hack-node-problem-detector.
kubectl get nodes -o json | jq '.items[0].status.conditions[] | select(.type | test("Kernel|Xfs|Cper|Readonly|Kubelet|ContainerRuntime"))'
```

## Inspect

```sh
# DaemonSet status
kubectl -n cf-monitoring get pods -l app.kubernetes.io/name=hack-node-problem-detector -o wide

# Logs
kubectl -n cf-monitoring logs -l app.kubernetes.io/name=hack-node-problem-detector -f --max-log-requests=10

# Live conditions on one pod
POD=$(kubectl -n cf-monitoring get pods -l app.kubernetes.io/name=hack-node-problem-detector -o name | head -1)
kubectl -n cf-monitoring port-forward "$POD" 20266:20266 &
curl -s localhost:20266/conditions | jq

# Prometheus metrics
kubectl -n cf-monitoring port-forward "$POD" 20267:20267 &
curl -s localhost:20267/metrics | grep -E '^problem_(counter|gauge)|^system_'
```

## Remove

```sh
kubectl delete -f deployment/cf1z/
```

After this, **no NPD runs on cf1z**. If you want the helm-managed NPD back,
re-install it from the CF helm repo.
