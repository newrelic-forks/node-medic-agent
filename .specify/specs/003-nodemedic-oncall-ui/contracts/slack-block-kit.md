# Contract — Slack Block Kit message shape

**Authority**: Spec 003 §6 FR-1, FR-2, FR-3 (Slack format requirements) + spec §0 Q3, Q7, Q8 (clarifications session bindings) + research [`R-2`](../research.md#r-2-block-kit-json-shape-closes-plan-pd-1).

**Direction**: NodeMedic Controller → Slack Incoming Webhook. **Auth**: webhook URL holds the auth (single-secret bearer); no separate Slack token. **Channel**: per-install Helm value `config.slackChannel` (display string only — actual routing is in the webhook URL).

This file freezes the byte-for-byte JSON shapes the controller's `internal/nodemedic/notifier/messages.go` Block Kit builders MUST produce. Golden tests at `internal/nodemedic/notifier/testdata/block-kit/{applied,human-in-loop,failed}.json` lock these against drift; an explicit golden-update commit is required to change them.

---

## Common shape

Every NodeMedic Slack message produced by the new builders has this top-level structure:

```json
{
  "text": "<plain-text fallback line>",
  "blocks": [
    { "type": "header",  "text": { "type": "plain_text", "text": "<emoji> <verb>: <node> (<cluster>)" } },
    { "type": "section", "fields": [ /* up to 4 fields */ ] },
    { "type": "actions", "elements": [
        { "type": "button", "style": "primary",
          "text": { "type": "plain_text", "text": "View full diagnosis" },
          "url": "<config.uiBaseURL>/cases/<nhdName>" }
    ]}
  ],
  "attachments": [
    { "color": "<severity color>", "fallback": "<same as top-level text>" }
  ]
}
```

**Top-level `text`** carries the mrkdwn-stripped fallback for clients that don't render Block Kit (Slack legacy mobile, certain bridges, the unfurl preview generated when a Slack message is quoted in another channel). Format: a single sentence.

**`header.text.text`** is `plain_text` only — Slack rejects mrkdwn inside `header` blocks. Format: `<emoji> <verb>: <node> (<cluster>)`.

**`section.fields`** is the 2-column grid Slack renders for `section` blocks with a `fields` array. Up to 10 fields supported; we use exactly 4. Each field is a `mrkdwn` text element with `*Label:*\nValue` shape so the label renders bold above the value.

**`actions.elements[0]`** is the single primary button. URL pattern: `<config.uiBaseURL>/cases/<nhdName>`. Default `config.uiBaseURL` is `http://localhost:8080` per FR-3.

**`attachments[0].color`** is the severity side bar. Slack's named aliases (`good` / `warning` / `danger`) render correctly on every Slack client; for `Failed` we use the hex `#808080` because Slack lacks a "neutral grey" alias.

---

## 1. `BuildBlockKitApplied` — gate-pass + cordon-applied

**Trigger**: NHD `phase=Acted`, `status.action.decision=Applied`, `status.action.operation=cordon`. The controller fired the auto-cordon.

**Header emoji**: 🚨 (U+1F6A8). **Verb**: `Cordoned`. **Color**: `danger`.

**Golden body** (with placeholder substitutions in angle brackets):

```json
{
  "text": "Cordoned cf1z-general-nodes-1000007 in cf1z: Applied @ 0.92 — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-1000007-1718374200",
  "blocks": [
    {
      "type": "header",
      "text": { "type": "plain_text", "text": "🚨 Cordoned: cf1z-general-nodes-1000007 (cf1z)" }
    },
    {
      "type": "section",
      "fields": [
        { "type": "mrkdwn", "text": "*Decision:*\nApplied" },
        { "type": "mrkdwn", "text": "*Confidence:*\n0.92" },
        { "type": "mrkdwn", "text": "*Trigger:*\nKubeletUnhealthy" },
        { "type": "mrkdwn", "text": "*rcaCategory:*\nKubelet" }
      ]
    },
    {
      "type": "actions",
      "elements": [
        {
          "type": "button",
          "style": "primary",
          "text": { "type": "plain_text", "text": "View full diagnosis" },
          "url": "http://localhost:8080/cases/cf1z-general-nodes-1000007-1718374200"
        }
      ]
    }
  ],
  "attachments": [
    { "color": "danger", "fallback": "Cordoned cf1z-general-nodes-1000007 in cf1z: Applied @ 0.92 — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-1000007-1718374200" }
  ]
}
```

**Field source mapping**:
- `Decision` ← `nhd.status.action.decision` (literal "Applied")
- `Confidence` ← `nhd.status.diagnosis.confidence`, formatted `%.2f`
- `Trigger` ← `nhd.spec.case.trigger.type`
- `rcaCategory` ← `nhd.status.diagnosis.rcaCategory` (literal enum value; "Unknown" if empty)

---

## 2. `BuildBlockKitHumanInLoop` — gate-fail, NOT cordoned

**Trigger**: NHD `phase=Acted`, `status.action.decision=HumanInLoop`. The controller's confidence gate did NOT fire — the diagnosis came back below threshold or the recommendation was `NoAction`. Node is NOT cordoned; on-call must triage.

**Header emoji**: ⚠️ (U+26A0 U+FE0F). **Verb**: `Needs review`. **Color**: `warning`.

**Golden body**:

```json
{
  "text": "Needs review on cf1z-general-nodes-2000007 in cf1z: HumanInLoop @ 0.42 — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-2000007-1718374500",
  "blocks": [
    {
      "type": "header",
      "text": { "type": "plain_text", "text": "⚠️ Needs review: cf1z-general-nodes-2000007 (cf1z)" }
    },
    {
      "type": "section",
      "fields": [
        { "type": "mrkdwn", "text": "*Decision:*\nHumanInLoop" },
        { "type": "mrkdwn", "text": "*Confidence:*\n0.42" },
        { "type": "mrkdwn", "text": "*Trigger:*\nKubeletUnhealthy" },
        { "type": "mrkdwn", "text": "*rcaCategory:*\nKubelet" }
      ]
    },
    {
      "type": "actions",
      "elements": [
        {
          "type": "button",
          "style": "primary",
          "text": { "type": "plain_text", "text": "View full diagnosis" },
          "url": "http://localhost:8080/cases/cf1z-general-nodes-2000007-1718374500"
        }
      ]
    }
  ],
  "attachments": [
    { "color": "warning", "fallback": "Needs review on cf1z-general-nodes-2000007 in cf1z: HumanInLoop @ 0.42 — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-2000007-1718374500" }
  ]
}
```

**Notes**:
- Same field shape as `Applied`. The on-call engineer can read both at a glance with the same scan path; only the header verb + color differ.
- The gate's reason (`internal/nodemedic/controller.GateResult.Reason` — "low-confidence", "single-source", "action=NoAction") does NOT surface in the Block Kit fields. It's available on the per-case page once the engineer clicks through. This is a deliberate trade-off: the Slack message is the trigger to investigate, not the investigation itself.

---

## 3. `BuildBlockKitFailed` — agent failed terminally

**Trigger**: NHD `phase=Failed`. The agent never produced a usable diagnosis (controller-side `ResultTimeout` + retry burned, agent-side `Failed{reason=ToolError|ModelError|...}`, etc.). Node is NOT cordoned.

**Header emoji**: ❌ (U+274C). **Verb**: `Failed`. **Color**: `#808080` (hex grey — Slack has no `neutral` alias).

**Golden body**:

```json
{
  "text": "Failed on cf1z-general-nodes-3000007 in cf1z: AgentUnreachable after 2 attempts — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-3000007-1718375100",
  "blocks": [
    {
      "type": "header",
      "text": { "type": "plain_text", "text": "❌ Failed: cf1z-general-nodes-3000007 (cf1z)" }
    },
    {
      "type": "section",
      "fields": [
        { "type": "mrkdwn", "text": "*Decision:*\n(none — agent failed)" },
        { "type": "mrkdwn", "text": "*Failure:*\nAgentUnreachable" },
        { "type": "mrkdwn", "text": "*Trigger:*\nKubeletUnhealthy" },
        { "type": "mrkdwn", "text": "*Attempts:*\n2" }
      ]
    },
    {
      "type": "actions",
      "elements": [
        {
          "type": "button",
          "style": "primary",
          "text": { "type": "plain_text", "text": "View full diagnosis" },
          "url": "http://localhost:8080/cases/cf1z-general-nodes-3000007-1718375100"
        }
      ]
    }
  ],
  "attachments": [
    { "color": "#808080", "fallback": "Failed on cf1z-general-nodes-3000007 in cf1z: AgentUnreachable after 2 attempts — View full diagnosis: http://localhost:8080/cases/cf1z-general-nodes-3000007-1718375100" }
  ]
}
```

**Field source mapping**:
- `Decision` ← literal "(none — agent failed)" — `status.action.decision` is unset on `Failed`
- `Failure` ← `nhd.status.conditions[type=ReportReady].reason` if present, else "Failed" (controller writes the reason on the failure transition)
- `Trigger` ← `nhd.spec.case.trigger.type`
- `Attempts` ← `BlockKitInput.Failure.Attempts` (carried from controller's retry counter)

The "View full diagnosis" button still points at the per-case page so the engineer can see whatever partial diagnosis or condition history exists.

---

## Severity matrix (FR-2)

| NHD state | Header emoji | Verb | `attachment.color` |
|---|---|---|---|
| `phase=Acted`, `decision=Applied` | 🚨 | Cordoned | `danger` |
| `phase=Acted`, `decision=HumanInLoop` | ⚠️ | Needs review | `warning` |
| `phase=Failed` | ❌ | Failed | `#808080` |
| `phase=Diagnosing` | (no message — controller doesn't post until terminal phase) | — | — |
| `phase=Pending` | (no message) | — | — |

The controller only posts on terminal phases (`Acted` or `Failed`). Diagnosing/Pending posts would be noise; the spec §1 "glance and act" framing requires a decided state to land in Slack.

---

## URL shape

`config.uiBaseURL` is a Helm value with default `http://localhost:8080` (the demo `kubectl port-forward` path, FR-3). The full button URL is `{uiBaseURL}/cases/{nhd.metadata.name}`. Operators flip `uiBaseURL` to a real URL when public ingress lands post-hackathon.

The controller's Slack builder does NOT URL-encode the NHD name — Spec 001's name format (`<nodeNameTruncated50>-<unixTsSeconds>`) uses only `[a-z0-9-]`, which is URL-path-safe. If Spec 001 ever changes the name format to include URL-unsafe characters, the builder MUST add encoding (this would also be a coordinated PR per Constitution Article II.2).

---

## Builder function signatures

```go
package notifier

type BlockKitInput struct {
    NodeName    string
    ClusterName string
    Namespace   string
    NHDName     string
    UIBaseURL   string  // e.g. "http://localhost:8080"
    Diagnosis   *nodemedicv1alpha1.Diagnosis  // may be nil for Failed
    GateReason  string  // populated only for HumanInLoop (currently unused in Slack body — reserved for future)
    Failure     *FailureView  // populated only for Failed
}

type FailureView struct {
    Reason   string  // "DeadlineExceeded" | "AgentUnreachable" | "BadRequest" | "ToolError" | ...
    Detail   string  // currently unused in Slack body — reserved for future
    Attempts int
}

func BuildBlockKitApplied(in BlockKitInput) ([]byte, error)
func BuildBlockKitHumanInLoop(in BlockKitInput) ([]byte, error)
func BuildBlockKitFailed(in BlockKitInput) ([]byte, error)
```

All three return JSON bytes ready to POST to the Slack incoming webhook via the existing `(*Slack).Post(ctx, payload []byte) PostResult` path in `internal/nodemedic/notifier/slack.go`. The post path does not change.

---

## Feature flag for rollback

The controller exposes `--use-block-kit` (default `true` once Spec 003 ships; Helm value `config.useBlockKit`). When `false`, the controller's reconciler falls back to the existing plain-text builders (`BuildApplied`, `BuildHumanInLoop`, `BuildCritical`). This is the demo-day rollback path: if Block Kit rendering breaks on demo morning, one Helm-value flip restores the prior plain-text shape.

Plain-text builders are NOT deleted by Spec 003. They stay alongside the Block Kit builders in `messages.go`.

---

## Cross-references

- Spec: [`spec.md`](../spec.md) §6 FR-1, FR-2, FR-3; §0 Q3, Q7, Q8
- Plan: [`plan.md`](../plan.md) PD-1
- Research: [`research.md`](../research.md) R-2, R-10
- Existing plain-text builders (the fallback path): `internal/nodemedic/notifier/messages.go`
- Existing post path (unchanged): `internal/nodemedic/notifier/slack.go`
- Controller chart Helm values: `deployment/helm/nodemedic-controller/values-azure.yaml`
- Slack Block Kit reference: https://api.slack.com/block-kit
