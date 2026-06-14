# Spec 003 — NodeMedic On-Call UI + Slack Format Upgrade

**Status:** Draft · 2026-06-14
**Scope:** Demo finale for AFA 2026 hackathon (Captain: Sachin)
**Constitution:** [`.specify/memory/constitution.md`](../../memory/constitution.md)
**Sister specs:** [`../001-nodemedic-controller/spec.md`](../001-nodemedic-controller/spec.md), [`../002-nodemedic-agent/spec.md`](../002-nodemedic-agent/spec.md)

This spec defines the **on-call experience** layer that sits on top of Specs 001 and 002. It does not add to the diagnosis pipeline; it makes the existing pipeline's output human-consumable and acts on it. It binds no new cross-scope contract — the UI reads the existing `NodeHealthDiagnosisAI` (NHD) CRD and uses the kube API for any mutation. Implementation choices that don't change the user-visible workflow are out of scope here and belong in the plan.

> **Hackathon scope (deliberate trade-offs):**
> - Anonymous UI behind `kubectl port-forward`. No auth, no SSO, no public ingress, no TLS. The port-forward is the access boundary.
> - Ephemeral state. UI reads NHDs from the kube API on every request. No SQL, no NoSQL, no cache. Action audit trail lives as JSON-encoded annotations on the NHD CR.
> - One cluster's view. The UI shows NHDs for the cluster it's deployed on; it does not federate.
> - Vanilla HTML/CSS/JS. No framework, no build step, no node_modules.
> - Slack message formatting only — no Slack interaction handlers, no action buttons in Slack itself. All actions live in the UI. The Slack message just deep-links there.
>
> All of this is intentional. The UI is the demo's closing punctuation, not a production feature. Production-grade observability, multi-cluster federation, durable audit, and SSO are on the post-hackathon hardening list.

---

## Clarifications

### Session 2026-06-14

- Q: Where does the UI source its data from — the existing NHD CRD, a new audit store, or both? → A: **NHD CRD only.** Spec 001's CRD already carries every field the UI needs (`spec.case.*` for trigger metadata, `status.diagnosis.*` for agent output, `status.action.*` for the cordon decision, `status.conditions[*]` for state transitions). Adding a separate store would duplicate the schema, complicate write paths, and gain nothing for a demo. The UI lists/gets NHDs via the kube API and renders directly. Trade-off: UI is only as available as cf1z's apiserver — fine for a demo running on cf1z itself.
- Q: Where do the action buttons (uncordon / drain / clear skipDeletion) execute — through the controller or directly via the kube API? → A: **Directly via the kube API.** UI backend has its own ServiceAccount with the four verbs it needs (`nodes get/list/watch/patch`, `pods/eviction create`, NHD `get/list/watch/patch`). No new HTTP contract between the UI and controller; no broadening of the controller's mutation surface. The action's audit trail lives as a JSON-encoded annotation on the NHD CR (`nodemedic.cf.newrelic.com/ui-action-history`) so it's observable via plain `kubectl get nhd -o yaml` without a separate audit log.
- Q: Slack actions — buttons that POST back to a NodeMedic endpoint, or just a link to the UI? → A: **Link only.** A button in Slack labeled "View full diagnosis" that opens the UI's per-case page. No interactive Slack components, no Socket Mode, no public webhook. The actions (uncordon / drain) are in the UI, not in Slack. Keeps Slack changes pure-formatting.
- Q: How does the on-call engineer reach the UI during the demo? → A: **`kubectl port-forward` to localhost.** UI runs as a Deployment + ClusterIP Service in `cf-monitoring`. The engineer (or a pre-canned demo script) runs `kubectl --context=cf1z -n cf-monitoring port-forward svc/nodemedic-oncall-ui 8080:8080` before the demo. The Slack 'View full diagnosis' button URL is `http://localhost:8080/cases/<nhd-name>`. Public ingress + DNS + TLS deferred to post-hackathon hardening.
- Q: Does the "Drain Node" button violate Constitution Article I.2 (cordon-only)? → A: **No, but it expands the mutation surface and must be called out.** Article I.2's prohibition is on the *agent* — "the cordon executor is not an LLM tool." The UI is a *human-initiated* action surface. A human clicking "Drain" is the same kind of operation as a human running `kubectl drain` from their laptop, except gated by a confirmation modal that lists the pods that will be evicted. Auto-drain stays explicitly off; no agent or controller code path reaches eviction. The plan must record this as a deliberate scope expansion with the safety gate (confirmation modal + actor logging) named explicitly.
- Q: What value does the `actor` field carry on UI-initiated action log lines, given the UI is anonymous? → A: **`demo-anonymous`** for the hackathon. The constitution requires every action be observable (Article I.4); it does not require that actions be attributable to a named user. Attribution lands when SSO does, on the production hardening track. The structured stdout log line still carries `ts`, `action`, `nhd_name`, `node`, and `result` so the action's *occurrence* and *effect* are auditable; only the *who* is degraded.
- Q: What channel does the Slack notification land in? → A: **Configurable per install via Helm value `config.slackChannel`.** Today's NodeMedic posts go to `#nodemedic-demo` in DN-Staging; the user is creating a fancier-named channel out of band (e.g. `#cf-oncall-alerts`) and will flip the value. The channel name does NOT live in the spec; only the contract that the value is per-install configurable does.
- Q: What color attachment per severity should the Block Kit message carry? → A: **Slack-conventional named colors.** `danger` for `decision=Applied` (red bar — automated cordon fired), `warning` for `decision=HumanInLoop` (yellow bar — gate failed, on-call to triage), `#808080` for `phase=Failed` (grey — system error, not a node-severity signal), no `attachment.color` for `Diagnosing`/`Pending` (no severity yet — clean default styling). Slack's named aliases render correctly on every Slack client without per-theme hex tuning.
- Q: When are the Uncordon / Drain / Clear-skipDeletion action buttons enabled on the per-case view? → A: **Gated by node state, not NHD phase.** Uncordon is enabled iff `node.spec.unschedulable=true`; Clear-skipDeletion iff the node carries the `machine-lifecycle.newrelic.com/skipDeletion` annotation; Drain iff the node exists. NHD phase (`Diagnosing`, `Pending`, `Acted`, `HumanInLoop`, `Failed`) does NOT gate button enablement — the engineer's intent is to act on the node, not the case, and tying button state to phase would block legitimate workflows like "the agent is taking forever, just uncordon it" or "this is clearly a chaos test, no need to wait." When `phase != Acted`, the per-case page MUST show a status banner naming the current phase so the engineer knows the diagnosis isn't final, but the buttons themselves stay actionable.
- Q: What's the size policy for the `ui-action-history` annotation, given etcd's ~256 KB per-object ceiling and that drain actions can produce many entries? → A: **Last-N FIFO ring buffer, N=20.** When appending the 21st entry, the oldest entry (entry[0]) is dropped. Each entry is ~150–250 bytes, so 20 entries totals ~5 KB — comfortably below any apiserver limit. Older audit history lives in (a) the UI pod's structured stdout logs (FR-18), durable for the pod's lifetime + log-rotation window, and (b) the controller's per-action log lines from Spec 001. The annotation is the live tail; stdout is the archive. The UI renders whatever is in the annotation as-is.
- Q: How does drain stream per-pod progress from the backend to the browser? → A: **Server-Sent Events (SSE) over POST.** The drain endpoint responds with `Content-Type: text/event-stream`; each per-pod result is emitted as a single `data: {...}\n\n` event (JSON payload `{pod, namespace, result: "evicted"|"skipped"|"error", detail}`); a final `event: complete\ndata: {summary}\n\n` closes the stream. Backend uses Go's `http.Flusher.Flush()` after each event. The action verbs are POSTs (the action mutates state), so the browser cannot use the native `EventSource` API — `EventSource` is GET-only by spec. The browser uses `fetch(url, {method: "POST"})` and reads `response.body.getReader()` (a `ReadableStream`), then parses SSE frames in JS. SSE is still one-way (server → client) which matches the drain shape; the only thing we lose vs `EventSource` is auto-reconnect, which is acceptable because the backend completes the eviction loop and writes the audit annotation regardless of whether the consumer is still attached (spec §5 edge case binds this). About 30 lines of Go for the backend; ~40 lines of JS for the SSE-frame parser.
- Q: What happens when an action is taken against a node that no longer exists (e.g. MLC reclaimed it before the engineer clicked Uncordon)? → A: **Treat as a benign "already reclaimed" state, not an error.** Action endpoints return HTTP 200 with `{result: "node no longer exists (already reclaimed)"}` when the apiserver's GET on the node returns NotFound. A single audit entry is appended capturing the action attempted and the not-found outcome. The per-case page refreshes with a banner "Node was reclaimed by MLC" and disables all three action buttons. Same shape as the "already uncordoned (no change)" idempotent path — the engineer's intent is mooted because the node is gone, which is the desired end state, not a failure.

