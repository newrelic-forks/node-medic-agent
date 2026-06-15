// detail.js — wires the per-case page's three action buttons to
// confirmation modals + POST + reload. Drain (Phase 6) replaces its
// modal body and reads the SSE stream via fetch+ReadableStream.
//
// MUST mirror tests/oncall_ui/unit/sse_parser_shape_test.go (T076a)
// for the SSE-frame parser. Algorithm: append decoded chunk to a
// buffer; split on "\n\n" to extract complete frames; for each frame,
// parse "event:" and "data:" lines; dispatch to onMessage / onComplete.
// Trailing partial frame is preserved as residual buffer until the
// next chunk arrives.
(function () {
  const result = document.getElementById("action-result");

  function setResult(html, ok) {
    if (!result) return;
    result.innerHTML = `<span class="${ok ? "ok" : "err"}">${html}</span>`;
  }

  async function postAction(verb, nhd) {
    const url = `/api/cases/${encodeURIComponent(nhd)}/actions/${verb}`;
    const resp = await fetch(url, { method: "POST", headers: { Accept: "application/json" } });
    let body = null;
    try { body = await resp.json(); } catch (e) { /* SSE drain has no JSON body on success */ }
    return { resp, body };
  }

  async function handleSimpleAction(button, verb) {
    const nhd = button.dataset.nhd;
    const node = button.dataset.node;
    const labels = {
      "uncordon": { title: "Uncordon node?", body: `This patches spec.unschedulable=false on the node.`, kc: `kubectl uncordon ${node}` },
      "clear-skip-deletion": { title: "Clear MLC skipDeletion?", body: `This removes the machine-lifecycle.newrelic.com/skipDeletion annotation; MLC will reclaim the VM on its next reconcile.`, kc: `kubectl annotate node ${node} machine-lifecycle.newrelic.com/skipDeletion-` },
    };
    const cfg = labels[verb];
    const ok = await window.NMConfirmModal.show({
      title: cfg.title, body: cfg.body, meta: cfg.kc, confirmLabel: "Confirm",
    });
    if (!ok) return;
    setResult("Working…", true);
    try {
      const { resp, body } = await postAction(verb, nhd);
      if (resp.ok) {
        setResult(`✓ ${(body && body.result) || "ok"} — reloading…`, true);
        setTimeout(() => window.location.reload(), 600);
      } else {
        const msg = (body && (body.message || body.error)) || `HTTP ${resp.status}`;
        setResult(`✗ ${msg}`, false);
      }
    } catch (e) {
      setResult(`✗ ${e.message}`, false);
    }
  }

  // ---------------------------------------------------------------
  // SSE parser — mirrors T076a Go-side reference.
  //
  // Algorithm (shared with sse_parser_shape_test.go):
  //   - Maintain a UTF-8 buffer string.
  //   - On each chunk: append, then loop splitting at "\n\n".
  //   - Each complete frame's lines are parsed: lines starting with
  //     "event:" set the event type (default "message"); lines
  //     starting with "data:" accumulate (with newlines in between);
  //     blank line terminates the frame.
  //   - The trailing partial (after the last "\n\n") is kept as the
  //     new buffer state.
  //   - At EOF we DO NOT dispatch the residual buffer (per SSE spec
  //     a frame requires a terminating blank line).
  // Edit this together with tests/oncall_ui/unit/sse_parser_shape_test.go.
  function parseSSEChunks(onFrame) {
    let buf = "";
    return function (chunk) {
      buf += chunk;
      while (true) {
        const idx = buf.indexOf("\n\n");
        if (idx < 0) return;
        const raw = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let event = "message";
        const dataLines = [];
        for (const line of raw.split("\n")) {
          if (line.startsWith("event:")) {
            event = line.slice(6).trim();
          } else if (line.startsWith("data:")) {
            dataLines.push(line.slice(5).trim());
          }
        }
        onFrame({ event, data: dataLines.join("\n") });
      }
    };
  }

  async function handleDrain(button) {
    const nhd = button.dataset.nhd;
    const node = button.dataset.node;
    const ok = await window.NMConfirmModal.show({
      title: "Drain node?",
      body: `Evict every non-DaemonSet, non-mirror, non-system-node-critical pod on ${node}. PDB violations surface per pod and the loop continues.`,
      meta: `kubectl drain ${node} --ignore-daemonsets --delete-emptydir-data`,
      confirmLabel: "Drain",
    });
    if (!ok) return;
    setResult("Drain starting…", true);

    const url = `/api/cases/${encodeURIComponent(nhd)}/actions/drain`;
    const resp = await fetch(url, { method: "POST", headers: { Accept: "text/event-stream" } });

    if (resp.status === 409) {
      let body = null;
      try { body = await resp.json(); } catch (e) {}
      const prog = body && body.progress;
      setResult(`✗ drain already in progress${prog ? ` — ${prog.evictedPods}/${prog.totalPods} evicted, ${prog.skippedPods} skipped, ${prog.erroredPods} errored` : ""}`, false);
      return;
    }
    if (!resp.ok) {
      let body = null;
      try { body = await resp.json(); } catch (e) {}
      setResult(`✗ ${(body && (body.message || body.error)) || `HTTP ${resp.status}`}`, false);
      return;
    }

    let progressEl = document.createElement("div");
    progressEl.className = "drain-progress";
    progressEl.innerHTML = "<div class='drain-pods'></div><div class='drain-summary'></div>";
    result.innerHTML = "";
    result.appendChild(progressEl);
    const podsEl = progressEl.querySelector(".drain-pods");
    const summaryEl = progressEl.querySelector(".drain-summary");

    const handleFrame = (frame) => {
      let payload = null;
      try { payload = JSON.parse(frame.data); } catch (e) { return; }
      if (frame.event === "complete") {
        summaryEl.textContent = `✓ complete — evicted=${payload.evicted} skipped=${payload.skipped} errored=${payload.errored} (${payload.durationMs}ms) — reloading…`;
        setTimeout(() => window.location.reload(), 1200);
        return;
      }
      const row = document.createElement("div");
      row.className = "drain-progress-row";
      const cls = payload.result === "evicted" ? "evicted" : payload.result === "skipped" ? "skipped" : "error";
      row.innerHTML = `<span class="${cls}">[${window.NMConfirmModal.escapeHTML(payload.result)}]</span> ${window.NMConfirmModal.escapeHTML(payload.namespace + "/" + payload.pod)}${payload.detail ? " — " + window.NMConfirmModal.escapeHTML(payload.detail) : ""}`;
      podsEl.appendChild(row);
    };
    const dispatch = parseSSEChunks(handleFrame);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder("utf-8");
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      dispatch(decoder.decode(value, { stream: true }));
    }
  }

  document.querySelectorAll(".action-btn[data-action]").forEach((btn) => {
    btn.addEventListener("click", (e) => {
      const verb = btn.dataset.action;
      if (btn.disabled) return;
      if (verb === "drain") {
        handleDrain(btn);
      } else {
        handleSimpleAction(btn, verb);
      }
    });
  });
})();
