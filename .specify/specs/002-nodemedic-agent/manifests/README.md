# NodeMedic Agent — Per-Cluster Secret Manifests

Apply these per cluster install. The Helm chart in the plan phase will absorb them as templated values; for the hackathon kickoff, hand-applied YAML is the path.

## Authentication path

The agent reaches Claude via New Relic's internal **nerd-completion gateway**, not the public Anthropic API and not AWS Bedrock. Same pattern as [`nova/k8s-agent-claude-sdk`](https://source.datanerd.us/nova/k8s-agent-claude-sdk). Two env vars do the entire integration:

- `ANTHROPIC_AUTH_TOKEN` — bearer token from Vault, mounted via `Secret/nodemedic-anthropic-token`.
- `ANTHROPIC_BASE_URL` — gateway URL, set on the Deployment env (default `https://nerd-completion.staging-service.nr-ops.net`).

The Claude Agent SDK respects both env vars natively. No code changes vs. the public-API path.

## Prerequisites

- Namespace `container-fabric` exists on the target cluster.
- nerd-completion token retrievable from Vault. Path: `containers/teams/nova/staging/nova-service/NERD_COMPLETION_API_TOKEN` (reusing Nova's `nova-service` path for the hackathon — same path `nova/k8s-agent-claude-sdk` uses). Container Fabric will register its own nerd-completion service path post-hackathon as part of production rollout. Retrieve via: `newrelic-vault us read -field=value containers/teams/nova/staging/nova-service/NERD_COMPLETION_API_TOKEN`.
- For AWS-cluster installs: an IAM role configured for IRSA, with `ec2:DescribeInstanceStatus` / `ec2:DescribeInstances` / `health:DescribeEvents` on the cluster's region. Trust policy bound to the agent's ServiceAccount per [EKS IRSA docs](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html). **No `bedrock:*` permissions needed** — Claude auth doesn't go through Bedrock.
- For Azure-cluster installs: a Service Principal with **Reader** on the cluster's resource group. Tenant/client/secret available out-of-band.
- Hackathon-scoped SSH keypair generated out-of-band; public half installed on target node AMIs / cloud-init.
- New Relic MCP token from the team's NR account `1` (staging).

## What's NOT here

- No PVC manifest. Hackathon scope has no durable audit log; `emptyDir` (configured in the Deployment) is sufficient.
- No ConfigMap for the runbook in this directory. The runbook lives in the agent image at `/app/prompts/runbook.md` and is shipped with the build, not mounted at runtime. (If we want runtime overrides for demo iteration, the plan phase can add a ConfigMap mount.)

## Files

| File | What it provisions | Required on |
|---|---|---|
| `secret-anthropic-token.yaml` | nerd-completion bearer token (`Secret/nodemedic-anthropic-token`) | All clusters |
| `secret-nr-token.yaml` | New Relic MCP auth token (`Secret/nodemedic-nr-token`) | All clusters |
| `secret-ssh-key.yaml` | Hackathon SSH private key (`Secret/nodemedic-ssh-key`) | All clusters |
| `secret-azure-creds.yaml` | Azure SP client-secret tuple (`Secret/nodemedic-azure-creds`) | Azure-cluster installs only |
| `serviceaccount-irsa.yaml` | ServiceAccount with IRSA annotation (`ServiceAccount/nodemedic-agent`) | AWS-cluster installs only |

## Apply order

```bash
# Per-cluster bootstrap (one-time per cluster install)
kubectl --context=<cluster> apply -f secret-anthropic-token.yaml
kubectl --context=<cluster> apply -f secret-nr-token.yaml
kubectl --context=<cluster> apply -f secret-ssh-key.yaml

# AWS-cluster installs additionally:
kubectl --context=<cluster> apply -f serviceaccount-irsa.yaml

# Azure-cluster installs additionally:
kubectl --context=<cluster> apply -f secret-azure-creds.yaml
```

Patch placeholder values (`<...>`) before applying. The placeholders are deliberately invalid so a forgotten edit fails loudly rather than silently mounting an empty Secret.

## Rotation

For the hackathon, rotation is by re-applying these manifests with new values and rolling the agent Deployment (`kubectl rollout restart deployment/nodemedic-agent -n container-fabric`). Production rotation will move to External Secrets / sealed-secrets per the constitution's "Production hardening" list.