---

## 1. Problem statement

Today, when NodeMedic cordons a node, the on-call engineer sees a Slack message that looks like this:

```
NodeMedic: cf1z-general-nodes-2000007 — Kubelet (conf 0.92)
Cluster: cf1z                                Action: cordoned
RCA: NPD's kubelet-monitor flipped KubeletUnhealthy/KubeletHealthzFailed because the
healthz probe to 127.0.0.1:10248 is being blackholed by iptables REJECT rules ...
[2000+ characters of dense prose continues]
Inspect: kubectl --context=cf1z -n cf-monitoring get nhd ...
```

To act, the engineer either reads the wall of prose to find the recommendation, or copies the `kubectl get nhd` command, runs it, and pastes the YAML into a JSON parser. There is no "view this case" surface, no "list all recent cases" surface, and no "do something about it" surface. Every action — uncordon, drain, clear MLC pin — is raw `kubectl` from the engineer's laptop.

For the demo finale we want the engineer to land on a polished surface that **shows** the diagnosis at a glance and **acts** on the node with a single button. The Slack message is the entry point; the UI is the destination; the kube API is the actuator.

---

## 2. Goals (in scope for v1)

| # | Goal | Why |
|---|---|---|
| G1 | Reformat the Slack post on every NHD `phase=Acted` (or `HumanInLoop`) transition into a Slack Block Kit message with a header (severity emoji + node + cluster), a fields grid (decision, confidence, rcaCategory, observed-at), and a primary "View full diagnosis" link button that deep-links to the per-case UI page. | Glance-and-go alert. The on-call eye sees the headline in the header and clicks one button to investigate. |
| G2 | Stand up a NodeMedic On-Call UI as a separate Deployment + Service in the same namespace as the controller and agent (`cf-monitoring` on cf1z). The UI has two pages: a list view (`/`) and a per-case detail view (`/cases/<nhd-name>`). | A human-readable lens on top of the NHD CRD. List view = "what's going on right now"; detail view = "what does this specific case mean and what do I do." |
| G3 | List view shows NHDs from the last 24h, sorted newest first, with columns for created-at, node, cluster, trigger condition, phase, decision, confidence, and a button linking to the per-case page. Auto-refreshes every 30 s. Filters by cluster (single-cluster install today; the filter is forward-looking). | The on-call engineer arrives mid-incident and needs to see what's already in flight. |
| G4 | Per-case view shows: case metadata (case_id, node, cluster, trigger.{type,reason,observedAt}), agent diagnosis (rootCause, recommendation.{action,reason}, rcaCategory, confidence, modelUsed, completedAt), evidence list (each with source, ref, result preview, observedAt), action history (status.action.{decision,operation,appliedAt} + UI-initiated actions from the audit annotation), and three action buttons. | The engineer's full mental model of the case in one screen. |
| G5 | Three action buttons on the per-case view: **Uncordon Node**, **Drain Node**, **Clear MLC skipDeletion**. Each opens a confirmation modal that names the exact action and its effect (and for Drain, lists the pods that will be evicted). On confirm the UI executes via the kube API, displays kubectl-style output (success / partial / error), and refreshes the page. | A human-initiated action surface that's auditable, scoped, and demo-friendly. |
| G6 | Every UI button click writes a structured stdout log line with `{ts, action, nhd_name, node, actor: "demo-anonymous", result}` AND appends an entry to the NHD's `nodemedic.cf.newrelic.com/ui-action-history` annotation (JSON-encoded array) so the audit trail survives the UI pod's lifetime. | Constitution Article I.4 — every action observable. The annotation is the durable surface; the stdout log is the live tail. |
| G7 | Run on any cluster the controller + agent already run on — `cf1z`, `jc1z`, `sk1z`, `test-*`. Helm chart's cluster-name guard rejects anything else. Same image-build pattern, same registry, same `cf-monitoring` namespace. | Constitution Article I.5. Reuses the existing chart pattern from Specs 001 + 002. |
| G8 | UI ServiceAccount carries the **minimum** RBAC verbs needed: `nodes get/list/watch/patch`, `pods/eviction create` (drain), `pods get/list/watch` (drain pre-flight + display), `nodemedic.cf.newrelic.com/nodehealthdiagnosisais get/list/watch/patch` (read CR + write audit annotation). No `delete`, no `create` on nodes, no broader resource access. | Constitution Article I.1 — credential-layer least privilege. |
| G9 | Cordon/uncordon and skipDeletion-clear actions are **idempotent**. Clicking "Uncordon" on an already-uncordoned node returns a clear "no change needed" result; the UI does not error. Drain is **not idempotent** — clicking it during an in-flight drain shows a "drain in progress" indicator and disables the button. | The on-call engineer's first instinct is to click the button. The UI must absorb double-click and "is this in flight?" gracefully. |
| G10 | Slack channel for posts is per-install configurable via Helm value (`config.slackChannel`). The webhook URL is mounted from a Secret as before; only the channel-display string changes. | Decouples this spec from any specific channel name; the user is renaming/recreating the channel out of band. |

