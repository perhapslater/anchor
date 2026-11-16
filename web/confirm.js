(() => {
  "use strict";
  let dialog, title, detail, accept, cancel, pending = null, opener = null;
  function settle(confirmed) {
    if (!pending) return;
    const resolve = pending;
    pending = null;
    dialog.close();
    if (opener?.isConnected && !opener.disabled) opener.focus({ preventScroll: true });
    opener = null;
    resolve(confirmed);
  }
  function create() {
    dialog = document.createElement("dialog"); dialog.id = "anchor-confirm"; dialog.className = "anchor-confirm";
    dialog.setAttribute("role", "alertdialog"); dialog.setAttribute("aria-labelledby", "anchor-confirm-title"); dialog.setAttribute("aria-describedby", "anchor-confirm-detail");
    const brand = document.createElement("p"); brand.className = "eyebrow"; brand.textContent = "Anchor";
    title = document.createElement("h2"); title.id = "anchor-confirm-title";
    detail = document.createElement("p"); detail.id = "anchor-confirm-detail";
    const actions = document.createElement("div"); actions.className = "actions";
    cancel = document.createElement("button"); cancel.id = "anchor-confirm-cancel"; cancel.type = "button"; cancel.className = "secondary";
    accept = document.createElement("button"); accept.id = "anchor-confirm-accept"; accept.type = "button";
    cancel.addEventListener("click", () => settle(false)); accept.addEventListener("click", () => settle(true));
    dialog.addEventListener("cancel", event => { event.preventDefault(); settle(false); });
    dialog.addEventListener("keydown", event => {
      if (event.key !== "Tab") return;
      event.preventDefault();
      const controls = [cancel, accept].filter(button => !button.disabled);
      const index = controls.indexOf(document.activeElement);
      const next = index < 0 ? (event.shiftKey ? controls.length - 1 : 0) : (index + (event.shiftKey ? -1 : 1) + controls.length) % controls.length;
      controls[next]?.focus();
    });
    dialog.addEventListener("click", event => {
      if (event.target !== dialog) return;
      const bounds = dialog.getBoundingClientRect();
      if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) settle(false);
    });
    dialog.addEventListener("close", () => { if (!dialog.open) settle(false); });
    actions.append(cancel, accept); dialog.append(brand, title, detail, actions); document.body.append(dialog);
  }
  window.anchorConfirm = (options = {}) => {
    // A second request must not replace a decision already being made.
    if (pending) return Promise.resolve(false);
    if (!dialog) create();
    title.textContent = options.title || "Confirm action";
    detail.textContent = options.message || "";
    accept.textContent = options.confirmLabel || "Continue";
    accept.className = options.danger ? "secondary danger" : "primary";
    cancel.textContent = options.cancelLabel || "Cancel";
    accept.disabled = cancel.disabled = false;
    opener = document.activeElement;
    return new Promise(resolve => {
      pending = resolve;
      dialog.showModal();
      cancel.focus();
    });
  };
})();
