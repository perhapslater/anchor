// Local arrangement drafts. Saving is owned by Administration's revision-checked API.
(() => {
  "use strict";
  const sectionKey = name => String(name || "Apps").toLowerCase();
  function groupsFor(apps, sections) {
    const groups = [], byName = new Map();
    function add(name) {
      name = name || "Apps";
      const key = sectionKey(name);
      if (!byName.has(key)) { const group = { name, apps: [] }; groups.push(group); byName.set(key, group); }
      return byName.get(key);
    }
    sections.forEach(add);
    apps.forEach(app => add(app.Section).apps.push(app));
    return groups;
  }
  function moveApplication(apps, sections, id, section, before = null) {
    const source = apps.find(app => app.ID === id);
    if (!source) throw new Error("This application is no longer in the arrangement.");
    const groups = groupsFor(apps, sections), destination = groups.find(group => sectionKey(group.name) === sectionKey(section));
    if (!destination) throw new Error("Choose an existing dashboard section.");
    if (before === id) return apps.slice();
    if (before && !destination.apps.some(app => app.ID === before)) throw new Error("Choose a position in the destination section.");
    groups.forEach(group => { group.apps = group.apps.filter(app => app.ID !== id); });
    const index = before ? destination.apps.findIndex(app => app.ID === before) : destination.apps.length;
    destination.apps.splice(index, 0, { ...source, Section: destination.name });
    return groups.flatMap(group => group.apps);
  }
  const sameOrder = (a, b) => a.length === b.length && a.every((app, i) => app.ID === b[i].ID && sectionKey(app.Section) === sectionKey(b[i].Section));

  function create(root, options) {
    root.innerHTML = `<p class="help order-help" id="order-help">Drag a handle to move an application within or between sections. Click a handle to choose an exact position using the keyboard or touch. Changes apply when you save.</p>
      <div class="order-toolbar"><div><strong>Arrange dashboard</strong><span class="help">Everyone sees this order for the applications they can access.</span></div><div class="order-toolbar-actions"><button type="button" class="primary order-save" disabled>Save order</button><button type="button" class="secondary order-discard" disabled>Discard changes</button></div></div>
      <p class="order-status help" role="status" aria-live="polite"></p><div class="order-sections"></div>
      <dialog class="sheet order-move-dialog" aria-labelledby="order-move-title"><h2 id="order-move-title">Move application</h2><form id="order-move-form"><label class="field">Section<select class="order-move-section"></select></label><label class="field">Position<select class="order-move-position"></select></label><div class="actions"><button type="submit" class="primary">Move application</button><button type="button" class="secondary order-move-cancel">Cancel</button></div></form></dialog>`;
    const board = root.querySelector(".order-sections"), status = root.querySelector(".order-status"), save = root.querySelector(".order-save"), discard = root.querySelector(".order-discard");
    const dialog = root.querySelector("dialog"), sectionSelect = root.querySelector(".order-move-section"), positionSelect = root.querySelector(".order-move-position");
    let apps = [], original = [], sections = [], busy = false, drag = null, frame = 0, moving = null, trigger = null, suppressClickUntil = 0;
    const changed = () => !sameOrder(apps, original);
    function notify(text, error = false) { status.textContent = text; status.classList.toggle("error", error); }
    function updateControls() {
      save.disabled = busy || !changed(); discard.disabled = busy || !changed();
      root.querySelectorAll(".order-handle").forEach(handle => { handle.disabled = busy; });
    }
    function render() {
      board.replaceChildren();
      if (!apps.length) { const empty = document.createElement("p"); empty.className = "empty service-empty"; empty.textContent = "Add applications before arranging your dashboard."; board.append(empty); }
      for (const group of apps.length ? groupsFor(apps, sections) : []) {
        const section = document.createElement("section"); section.className = "order-section"; section.dataset.orderSection = group.name;
        const heading = document.createElement("h2"); heading.textContent = group.name;
        const count = document.createElement("span"); count.className = "order-count"; count.textContent = `${group.apps.length} application${group.apps.length === 1 ? "" : "s"}`;
        const header = document.createElement("div"); header.className = "order-section-heading"; header.append(heading, count);
        const grid = document.createElement("ol"); grid.className = "order-grid"; grid.setAttribute("aria-label", group.name + " application order");
        group.apps.forEach((app, index) => {
          const tile = document.createElement("li"); tile.className = "order-tile"; tile.dataset.orderApp = app.ID;
          const copy = document.createElement("div"); copy.className = "order-tile-copy";
          const name = document.createElement("strong"); name.textContent = app.Name;
          const detail = document.createElement("span"); detail.textContent = `${index+1}${app.Disabled ? " · Hidden" : ""}`; copy.append(name, detail);
          const handle = document.createElement("button"); handle.type = "button"; handle.className = "order-handle";
          handle.setAttribute("aria-label", "Move " + app.Name); handle.setAttribute("aria-describedby", "order-help"); handle.title = "Drag to move, or click to choose a position";
          handle.innerHTML = '<span aria-hidden="true">⠿</span>';
          handle.addEventListener("click", event => { if (!busy && (event.detail === 0 || performance.now() >= suppressClickUntil)) openMove(app.ID, handle); });
          tile.append(options.icon(app), copy, handle); grid.append(tile);
        });
        const empty = document.createElement("p"); empty.className = "order-empty"; empty.textContent = "Drop an application here"; empty.hidden = group.apps.length > 0;
        section.append(header, grid, empty); board.append(section);
      }
      updateControls();
    }
    function apply(id, section, before) {
      const next = moveApplication(apps, sections, id, section, before);
      if (sameOrder(apps, next)) return;
      apps = next; render(); options.onChange(changed());
      const group = groupsFor(apps, sections).find(group => group.apps.some(app => app.ID === id));
      const index = group.apps.findIndex(app => app.ID === id);
      notify(`Moved ${apps.find(app => app.ID === id).Name} to ${group.name}, position ${index+1}. ${changed() ? "Save order to apply." : "Original order restored."}`);
    }
    function handleFor(id) { return [...board.querySelectorAll("[data-order-app]")].find(tile => tile.dataset.orderApp === id)?.querySelector("button"); }
    function positions() {
      const group = groupsFor(apps, sections).find(group => group.name === sectionSelect.value);
      const others = group.apps.filter(app => app.ID !== moving);
      positionSelect.replaceChildren(...others.map((app, index) => { const option = document.createElement("option"); option.value = app.ID; option.textContent = `${index+1} · Before ${app.Name}`; return option; }));
      const end = document.createElement("option"); end.value = ""; end.textContent = others.length ? `${others.length+1} · Last in section` : "1 · First in section"; positionSelect.append(end);
    }
    function openMove(id, handle) {
      moving = id; trigger = handle;
      const group = groupsFor(apps, sections).find(group => group.apps.some(app => app.ID === id));
      root.querySelector("#order-move-title").textContent = "Move " + apps.find(app => app.ID === id).Name;
      sectionSelect.replaceChildren(...sections.map(name => { const option = document.createElement("option"); option.value = option.textContent = name; return option; }));
      sectionSelect.value = group.name; positions();
      const next = group.apps[group.apps.findIndex(app => app.ID === id)+1]; positionSelect.value = next?.ID || "";
      dialog.showModal(); sectionSelect.focus();
    }
    sectionSelect.addEventListener("change", positions);
    root.querySelector("form").addEventListener("submit", event => {
      event.preventDefault(); if (busy) return;
      const id = moving; apply(id, sectionSelect.value, positionSelect.value || null); dialog.close(); handleFor(id)?.focus();
    });
    root.querySelector(".order-move-cancel").addEventListener("click", () => dialog.close());
    dialog.addEventListener("close", () => { if (trigger?.isConnected) trigger.focus(); });
    dialog.addEventListener("click", event => { if (event.target === dialog) dialog.close(); });
    discard.addEventListener("click", () => { if (busy) return; cancelDrag(); apps = original.slice(); render(); options.onChange(false); notify("Changes discarded. Saved order restored."); });
    save.addEventListener("click", () => { if (!busy && changed()) options.onSave(); });

    function clearTarget() {
      board.querySelectorAll("[data-drop]").forEach(el => el.removeAttribute("data-drop"));
      if (drag) drag.target = null;
    }
    function findTarget() {
      clearTarget();
      const hit = document.elementFromPoint(drag.x, drag.y), section = hit?.closest("[data-order-section]");
      if (!section || !board.contains(section)) return;
      const tile = hit.closest("[data-order-app]");
      const group = groupsFor(apps, sections).find(group => group.name === section.dataset.orderSection);
      if (tile) {
        if (tile.dataset.orderApp === drag.id) return;
        const rect = tile.getBoundingClientRect(), grid = tile.parentElement;
        const singleColumn = grid.clientWidth < rect.width * 1.5;
        const after = singleColumn ? drag.y > rect.top + rect.height/2 : drag.x > rect.left + rect.width/2;
        const index = group.apps.findIndex(app => app.ID === tile.dataset.orderApp);
        drag.target = { section: group.name, before: after ? group.apps[index+1]?.ID || null : tile.dataset.orderApp };
        tile.dataset.drop = after ? "after" : "before";
      } else { drag.target = { section: group.name, before: null }; section.dataset.drop = "end"; }
    }
    function animate() {
      if (!drag?.started) return;
      const edge = 75, height = window.innerHeight;
      const speed = drag.y < edge ? -Math.ceil((edge-drag.y)/5) : drag.y > height-edge ? Math.ceil((drag.y-height+edge)/5) : 0;
      if (speed) window.scrollBy(0, Math.max(-18, Math.min(18, speed)));
      drag.ghost.style.transform = `translate(${drag.x-drag.offsetX}px, ${drag.y-drag.offsetY}px)`;
      findTarget(); frame = requestAnimationFrame(animate);
    }
    function cancelDrag() {
      cancelAnimationFrame(frame); clearTarget();
      if (drag) {
        drag.ghost?.remove(); drag.tile.classList.remove("is-dragging");
        if (drag.handle.hasPointerCapture(drag.pointer)) drag.handle.releasePointerCapture(drag.pointer);
      }
      drag = null; document.body.classList.remove("is-order-dragging");
    }
    board.addEventListener("pointerdown", event => {
      const handle = event.target.closest(".order-handle");
      if (!handle || busy || event.button !== 0 || !event.isPrimary) return;
      suppressClickUntil = 0;
      cancelDrag();
      const tile = handle.closest("[data-order-app]"), rect = tile.getBoundingClientRect();
      drag = { id: tile.dataset.orderApp, pointer: event.pointerId, tile, handle, startX: event.clientX, startY: event.clientY, x: event.clientX, y: event.clientY, offsetX: event.clientX-rect.left, offsetY: event.clientY-rect.top, started: false };
      handle.setPointerCapture(event.pointerId);
    });
    board.addEventListener("contextmenu", event => { if (event.target.closest(".order-handle")) event.preventDefault(); });
    window.addEventListener("pointermove", event => {
      if (!drag || event.pointerId !== drag.pointer) return;
      drag.x = event.clientX; drag.y = event.clientY;
      if (!drag.started && Math.hypot(drag.x-drag.startX, drag.y-drag.startY) > 7) {
        drag.started = true; drag.tile.classList.add("is-dragging");
        const ghost = drag.tile.cloneNode(true); ghost.classList.add("order-ghost"); ghost.setAttribute("aria-hidden", "true"); ghost.inert = true;
        ghost.querySelectorAll("[id]").forEach(el => el.removeAttribute("id")); ghost.style.width = drag.tile.getBoundingClientRect().width + "px";
        document.body.append(ghost); drag.ghost = ghost; document.body.classList.add("is-order-dragging"); animate();
      }
      if (drag.started) event.preventDefault();
    }, { passive: false });
    window.addEventListener("pointerup", event => {
      if (!drag || event.pointerId !== drag.pointer) return;
      if (drag.started) { drag.x = event.clientX; drag.y = event.clientY; findTarget(); }
      const { id, target, started } = drag; cancelDrag();
      if (started) { suppressClickUntil = performance.now()+300; if (target) { apply(id, target.section, target.before); handleFor(id)?.focus({ preventScroll: true }); } }
    });
    window.addEventListener("pointercancel", cancelDrag);
    window.addEventListener("blur", cancelDrag);
    window.addEventListener("keydown", event => { if (event.key === "Escape" && drag) { event.preventDefault(); suppressClickUntil = performance.now()+300; cancelDrag(); notify("Move cancelled."); } });
    return {
      load(nextApps, nextSections) { cancelDrag(); sections = groupsFor(nextApps, nextSections).map(group => group.name); apps = groupsFor(nextApps, sections).flatMap(group => group.apps); original = apps.slice(); render(); notify(""); options.onChange(false); },
      snapshot() { return { apps: apps.map(app => ({ ...app })), sections: sections.slice() }; },
      setBusy(value) { busy = value; if (value) cancelDrag(); updateControls(); },
      deactivate() { cancelDrag(); if (dialog.open) dialog.close(); },
      notify,
    };
  }
  window.anchorAppOrder = { create, groupsFor, moveApplication, sameOrder };
})();