---

## 3. Non-goals (explicitly out of v1)

Per the constitution's hackathon-scope deviations and the clarifications above:

- **No authentication, no SSO, no per-user attribution.** UI is anonymous behind `kubectl port-forward`. Action log lines record `actor: "demo-anonymous"`. SSO lands when the production hardening track lands.
- **No public ingress, no DNS, no TLS.** Slack 'View full diagnosis' link is `http://localhost:8080/...` and assumes the engineer has the port-forward up.
- **No multi-cluster federation.** UI shows the NHDs on the cluster it's deployed on. Cross-cluster federation is a post-hackathon concern.
- **No durable audit store beyond the NHD annotation.** No SQLite, no Postgres, no SQS. The annotation is the audit; `kubectl get nhd -o yaml` is the query.
- **No action retries, no async job tracking.** Each action button click is a synchronous request → response. Failures fail loudly with the apiserver's error text shown in the UI.
- **No Slack interaction handlers.** Buttons in the Slack message are link buttons only. No POSTs back to NodeMedic from Slack. No Socket Mode.
- **No mobile / responsive UI.** Desktop-only. Demo viewport is "engineer's laptop on the demo screen."
- **No theming, no dark mode, no logo.** A single clean stylesheet. The hackathon doesn't need a brand.
- **No new CRD, no CRD changes.** Reuses the existing `NodeHealthDiagnosisAI` schema. Article II.1 stays a clean two-contract surface.
- **No Prometheus `/metrics` endpoint on the UI.** Stdout structured logs are enough for the demo. Production observability lands later.
- **No integration with the eval agent (Scope 4).** The UI does not read or write `status.evaluation.*`.
- **No runbook integration.** Knowledge / decision-tree content stays in the agent's runbook; the UI does not surface it. (The engineer follows links in the diagnosis text; we don't render runbook excerpts.)
- **No webhook fan-out, no PagerDuty, no email.** Slack is the only notification surface.

If something here moves into scope mid-hackathon, it requires a constitution amendment.

---

## 4. User personas

- **On-call engineer (primary).** Receives a Slack page when NodeMedic cordons a node. Wants to see at a glance whether it's a real fault or a chaos test, what the diagnosis is, and what action to take. Time budget for a single page: < 60 s from Slack ping to action taken.
- **Investigator (secondary).** Joins the channel an hour after the cordon to look at past cases. Needs a list view to find the case, then the per-case view to read the diagnosis without paging back through Slack history.
- **Demo audience (tertiary).** Watches the demo finale. Sees the polished message in Slack, the engineer click through to the UI, and the action take effect on cf1z. The narrative is the audience experience; the UI is the cinematography.

---

## 5. User stories

### US1 — Slack alert with structured glance and click-through (Priority: P1)

**As an** on-call engineer in the alerts channel
**I want** the NodeMedic Slack post to show me node, decision, confidence, and rcaCategory at a glance, with a single button to go deeper
**So that** I can triage the alert in two seconds and decide whether to investigate now or later

