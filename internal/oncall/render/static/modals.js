// modals.js — small confirmation-modal scaffold. Phase 5 + 6 attach
// concrete confirm handlers via window.NMConfirmModal.
(function () {
  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function show(opts) {
    return new Promise((resolve) => {
      const backdrop = document.createElement("div");
      backdrop.className = "modal-backdrop";

      const meta = opts.meta ? `<pre class="modal-meta">${escapeHTML(opts.meta)}</pre>` : "";
      const extra = opts.bodyHTML || "";

      backdrop.innerHTML = `<div class="modal" role="dialog" aria-modal="true">
        <h3>${escapeHTML(opts.title || "Confirm action")}</h3>
        <p>${escapeHTML(opts.body || "")}</p>
        ${meta}
        ${extra}
        <div class="modal-actions">
          <button type="button" class="action-btn modal-cancel">Cancel</button>
          <button type="button" class="action-btn modal-confirm">${escapeHTML(opts.confirmLabel || "Confirm")}</button>
        </div>
      </div>`;

      function close(result) {
        document.body.removeChild(backdrop);
        document.removeEventListener("keydown", onKey);
        resolve(result);
      }
      function onKey(e) {
        if (e.key === "Escape") { close(false); }
        if (e.key === "Enter") { close(true); }
      }

      backdrop.addEventListener("click", (e) => {
        if (e.target === backdrop) close(false);
      });
      backdrop.querySelector(".modal-cancel").addEventListener("click", () => close(false));
      backdrop.querySelector(".modal-confirm").addEventListener("click", () => close(true));

      document.body.appendChild(backdrop);
      document.addEventListener("keydown", onKey);
      backdrop.querySelector(".modal-confirm").focus();
    });
  }

  window.NMConfirmModal = { show: show, escapeHTML: escapeHTML };
})();
