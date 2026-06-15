// list.js — auto-refresh the list view's <tbody> every 30 s by
// fetching /api/cases (research R-4). Pauses on document.hidden;
// resumes on visibilitychange. Stays under 80 lines.

(function () {
  const REFRESH_MS = 30000;
  const tbody = document.getElementById("list-tbody");
  if (!tbody) return;
  let timer = null;

  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function formatRelative(iso) {
    const ms = Date.now() - new Date(iso).getTime();
    const s = Math.floor(ms / 1000);
    if (s < 60) return s + "s ago";
    const m = Math.floor(s / 60);
    if (m < 60) return m + "m ago";
    const h = Math.floor(m / 60);
    if (h < 24) return h + "h ago";
    return Math.floor(h / 24) + "d ago";
  }

  function fmtConfidence(c) {
    if (c === undefined || c === null || c < 0) return "—";
    return Number(c).toFixed(2);
  }

  function rowHTML(r) {
    const trigger = r.triggerType + (r.triggerReason ? ` <span class="trigger-reason">(${escapeHTML(r.triggerReason)})</span>` : "");
    return `<tr class="${escapeHTML(r.rowClass)}" data-nhd="${escapeHTML(r.nhdName)}">
      <td><time title="${escapeHTML(r.createdAt)}">${escapeHTML(formatRelative(r.createdAt))}</time></td>
      <td>${escapeHTML(r.nodeName)}</td>
      <td>${escapeHTML(r.clusterName)}</td>
      <td>${escapeHTML(r.triggerType)}${r.triggerReason ? ' <span class="trigger-reason">(' + escapeHTML(r.triggerReason) + ')</span>' : ''}</td>
      <td>${escapeHTML(r.phase)}</td>
      <td>${r.decision ? escapeHTML(r.decision) : "—"}</td>
      <td>${escapeHTML(fmtConfidence(r.confidence))}</td>
      <td><a class="view-link" href="/cases/${encodeURIComponent(r.nhdName)}">View</a></td>
    </tr>`;
  }

  async function refresh() {
    try {
      const r = await fetch("/api/cases", { headers: { Accept: "application/json" } });
      if (!r.ok) return;
      const rows = await r.json();
      if (!Array.isArray(rows)) return;
      if (rows.length === 0) {
        tbody.innerHTML = '<tr><td colspan="8" class="empty">No NHDs in the last 24 h.</td></tr>';
        return;
      }
      tbody.innerHTML = rows.map(rowHTML).join("");
    } catch (e) {
      // Swallow — the next tick will retry. SSR error banner handles
      // the page-load failure case.
    }
  }

  function start() {
    if (timer) return;
    timer = setInterval(refresh, REFRESH_MS);
  }
  function stop() {
    if (!timer) return;
    clearInterval(timer);
    timer = null;
  }

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { stop(); }
    else { refresh(); start(); }
  });

  start();
})();