**Why this priority:** Without P1 the rest of the demo is unreachable from Slack. The Slack message is the only entry point.

**Independent test:** Hand-craft an NHD CR with `phase=Acted`, `decision=Applied`, `operation=cordon`, confidence=0.92, rcaCategory=Kubelet, and a small canned diagnosis. The controller posts to Slack. The message renders as Block Kit with a working "View full diagnosis" button URL.

**Acceptance scenarios:**
1. **Given** the controller has just stamped `phase=Acted` with `decision=Applied`, **When** the Slack notifier fires, **Then** the channel receives a Block Kit message with: a header `🚨 Cordoned: <node> (<cluster>)`, a fields section showing `Decision`, `Confidence`, `Trigger`, `rcaCategory`, and a primary action button labeled "View full diagnosis" pointing at `<UI base URL>/cases/<nhd-name>`.
2. **Given** the controller stamps `decision=HumanInLoop` (low confidence, gate failed), **When** the Slack notifier fires, **Then** the message header reads `⚠️ Needs review: <node> (<cluster>)` and the action button still points at the per-case page.
3. **Given** the operator clicks the "View full diagnosis" button **and** the `kubectl port-forward` is up, **When** the browser opens, **Then** the per-case page for that NHD renders.

---

### US2 — On-call UI: list and detail browse (Priority: P1)

**As an** on-call engineer mid-investigation
**I want** to see all NHDs from the last 24h and drill into any one of them
**So that** I have context beyond the single Slack alert in front of me

**Why this priority:** P1 because without this, the Slack click-through has nowhere to go. Together with US1, this is the demo's primary path.

**Independent test:** Run `kubectl port-forward svc/nodemedic-oncall-ui 8080:8080` against a cluster with at least three NHDs in the last 24h. Open `localhost:8080/`. The list table renders with all three rows, sorted newest first. Clicking any row navigates to `/cases/<nhd-name>` and renders the diagnosis.

**Acceptance scenarios:**
1. **Given** the UI deployment is up and `kubectl port-forward` is established, **When** I open `http://localhost:8080/`, **Then** I see a table of NHDs from the last 24h with columns: Created, Node, Cluster, Trigger, Phase, Decision, Confidence, View.
2. **Given** the list view is open, **When** 30 s pass, **Then** the table refreshes automatically without a full page reload.
3. **Given** I click "View" on any row, **When** the per-case page loads, **Then** I see case metadata, agent diagnosis (rootCause, rcaCategory, confidence, modelUsed, recommendation), and the evidence list rendered in human-readable form.
4. **Given** an NHD has 6 evidence entries with 4 KB results each, **When** the per-case page renders, **Then** each result is shown with a "show more / show less" toggle so the page doesn't drown in dense text.

---

### US3 — Uncordon and Clear-skipDeletion actions (Priority: P1)

**As an** on-call engineer who has just diagnosed a chaos run
**I want** a one-click way to uncordon the node and let MLC reclaim it
**So that** I don't have to copy-paste kubectl commands from the diagnosis text

**Why this priority:** P1 because the demo's catchy moment is "click button → node uncordoned, MLC takes over." Without this, the demo ends at "we showed you the diagnosis; now imagine clicking a button."

**Independent test:** Cordon a canary node manually (`kubectl cordon X` + annotate it with `skipDeletion=true`). Open the UI, navigate to a per-case page that maps to that node, click "Uncordon Node", confirm the modal. Verify with `kubectl get node X` that `.spec.unschedulable=false`. Click "Clear MLC skipDeletion", confirm. Verify the annotation is gone. Verify `kubectl get nhd <name> -o yaml` shows two new entries in `nodemedic.cf.newrelic.com/ui-action-history`.

**Acceptance scenarios:**
1. **Given** a per-case page is open for a cordoned node, **When** I click "Uncordon Node", **Then** a confirmation modal appears showing `kubectl uncordon <node>` as the equivalent command and the node's current state.
2. **Given** the confirmation modal is open, **When** I click "Confirm", **Then** the UI patches `node.spec.unschedulable=false` via the kube API, displays the success result, refreshes the page state, and adds a new entry to the NHD's `ui-action-history` annotation with `{ts, action: "uncordon", actor: "demo-anonymous", result: "ok"}`.
3. **Given** the node is already uncordoned, **When** I click "Uncordon Node" and confirm, **Then** the UI returns "already uncordoned (no change)" without erroring; no new audit entry is written.
4. **Given** the per-case page shows a node that has `machine-lifecycle.newrelic.com/skipDeletion=true`, **When** I click "Clear MLC skipDeletion" and confirm, **Then** the annotation is removed, the page refreshes, and the audit entry records the action.

---

### US4 — Drain action (Priority: P2)

**As an** on-call engineer who has decided the node is genuinely bad
**I want** to drain it via a button rather than running `kubectl drain` by hand
**So that** the action is auditable, the affected pods are visible before I commit, and PDBs are respected

**Why this priority:** P2 because Drain is an additional action on top of the demo's headline (cordon → uncordon round trip). It widens the demo narrative but isn't essential for the headline.

**Independent test:** Pick a canary node with at least 2 non-DaemonSet pods. Open per-case page. Click "Drain Node". Confirmation modal lists the pods that would be evicted. Confirm. Watch the UI report progress as each pod is evicted. Verify with `kubectl get pods -A --field-selector spec.nodeName=<node>` that only DaemonSet + mirror pods remain.

