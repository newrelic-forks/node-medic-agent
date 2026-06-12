# cf1z baseline manifests

Node Problem Detector tuned for cf1z (kubeadm + Cluster API on Azure VMs;
Ubuntu 24.04 / kernel 6.17-azure / containerd 1.7.24 / k8s 1.33).

## Coexistence with the helm-managed NPD

cf1z **already runs** a helm-managed NPD on every node:

- DaemonSet: `node-problem-detector-dev-cf1z` in `cf-monitoring`
- Image: `cf-registry.nr-ops.net/registry.k8s.io/node-problem-detector/node-problem-detector:v0.8.24`
- Loaded configs: image-bundled `kernel-monitor.json` + `docker-monitor.json` + a
  helm `cf-kernel-monitor.json` with three Azure-SR-IOV rules
  (`SYNFlood`, `VFRemoved`, `VFRegistered`)
- Owns NodeConditions: `KernelDeadlock`, `XfsShutdown`, `CperHardwareErrorFatal`

`node-medic-agent` runs **alongside** that DaemonSet. To avoid two NPDs
writing the same NodeCondition (they would fight on every reconcile),
node-medic does **not** load `kernel-monitor.json`. It loads only configs
the helm one doesn't:

| Config | Adds |
|---|---|
| `readonly-monitor.json` (image-bundled) | `ReadonlyFilesystem` NodeCondition |
| `systemd-monitor.json` (image-bundled) | journald-sourced events (kubelet/containerd start, etc.) |
| `system-stats-monitor.json` (image-bundled) | Prometheus host metrics on `:20257` |
| `health-checker-kubelet.json` (override w/ `--enable-repair=false`) | `KubeletUnhealthy` NodeCondition |
| `health-checker-containerd.json` (override w/ `--enable-repair=false`) | `ContainerRuntimeUnhealthy` NodeCondition |

The two health-checker overrides set `--enable-repair=false`. A baseline
must NOT restart kubelet/containerd on a working cluster. We can flip
repair back on for the demo, on a single node, intentionally.

## Why these other choices

| Choice | Why |
|---|---|
| Namespace `cf-monitoring` (existing, helm-managed) | Co-locates with NR license-key secrets and other CF monitoring agents |
| Image from `cf-registry.nr-ops.net` (CF mirror) | Matches the existing NPD; reuses already-pulled layers on every node; no external pull |
| Resources 50m/200Mi req, 200m/400Mi lim | Realistic headroom; the helm chart's defaults are unset |
| Headless Service exposing `:20256` + `:20257` | Pod-DNS targets for Prometheus scrape and `kubectl port-forward` |
| `system-node-critical` priorityClass | Matches the helm NPD; resists eviction |
| Tolerations: `NoSchedule` + `NoExecute` Exists | Schedule on control-plane too |

## Apply

```sh
kubectl apply -f deployment/cf1z/
kubectl -n cf-monitoring rollout status ds/node-medic-agent
```

## Inspect

```sh
# DaemonSet status
kubectl -n cf-monitoring get pods -l app.kubernetes.io/name=node-medic-agent -o wide

# Logs
kubectl -n cf-monitoring logs -l app.kubernetes.io/name=node-medic-agent -f --max-log-requests=10

# NodeConditions WE add (the helm NPD owns Kernel/Xfs/Cper)
kubectl get nodes -o json \
  | jq '.items[] | {name: .metadata.name, conds: [.status.conditions[] | select(.type | test("ReadonlyFilesystem|KubeletUnhealthy|ContainerRuntimeUnhealthy"))]}'

# Live conditions on one pod
POD=$(kubectl -n cf-monitoring get pods -l app.kubernetes.io/name=node-medic-agent -o name | head -1)
kubectl -n cf-monitoring port-forward "$POD" 20256:20256 &
curl -s localhost:20256/conditions | jq

# Prometheus metrics
kubectl -n cf-monitoring port-forward "$POD" 20257:20257 &
curl -s localhost:20257/metrics | grep -E '^problem_(counter|gauge)|^system_'
```

## Remove

```sh
kubectl delete -f deployment/cf1z/
```

This deletes only the node-medic-agent resources — the helm-managed
`node-problem-detector-dev-cf1z` is untouched.