**Acceptance scenarios:**
1. **Given** a per-case page is open, **When** I click "Drain Node", **Then** a confirmation modal appears showing the list of pods that would be evicted (excluding DaemonSets, mirror pods, and `system-node-critical`-priority pods).
2. **Given** the confirmation modal is open, **When** I click "Confirm Drain", **Then** the UI evicts the listed pods one by one via the eviction API, displays per-pod success/skip/error, and on completion updates the NHD's audit annotation with the action and the per-pod result count.
3. **Given** a pod has a Pod Disruption Budget that would be violated, **When** the UI attempts to evict it, **Then** the apiserver returns a 429 ("would violate PDB"); the UI surfaces this clearly per pod and continues with the next pod rather than aborting the whole drain.
4. **Given** a drain is in flight (the UI is mid-loop), **When** I click "Drain" again, **Then** the button is disabled and a "drain in progress" indicator shows the per-pod progress instead of starting a second drain.

---

### US5 — Audit trail visible on the per-case page (Priority: P2)

**As an** investigator looking at a case after the fact
**I want** to see what the controller did automatically AND what humans did via the UI in one place
**So that** I can reconstruct the full action sequence without going to a separate audit log

**Why this priority:** P2 because the audit trail is in the NHD annotation regardless of whether the UI surfaces it; surfacing it is the polish that closes US3 + US4. Without US5, the audit is in YAML; with US5, it's in the page.

**Independent test:** Open a per-case page where the controller cordoned (US1 of Spec 001) and a human later clicked "Uncordon" (US3 of this spec). The page renders an "Action history" section with two entries: `Cordon (controller, 2026-06-14T15:35Z)` and `Uncordon (UI, demo-anonymous, 2026-06-14T15:42Z)`.

**Acceptance scenarios:**
1. **Given** the NHD's `status.action` carries the controller-stamped cordon decision **and** the `ui-action-history` annotation has one or more UI entries, **When** I open the per-case page, **Then** the "Action history" section shows both, sorted by timestamp, each with actor (`controller` vs `UI: demo-anonymous`), action type, and timestamp.
2. **Given** an NHD has no UI-initiated actions yet, **When** I open the per-case page, **Then** the "Action history" section shows only the controller's action.

---

### Edge cases

- The NHD referenced in the Slack URL no longer exists (e.g. CRD garbage-collected the case). The per-case page renders a 404-equivalent ("Case not found") with a link back to the list view.
- The cluster is unreachable from the UI pod (apiserver outage during a real incident). The list view renders an error banner ("apiserver unreachable: <error>") rather than a blank page; the per-case page does the same.
- A user clicks "Confirm Drain" and immediately closes the browser tab. The eviction loop continues server-side until done; the audit annotation reflects the final state. (Trade-off: the user doesn't see the result, but the action's audit trail is complete.)
- The action API is called with a node name that doesn't match the NHD's `spec.case.nodeName` (e.g. by hand-crafting a request). The backend refuses with 400; this is a defensive belt against mis-routed clicks even though the UI itself never produces such a request.
- A new NHD lands while the list view is open and auto-refresh is paused (browser backgrounded). When the tab regains focus the auto-refresh fires and the new NHD appears with a brief highlight to draw the eye.
- The NHD exists but the node it references has been deleted (e.g. MLC reclaimed it between Slack ping and engineer click). Action endpoints return HTTP 200 with `{result: "node no longer exists (already reclaimed)"}`, append a single audit entry, and the per-case page renders a "Node was reclaimed by MLC" banner with all three action buttons disabled (FR-17a).

---

## 6. Functional requirements

### Slack format (US1)

- **FR-1**: The Slack notifier MUST emit Block Kit JSON (not plain text) for every NHD post. Block Kit blocks include a `header` block (severity emoji + node name + cluster), a `section` block with a fields array (Decision, Confidence, Trigger, rcaCategory), and an `actions` block with a single `button` element labeled "View full diagnosis" carrying a URL of the form `<config.uiBaseURL>/cases/<nhd-name>`.
- **FR-2**: The header block's emoji MUST be `🚨` for `decision=Applied`, `⚠️` for `decision=HumanInLoop`, `❌` for `phase=Failed`. Header text MUST include the verb (`Cordoned`, `Needs review`, `Failed`). The Block Kit `attachment.color` MUST be `danger` for `Applied`, `warning` for `HumanInLoop`, `#808080` for `Failed`, and unset (no `attachment.color`) for `Diagnosing`/`Pending`. Slack's named aliases (`danger`/`warning`/`good`) render correctly on every client without per-theme hex tuning.
- **FR-3**: `config.uiBaseURL` MUST be a Helm value with default `http://localhost:8080` (the port-forward demo path). Operators flip this to a real URL when public ingress lands.
- **FR-4**: `config.slackChannel` MUST be a Helm value (display string only — the actual webhook routing is in the secret). Default: `#nodemedic-demo`. Visible in the controller's startup log line.

### List view (US2)

- **FR-5**: `GET /` MUST render an HTML page listing NHDs from the namespace the UI is deployed in, filtered to `creationTimestamp >= now - 24h`, sorted newest first.
- **FR-6**: The list view MUST auto-refresh every 30 s by re-fetching `GET /api/cases` (JSON) and re-rendering the table without a full page reload.
- **FR-7**: Each row MUST show: Created (relative + absolute on hover), Node, Cluster, Trigger.type/reason, Phase, Decision, Confidence, View button. The View button is a link to `/cases/<nhd-name>`.
- **FR-8**: Rows for `phase=Acted{Applied}` MUST be highlighted green; `Acted{HumanInLoop}` yellow; `Failed` red; `Diagnosing`/`Pending` neutral.

### Per-case view (US2 + US3 + US4 + US5)

- **FR-9**: `GET /cases/<nhd-name>` MUST render an HTML page showing the case metadata (case_id, node, cluster, provider, region, trigger.{type,reason,observedAt,message}), agent diagnosis (rootCause, recommendation.{action,reason}, rcaCategory, confidence, modelUsed, completedAt), evidence list, action history, and the three action buttons.
- **FR-9a**: Action button enablement is gated by node state, not NHD phase. Uncordon button is enabled iff `node.spec.unschedulable=true`; Clear-skipDeletion is enabled iff the node carries the `machine-lifecycle.newrelic.com/skipDeletion` annotation; Drain is enabled iff the node exists. Disabled buttons show a tooltip with the reason ("Already uncordoned", "Annotation already cleared", "Node not found"). When `phase ∈ {Diagnosing, Pending}` the page MUST show a status banner naming the current phase so the engineer knows the diagnosis isn't final; the banner does NOT gate button enablement.
- **FR-10**: Each evidence entry MUST be rendered with a header (source — the `ref` truncated to ~120 chars) and a collapsible body (the full `result`). Default collapsed for entries longer than 800 chars.
- **FR-11**: The "Action history" section MUST merge the controller's `status.action` (stamped at cordon time) with the UI's `nodemedic.cf.newrelic.com/ui-action-history` annotation entries, sorted by timestamp ascending. Each entry shows actor (`controller` or `UI: <actor>`), action verb (`Cordon`, `Uncordon`, `Drain`, `ClearSkipDeletion`), timestamp, and result (one-line summary).
- **FR-12**: If the NHD does not exist (or its `metadata.namespace` does not match the UI's configured namespace), the per-case page MUST render a "Case not found" panel with a link back to the list view; HTTP status 200 (not 404 — the page itself is found, the entity inside isn't).
- **FR-13**: If the apiserver is unreachable, the per-case page MUST render an error banner naming the underlying error and offering a "Retry" link; HTTP status 200.

### Actions (US3 + US4)

- **FR-14**: `POST /api/cases/<nhd-name>/actions/uncordon` MUST patch `node.spec.unschedulable=false` on the NHD's referenced node. If already false, return `{result:"already uncordoned (no change)"}` without writing the audit annotation. On success, append `{ts, action:"uncordon", actor:"demo-anonymous", result:"ok"}` to `nodemedic.cf.newrelic.com/ui-action-history` (JSON-encoded array).
- **FR-15**: `POST /api/cases/<nhd-name>/actions/clear-skip-deletion` MUST remove the `machine-lifecycle.newrelic.com/skipDeletion` annotation from the NHD's referenced node. If absent, return `{result:"already cleared (no change)"}`. On success, append the audit entry.
- **FR-16**: `POST /api/cases/<nhd-name>/actions/drain` MUST first list pods on the node (excluding DaemonSets, mirror pods, `system-node-critical` priority), evict each via `pods/eviction`, and stream per-pod progress via Server-Sent Events. The response carries `Content-Type: text/event-stream`; each per-pod outcome is emitted as `data: {pod, namespace, result, detail}\n\n`; a terminal `event: complete\ndata: {summary}\n\n` closes the stream. On completion, write a single audit entry summarizing the drain (per-pod counts: evicted, skipped, error). PDB violations (HTTP 429 from apiserver) MUST surface per-pod (`result:"error"`, detail names the PDB) and not abort the loop.
- **FR-17**: All three action endpoints MUST refuse with HTTP 400 if the request's NHD's `spec.case.nodeName` does not match the node the action would target (defensive belt against mis-routed clicks; the UI never produces such requests but the endpoint validates).
- **FR-17a**: All three action endpoints MUST treat a NotFound on the target node as a benign "already reclaimed" state — return HTTP 200 with `{result: "node no longer exists (already reclaimed)"}` and append a single audit entry capturing the action attempted and the not-found outcome. The per-case page MUST render a "Node was reclaimed by MLC" banner and disable all three action buttons in this state. This mirrors the idempotent "already uncordoned" path: the engineer's intent is mooted because the node is gone, which is the desired end state, not a failure.
- **FR-18**: All three action endpoints MUST emit a structured stdout log line per call: `{ts, event:"ui_action", action, nhd_name, node, actor:"demo-anonymous", result, duration_ms}`. The log line MUST be JSON, one per line, sortable by `ts`.
- **FR-18a**: The `ui-action-history` annotation is a **last-N FIFO ring buffer** with N=20. When appending an entry would exceed N, the oldest entry (index 0) is dropped before the new entry is appended. Existing entries are never mutated. The cap protects against etcd's per-object size ceiling for chatty cases (e.g. drain producing one entry per pod). Older audit lives in stdout logs (FR-18), not in the annotation.

### Hosting + ops

- **FR-19**: The UI binary MUST listen on `:8080` for both HTTP API endpoints and static-asset serving. No separate ports.
- **FR-20**: The Helm chart MUST refuse to render if `clusterName` is not in the allowlist (`cf1z`, `jc1z`, `sk1z`, or `test-*`). Mirrors the controller and agent charts.
- **FR-21**: The UI's ServiceAccount MUST carry only the verbs needed (Goal G8). The chart's rendered ClusterRole is the audit surface.
- **FR-22**: The image MUST be built per the same pattern as the controller (`Dockerfile.nodemedic-oncall-ui`, Makefile target, `cf-registry.nr-ops.net/container-fabric/nodemedic-oncall-ui:dev-cf1z-<sha>` tag).

### Cross-cutting

- **FR-23**: The UI binary MUST refuse to start if `CLUSTER_NAME` env is empty or doesn't match the allowlist (binary-level belt + suspenders to the chart guard, mirroring the controller's `validateClusterName`).
- **FR-24**: The UI MUST NOT cache NHD reads beyond a single request. Every list / detail render hits the apiserver. Trade-off: slightly chattier, but no staleness.

---

## 7. Key entities

The UI does not own a CRD. It reads, displays, and writes annotations on the existing entities owned by Specs 001 and 002.

- **NodeHealthDiagnosisAI (NHD)** (owned by Spec 001 controller). The UI reads `spec.case.*`, `status.diagnosis.*`, `status.action.*`, `status.conditions[*]`, and `metadata.annotations`. The UI writes a single annotation: `nodemedic.cf.newrelic.com/ui-action-history`.
- **Node** (kube core). The UI reads `spec.unschedulable`, `metadata.annotations.machine-lifecycle.newrelic.com/skipDeletion`, `status.conditions[type=Ready]`, `metadata.labels`. The UI writes `spec.unschedulable=false` (uncordon) and removes the `skipDeletion` annotation (clear-skip-deletion).
- **Pod** (kube core). The UI reads pods on the target node (drain pre-flight + display). The UI creates `pods/eviction` resources for each non-skip pod (drain).
- **ui-action-history** annotation (new, on the NHD CR). JSON-encoded array of `{ts, action, actor, result}` objects, capped at the last 20 entries (FIFO ring buffer — appending the 21st drops entry[0]). Each entry ~150–250 bytes; max array ~5 KB, comfortably below any apiserver limit. Schema-stable from v1; fields are append-only at the entry level (existing entries are never mutated, only dropped from the front when the cap is hit). Garbage-collected with the NHD itself. Older audit history lives in the UI pod's structured stdout logs (FR-18) and the controller's per-action log lines.

---

## 8. Acceptance criteria

These are the spec-bound, demo-verifiable gates. Each AC corresponds to one or more user stories and is independently rehearsable on cf1z.

- **AC-1 — Block Kit Slack post on Acted.** Hand-craft an NHD with `phase=Acted`, `decision=Applied`, `operation=cordon`, confidence ≥ 0.7, rcaCategory=Kubelet (or any non-empty value). The Slack channel receives a Block Kit message with: `🚨 Cordoned: <node> (<cluster>)` header, `attachment.color=danger` (red side bar), fields grid showing Decision/Confidence/Trigger/rcaCategory, "View full diagnosis" button URL = `<config.uiBaseURL>/cases/<nhd-name>`. (US1)
- **AC-2 — Block Kit Slack post on HumanInLoop.** Hand-craft an NHD with `decision=HumanInLoop` (low confidence path). The message header reads `⚠️ Needs review: <node> (<cluster>)`, `attachment.color=warning` (yellow side bar), with the same fields grid + button. (US1)
- **AC-3 — List view renders 24h of NHDs.** Pre-populate cf1z with at least 3 NHDs. Open `http://localhost:8080/` (after port-forward). The table shows all 3 sorted newest first; auto-refresh fires after 30 s. Click a row → land on per-case page. (US2)
- **AC-4 — Per-case view from Slack click-through.** From the Slack message in AC-1, click the "View full diagnosis" button. The browser opens `http://localhost:8080/cases/<nhd-name>` and renders the full diagnosis: case metadata, rootCause, evidence list with collapsible bodies, action history. (US1 + US2)
- **AC-5 — Uncordon button round-trip.** Pick a cordoned canary node. Open its per-case page. Click "Uncordon Node" → confirm. The node's `.spec.unschedulable` flips to `false` (verify via `kubectl get node`). The page refreshes showing the new state. The NHD's `ui-action-history` annotation has a new entry. (US3)
- **AC-6 — Idempotent uncordon.** On an already-uncordoned node, click "Uncordon Node" → confirm. The UI returns "already uncordoned (no change)". No new audit entry is written. (US3)
- **AC-7 — Clear skipDeletion button.** On a node with `skipDeletion=true`, click "Clear MLC skipDeletion" → confirm. The annotation is removed (verify with `kubectl get node <name> -o jsonpath='{.metadata.annotations}'`). MLC's next reconcile reclaims the VM (verify with `kubectl get node` showing the node disappears within ~30 s). Audit annotation records the action. (US3)
- **AC-8 — Drain button respects PDBs.** On a node with at least 2 non-DaemonSet pods, click "Drain Node". The confirmation modal lists the pods. Confirm. Pods are evicted one by one; if any has a violated PDB, that pod's eviction returns 429 surfaced in the UI but the loop continues. Final audit annotation records per-pod outcomes. (US4)
- **AC-9 — Action audit annotation.** After AC-5 + AC-7 on the same NHD, the annotation `nodemedic.cf.newrelic.com/ui-action-history` contains 2 entries: `{action:"uncordon", ...}` and `{action:"clear-skip-deletion", ...}`. (US3 + US5)
- **AC-10 — Combined action history view.** Open the per-case page from AC-9. The "Action history" section shows three entries in timestamp order: `Cordon (controller, ...)`, `Uncordon (UI: demo-anonymous, ...)`, `ClearSkipDeletion (UI: demo-anonymous, ...)`. (US5)
- **AC-11 — Chart cluster-name guard.** `helm template ./deployment/helm/nodemedic-oncall-ui --set clusterName=stg-foo` fails. With `clusterName=cf1z` succeeds. (FR-20)
- **AC-12 — RBAC review.** Captain renders the chart and inspects the ClusterRole. Verbs match the spec: `nodes get/list/watch/patch`, `pods/eviction create`, `pods get/list/watch`, `nodemedic.cf.newrelic.com/nodehealthdiagnosisais get/list/watch/patch`. No `delete`, no broader resource access. (FR-21 + Constitution Article I.1)
- **AC-13 — Structured action logs.** After AC-5 + AC-7 + AC-8, `kubectl logs deployment/nodemedic-oncall-ui --since=10m | jq 'select(.event == "ui_action")'` returns at least 3 lines, each carrying `ts`, `action`, `nhd_name`, `node`, `actor`, `result`, `duration_ms`. (FR-18 + Article I.4)
- **AC-14c — Action against reclaimed node.** Take an NHD whose `spec.case.nodeName` references a node that has been deleted (e.g. via `kubectl delete node <name>` after MLC reclaimed it; or rotate via natural CAPI flow). Click "Uncordon Node" on its per-case page. The UI receives HTTP 200 with `{result: "node no longer exists (already reclaimed)"}`. The page renders a "Node was reclaimed by MLC" banner with all three action buttons disabled. The NHD's `ui-action-history` annotation has a single new entry capturing the attempt + not-found outcome. (FR-17a)
- **AC-14b — Action-history ring buffer cap.** Drive 25 distinct UI actions against a single NHD (e.g. via a script that toggles uncordon/cordon repeatedly with `--force` or against multiple matching NHDs). Verify `kubectl get nhd <name> -o jsonpath='{.metadata.annotations.nodemedic\.cf\.newrelic\.com/ui-action-history}' | jq 'length'` returns 20 (not 25). The 5 oldest entries were dropped FIFO. The annotation's total size stays well below 8 KB. (FR-18a)
- **AC-14a — Buttons gated by node state (not NHD phase).** Open the per-case page for an NHD with `phase=Diagnosing` whose node is currently cordoned and carries `skipDeletion=true`. The page shows a "Diagnosis in progress" banner; the Uncordon and Clear-skipDeletion buttons are enabled (clickable). On a different NHD whose node is already uncordoned, the Uncordon button is disabled with tooltip "Already uncordoned". (FR-9a)
- **AC-14 — Demo finale walkthrough.** End-to-end: trigger chaos-kubelet on cf1z. Slack message lands in the configured channel. Engineer clicks the button. Per-case page opens. Engineer reads diagnosis, clicks Uncordon → confirm. Page refreshes; node is uncordoned; audit annotation grows. Total time from Slack ping to action: ≤ 60 s (excluding the chaos-trigger setup). (Composite of US1 + US2 + US3)

---

## 9. Demo flow (the punch line)

The NodeMedic demo has been building toward this. Here's the closing 90 seconds:

1. **0:00** — Engineer is in `#cf-oncall-alerts` (or whatever the user named it). Pre-canned chaos-kubelet job is triggered (off-screen, or visible in a side terminal as part of the narrative).
2. **0:30** — Slack message lands: header `🚨 Cordoned: cf1z-general-nodes-1000007 (cf1z)`, fields grid showing `Decision: Applied · Confidence: 0.92 · Trigger: KubeletUnhealthy · rcaCategory: Kubelet`. Primary button: "View full diagnosis".
3. **0:35** — Engineer clicks the button. (Port-forward was set up before the demo.) Browser opens to `http://localhost:8080/cases/cf1z-general-nodes-1000007-<seq>`.
4. **0:40** — Per-case page is on the screen. Header line names the node + cluster + decision. Below: a one-paragraph rootCause. Below: 6 evidence entries (kubectl, ssh, NRQL, ssh, ssh, kubectl) collapsed by default; engineer expands the iptables ssh probe to show the REJECT rule. Below: action history with "Cordon (controller, T+0:30)".
5. **0:55** — Engineer clicks "Uncordon Node". Modal shows the equivalent `kubectl uncordon` command and the node's current state. Confirm.
6. **1:00** — Page refreshes. Node is uncordoned; action history now has two entries. Engineer says: "And now MLC will rotate it on its next reconcile." (Off-screen: MLC's `TerminationSkipped → cordon ok` cycle; viewable in a side terminal.)
7. **1:30** — Engineer says "And here's the audit trail" → kubectl-side: `kubectl get nhd … -o yaml` showing the ui-action-history annotation. Demo lands.

The narrative is: NodeMedic detected, diagnosed, decided, acted; the engineer reviewed and chose the next step; the audit trail is one annotation; total time, well under two minutes.

---

## 10. Open questions

None at the spec level — all clarifications resolved in §0. Plan-phase questions to expect:

- Exact Block Kit JSON shape (which `section` block layout, explicit `text` fallback for clients that don't render Block Kit). Color attachment per severity is bound by the Clarifications session (see §0).
- Server-side rendering vs client-side fetch for the list/detail views (HTML templates vs HTMX vs vanilla `fetch`+template-literal).
- Pod eviction loop concurrency: serial-with-progress (US4) is the spec's contract; implementation may parallelize within a small concurrency cap.
- Auto-refresh implementation (`setInterval` vs `EventSource`/SSE). The spec says "every 30 s"; the plan picks the mechanism.

---

## 11. Glossary

| Term | Meaning |
|---|---|
| NHD | `NodeHealthDiagnosisAI` custom resource, owned by Spec 001 controller. |
| Per-case page | UI route `/cases/<nhd-name>` showing one NHD in detail. |
| List view | UI route `/` showing the last 24h of NHDs. |
| Action history | The combined view of `status.action` (controller) + `ui-action-history` annotation (UI) on a single NHD. |
| skipDeletion annotation | `machine-lifecycle.newrelic.com/skipDeletion=true`, makes MLC pause cordon-driven Machine deletion. Owned by Spec 001 controller's cordon path; can be cleared by the UI. |
| Block Kit | Slack's rich message format ([api.slack.com/block-kit](https://api.slack.com/block-kit)). What we replace plain-text posts with. |
| Demo-anonymous | The literal string `demo-anonymous` written into the `actor` field of UI-initiated action log lines and audit annotations, in lieu of an authenticated user identity. |
