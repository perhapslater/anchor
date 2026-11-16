(() => {
  "use strict";
  const $ = (id) => document.getElementById(id),
    clone = (value) => JSON.parse(JSON.stringify(value));
  const split = (value) =>
      value
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean),
    lines = (value) =>
      value
        .split("\n")
        .map((x) => x.trim())
        .filter(Boolean);
  let state = null,
    editingApp = null,
    editingUser = null,
    imported = null,
    dirty = false,
    busy = false,
    appIdTouched = false,
    appIconTouched = false,
    importRevision = null;
  const dirtyForms = new Set();
  let userPhotoEditor = null;
  let authStatus = null, builderStep = -1, builderPreviewOpen = false;
  const editingStages = ["Details", "Appearance", "Connection", "Advanced"];
  const notificationForms = ["notifications-form", "webhook-form"];
  const applicationStages = ["Type", "App details", "Appearance", "Connection", "Review application"];
  let wizardActive = false;
  let applicationsView = "manage", appOrder = null;
  let userDialogOpener = null;
  let tailscaleDialogOpener = null, tailscaleGuideOpen = null;
  let tailscaleTestGeneration = 0, tailscaleTestRevision = null;
  const wizardSteps = [
    ["People", "users", "Add people and assign groups. You can link their Tailscale identities after configuring applications."],
    ["Applications", "apps", "Add applications and choose who can see them. Save or cancel the application editor to return to Anchor setup."],
    ["Notifications", "notifications", "Optionally enable ntfy, a Discord or JSON webhook, or both. Save each connection and send its test notification."],
    ["Tailscale", "tailscale", "Use Tailscale to give people private access to Anchor and sign them in automatically. Once you link each person's Tailscale account, they'll see the groups and applications assigned to their Anchor account."],
    ["Review setup", "review", "Check your saved setup and current connection, then finish. Optional connections can be completed later."],
  ];
  function message(text, error = false) {
    const editor = ["user", "tailscale"].find(name => $(name+"-dialog").open);
    if (error && editor) {
      const alert = $(editor+"-form-error");
      alert.textContent = text;
      alert.hidden = false;
      alert.focus();
      alert.scrollIntoView({ block: "nearest" });
      return;
    }
    $("message").hidden = false;
    $("message").textContent = text;
    $("message").classList.toggle("error", error);
    $("message").scrollIntoView({ block: "nearest" });
  }
  function v(id) {
    return $(id).value.trim();
  }
  function set(id, value) {
    $(id).value = value ?? "";
  }
  async function api(path, body, method = "POST") {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 20_000);
    try {
      const r = await fetch(
        path,
        body === undefined
          ? { cache: "no-store", signal: controller.signal }
          : {
              method,
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(body),
              signal: controller.signal,
            },
      );
      const result = await r.json();
      if (path === "/api/auth/status" && r.headers?.get("X-Anchor-Dev-Identities") === "1") result.simulated = true;
      if (!r.ok) {
        if (r.status === 401 || r.status === 403)
          throw new Error("Sign in as an administrator, then reload this page.");
        throw new Error(result.message || "The operation failed.");
      }
      return result;
    } catch (e) {
      if (controller.signal.aborted)
        throw new Error("Anchor did not respond in time. Please try again.");
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }
  async function refresh() {
    state = await api("/api/admin/state");
    render();
    dirty = false;
    dirtyForms.clear();
  }
  async function work(action) {
    if (busy) return;
    busy = true;
    appOrder?.setBusy(true);
    const controls = [...document.querySelectorAll("button, input, select, textarea")].map(control => [control, control.disabled]);
    controls.forEach(([control]) => (control.disabled = true));
    $("content").setAttribute("aria-busy", "true");
    try {
      await action();
    } catch (e) {
      message(e.message, true);
    } finally {
      busy = false;
      controls.forEach(([control, disabled]) => (control.disabled = disabled));
      appOrder?.setBusy(false);
      $("content").removeAttribute("aria-busy");
      if (state) { renderDelivery(); syncWebhook(); if (wizardActive) renderWizard(); }
    }
  }
  async function save(config, text = "Saved.", keys) {
    await api("/api/admin/config", { revision: state.revision, config, keys });
    await refresh();
    dirtyForms.clear();
    message(text);
  }
  function button(text, action, kind = "secondary", saving = true) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = kind;
    b.textContent = text;
    b.addEventListener("click", async () => {
      if (!await discard()) return;
      dirty = false;
      dirtyForms.clear();
      if (saving) await work(action);
      else await action();
    });
    return b;
  }
  function serviceStatus(text, tone = "neutral") {
    const badge = document.createElement("span"); badge.className = "service-status"; badge.dataset.tone = tone; badge.textContent = text; return badge;
  }
  function serviceIcon(app) {
    const icon = document.createElement("span"); icon.className = "service-icon"; icon.setAttribute("aria-hidden", "true");
    const letter = document.createElement("span"); letter.textContent = Array.from(app.Name)[0]?.toUpperCase() || "A"; icon.append(letter);
    const bundled = (state.icons || []).find(item => item.name === app.Icon);
    const src = bundled ? "/api/admin/icons/" + encodeURIComponent(app.Icon) : /^https?:\/\//i.test(app.Icon || "") ? app.Icon : app.Icon ? "/icons/" + encodeURIComponent(app.Icon) : "";
    if (src) {
      const image = document.createElement("img"); image.src = src; image.alt = ""; image.width = image.height = 42; image.loading = "lazy";
      letter.hidden = true; image.addEventListener("error", () => { image.hidden = true; letter.hidden = false; }); icon.append(image);
    }
    return icon;
  }
  function serviceCard(app, type, edit, kind = "app") {
    const card = document.createElement("article"); card.className = "service-card"; card.dataset.appId = app.ID;
    const titleID = `service-title-${kind}-${app.ID}`; card.setAttribute("aria-labelledby", titleID);
    const header = document.createElement("div"); header.className = "service-header";
    const heading = document.createElement("div"); heading.className = "service-heading";
    const name = document.createElement("h2"); name.id = titleID; name.textContent = app.Name;
    const subtitle = document.createElement("span"); subtitle.className = "service-type"; subtitle.textContent = type; heading.append(name, subtitle);
    header.append(serviceIcon(app), heading, edit); card.append(header); return card;
  }
  function audienceText(app) {
    const access = app.Access || [];
    if (access.some(rule => rule.trim().toLowerCase() === "everyone")) return "Everyone";
    const others = access.filter(rule => rule.trim().toLowerCase() !== "admin").map(rule => state.users.find(user => user.id === rule.trim().toLowerCase())?.name || rule);
    return others.length ? "Administrators, " + others.join(", ") : "Administrators only";
  }
  function serviceBody(app, badges) {
    const body = document.createElement("div"); body.className = "service-body";
    if (app.Description) { const description = document.createElement("p"); description.className = "service-description"; description.textContent = app.Description; body.append(description); }
    const statuses = document.createElement("div"); statuses.className = "service-statuses"; statuses.append(...badges); body.append(statuses); return body;
  }
  function serviceDetails(label, entries) {
    const details = document.createElement("details"); details.className = "service-details";
    const summary = document.createElement("summary"); summary.textContent = label;
    const facts = document.createElement("dl"); facts.className = "connection-summary";
    for (const [name, value] of entries) addFact(facts, name, value);
    details.append(summary, facts); return details;
  }
  function applicationCard(app, index, cfg) {
    const requests = connections("requests", cfg).some(connection => connection.App === app.ID);
    const ebooks = connections("ebooks", cfg).some(connection => connection.App === app.ID);
    const type = requests && ebooks ? "Requests & ebook library" : ebooks ? "Ebook library" : requests ? "Seerr requests & releases" : app.Guide ? "Welcome guide" : "Standard application";
    const card = serviceCard(app, type, button("Edit", () => editApp(index), "secondary", false));
    card.classList.add("application-card");
    const badges = [serviceStatus(app.Disabled ? "Hidden from dashboard" : "Shown on dashboard", app.Disabled ? "neutral" : "good")];
    if (app.Maintenance) badges.push(serviceStatus("Maintenance", "warning"));
    if (app.Mobile) badges.push(serviceStatus("Phone-app instructions"));
    const sources = Object.keys(featureTypes).flatMap(kind => connections(kind, cfg).filter(connection => connection.App === app.ID).map(connection => ({kind, connection})));
    if (sources.length) {
      const missing = sources.some(({kind, connection}) => (kind === "ebooks" && !connection.LibraryID) || connectionProviders(kind, connection).some(([, provider, url, required]) => (required || url) && !(url && state.keys[builderSecretName(kind, provider, app.ID)])));
      badges.push(serviceStatus(missing ? "Needs setup" : "Configured", missing ? "warning" : "good"));
      if (sources.some(({kind, connection}) => kind === "requests" && (connection.Sonarr || connection.Radarr))) badges.push(serviceStatus("Coming up"));
    }
    card.append(serviceBody(app, badges));
    const facts = document.createElement("dl"); facts.className = "service-facts";
    addFact(facts, "Section", app.Section || "Apps"); addFact(facts, "Visible to", audienceText(app)); card.append(facts);
    const entries = [["Address", app.URL], ["Shortcut ID", app.ID], ["Health checks", app.Check === "off" ? "Off" : "Enabled"], ["Home-screen shortcut", app.HomeScreen === false ? "Not offered" : "Offered"]];
    if (app.Maintenance) entries.push(["Maintenance note", app.Maintenance]);
    const detailsGroup = document.createElement("div"); detailsGroup.className = "service-detail-group";
    detailsGroup.append(serviceDetails("Application details", entries)); card.append(detailsGroup);
    if (sources.length) {
      const details = document.createElement("details"); details.className = "service-details application-connections";
      const summary = document.createElement("summary"); summary.textContent = "Connection details"; details.append(summary);
      for (const {kind, connection} of sources) {
        const title = document.createElement("p"); title.className = "help"; title.textContent = kind === "requests" ? "Seerr requests & releases" : "Ebook library"; details.append(title);
        const list = document.createElement("ul"); list.className = "integration-services";
        const facts = document.createElement("dl"); facts.className = "connection-summary";
        for (const [name, provider, url, required] of connectionProviders(kind, connection)) {
          const saved = !!(url && state.keys[builderSecretName(kind, provider, app.ID)]);
          const item = document.createElement("li"), label = document.createElement("span"); label.textContent = name;
          item.append(label, serviceStatus(saved ? "Credential saved" : url ? "Key needed" : required ? "Server needed" : "Not added · optional", saved ? "good" : required || url ? "warning" : "neutral")); list.append(item);
          if (url) addFact(facts, name + " server", url);
        }
        if (kind === "ebooks") addFact(facts, "Library ID", connection.LibraryID);
        details.append(list, facts);
      }
      const note = document.createElement("p"); note.className = "help"; note.textContent = "Saved API credentials stay out of YAML exports. Full database backups include them."; details.append(note);
      detailsGroup.append(details);
    }
    const actions = document.createElement("div"); actions.className = "service-actions";
    const remove = button("Remove", async () => {
      if (!await window.anchorConfirm({ title: "Remove application?", message: `Remove ${app.Name}? Existing shortcuts for this tile will stop opening it.`, confirmLabel: "Remove application", cancelLabel: "Keep application", danger: true })) return;
      await work(async () => {
        const c = clone(cfg); c.Apps.splice(index, 1); removeConnections(c, app.ID); await save(c);
      });
    }, "secondary danger", false);
    actions.append(remove); card.append(actions); return card;
  }
  function addFact(list, label, value) {
    const row = document.createElement("div"), term = document.createElement("dt"), detail = document.createElement("dd");
    term.textContent = label; detail.textContent = value; row.append(term, detail); list.append(row);
  }
  function personCard(user) {
    const card = document.createElement("article"); card.className = "person-card";
    const header = document.createElement("div"); header.className = "person-header";
    const avatar = document.createElement("span"); avatar.className = "person-avatar"; avatar.setAttribute("aria-hidden", "true");
    window.anchorAvatar.render(avatar, user);
    const heading = document.createElement("div"); heading.className = "person-heading";
    const name = document.createElement("h2"); name.className = "person-name"; name.textContent = user.name;
    const role = document.createElement("span"); role.className = "person-role";
    role.textContent = !user.enabled ? "Account paused" : user.groups.some(group => group.toLowerCase() === "admin") ? "Administrator" : "Member";
    heading.append(name, role);
    const edit = button("Edit", () => editUser(user), "secondary", false); edit.setAttribute("aria-label", "Edit "+user.name); edit.dataset.userId = user.id;
    header.append(avatar, heading, edit); card.append(header);
    const groups = [...user.groups].sort((a, b) => Number(b.toLowerCase() === "admin") - Number(a.toLowerCase() === "admin"));
    const tags = document.createElement("div"); tags.className = "person-tags";
    for (const group of groups.slice(0,3)) { const tag = document.createElement("span"); tag.className = "person-tag"; tag.textContent = group; tags.append(tag); }
    if (groups.length > 3) { const more = document.createElement("span"); more.className = "person-tag"; more.textContent = "+"+(groups.length-3); more.title = groups.slice(3).join(", "); tags.append(more); }
    card.append(tags);
    const methods = document.createElement("div"); methods.className = "person-methods"; methods.setAttribute("aria-label", "Sign-in methods"); methods.setAttribute("role", "group");
    for (const label of [user.has_password && "Password", user.tailscale && "Tailscale"].filter(Boolean)) { const item = document.createElement("span"); item.className = "person-method"; item.textContent = label; methods.append(item); }
    card.append(methods);
    const details = document.createElement("details"), summary = document.createElement("summary"); summary.textContent = "Account details";
    const facts = document.createElement("dl"); facts.className = "connection-summary";
    addFact(facts, "Local login", user.has_password ? user.username : "Not enabled");
    if (user.tailscale) addFact(facts, "Tailscale email", user.tailscale);
    if (groups.length > 3) addFact(facts, "Groups", groups.join(", "));
    details.append(summary, facts);
    const remove = button("Remove user", () => removeUser(user), "secondary danger", false);
    remove.setAttribute("aria-label", "Remove " + user.name);
    const protectedAdmin = user.enabled && user.has_password && user.groups.some(group => group.toLowerCase() === "admin") &&
      !state.users.some(other => other.id !== user.id && other.enabled && other.has_password && other.groups.some(group => group.toLowerCase() === "admin"));
    remove.disabled = protectedAdmin;
    if (protectedAdmin) {
      const help = document.createElement("p"); help.className = "help"; help.id = "keep-admin-" + user.id;
      help.textContent = "Keep at least one enabled administrator with a local password. You can still edit this account.";
      remove.setAttribute("aria-describedby", help.id); details.append(help);
    }
    details.append(remove); card.append(details); return card;
  }
  async function removeUser(user) {
    const self = user.id === authStatus?.user?.id;
    if (!await window.anchorConfirm({ title: "Remove user?", message: `Remove ${user.name}? Their Anchor account, sign-in links and sessions will be removed. Their direct access rules will be removed; applications and announcements with no remaining audience become administrators-only.${self ? " You are removing your own account and will be signed out." : ""}`, confirmLabel: "Remove user", cancelLabel: "Keep user", danger: true })) return;
    let removed = false;
    await work(async () => {
      await api("/api/admin/users/" + encodeURIComponent(user.id), { revision: state.revision }, "DELETE");
      if (self) { location.replace("/account.html"); return; }
      await refresh(); message("User removed."); removed = true;
    });
    if (removed) $("add-user").focus();
  }
  async function discard() {
    return !(dirty || userPhotoEditor?.dirty()) || window.anchorConfirm({ title: "Discard unsaved changes?", message: "Your unsaved form changes will be lost.", confirmLabel: "Discard changes", cancelLabel: "Keep editing", danger: true });
  }
  async function setApplicationsView(view, checkDraft = true) {
    if (checkDraft && view === applicationsView) return true;
    if (checkDraft && !await discard()) return false;
    applicationsView = view;
    if (checkDraft && (dirty || !$('app-form').hidden)) {
      render(); dirty = false; dirtyForms.clear();
    }
    for (const name of ["manage", "arrange"]) {
      const selected = name === view, tab = $("applications-"+name+"-tab");
      tab.setAttribute("aria-selected", String(selected)); tab.tabIndex = selected ? 0 : -1;
      $("applications-"+name).hidden = !selected;
    }
    if (view !== "arrange") appOrder?.deactivate();
    syncSetupActions();
    return true;
  }
  async function saveApplicationOrder() {
    await work(async () => {
      try {
        const draft = appOrder.snapshot(), cfg = clone(state.config);
        cfg.Apps = draft.apps; cfg.Sections = draft.sections;
        await api("/api/admin/config", { revision: state.revision, config: cfg });
        await refresh(); appOrder.notify("Dashboard order saved.");
      } catch (error) { appOrder.notify(error.message + " Your arrangement has been kept; reload Administration if settings changed elsewhere.", true); }
    });
  }
  async function showPanel(panel) {
    if (!await discard()) return false;
    if (dirty || applicationEditorActive()) {
      render();
      $("import-yaml").value = "";
      $("import-preview").hidden = true;
      imported = null;
    }
    dirty = false;
    dirtyForms.clear();
    if (panel === "integrations") panel = "apps";
    document.querySelectorAll("[data-view]").forEach(el => {
      el.hidden = el.dataset.view !== panel && !(el.dataset.view === "security" && panel === "tailscale");
    });
    if (panel === "security" || panel === "tailscale") setConnectionView(panel === "tailscale" ? "tailscale" : "overview");
    document
      .querySelectorAll("[data-panel]")
      .forEach((el) =>
        el.setAttribute("aria-pressed", String(el.dataset.panel === (panel === "tailscale" ? "security" : panel))),
      );
    $("message").hidden = true;
    syncSetupActions();
    return true;
  }
  function render() {
    const cfg = state.config;
    cfg.Apps = cfg.Apps || [];
    cfg.Sections = cfg.Sections || [];
    renderIconOptions();
    $("loading").hidden = true;
    $("content").hidden = false;
    $("app-list").replaceChildren();
    if (!cfg.Apps.length) {
      const empty = document.createElement("p");
      empty.className = "empty service-empty";
      empty.textContent = "No applications yet. Add your first service above.";
      $("app-list").append(empty);
    }
    cfg.Apps.forEach((app, i) => $("app-list").append(applicationCard(app, i, cfg)));
    if (!appOrder) appOrder = window.anchorAppOrder.create($("applications-arrange"), {
      icon: serviceIcon,
      onSave: saveApplicationOrder,
      onChange(changed) { if (changed) dirtyForms.add("application-order"); else dirtyForms.delete("application-order"); dirty = dirtyForms.size > 0; },
    });
    appOrder.load(cfg.Apps, cfg.Sections);
    setApplicationsView(applicationsView, false);
    $("user-list").replaceChildren(...state.users.map(personCard));
    $("sections-list").replaceChildren(
      ...cfg.Sections.map((x) => {
        const o = document.createElement("option");
        o.value = x;
        return o;
      }),
    );
    $("access-reference").textContent =
      "User IDs: " + state.users.map((u) => `${u.name}: ${u.id}`).join(" | ");
    renderNotification("ntfy"); renderNotification("webhook");
    renderSecurity();
    renderTailscale();
    set("settings-title", cfg.Title);
    set("settings-subtitle", cfg.Subtitle);
    set("settings-contact", cfg.Contact);
    set("settings-interval", cfg.CheckInterval || "60s");
    set("settings-sections", cfg.Sections.join("\n"));
    set("notice-message", cfg.Notice?.Message);
    set("notice-style", cfg.Notice?.Style || "info");
    set("notice-access", (cfg.Notice?.Access || []).join(", "));
    $("app-form").hidden = true;
    $("app-list").hidden = false;
    $("add-app").hidden = false;
    if ($("user-dialog").open) $("user-dialog").close();
    setWizardActive(wizardActive);
    if (wizardActive) renderWizard();
  }
  function normalizedIconName(name) {
    return name.toLowerCase().replace(/[^a-z0-9]/g, "");
  }
  function matchingIcon(name) {
    const key = normalizedIconName(name);
    return (state?.icons || []).find(
      (icon) =>
        normalizedIconName(icon.name) === key ||
        normalizedIconName(icon.label) === key,
    );
  }
  function renderNotification(destination) {
    const cfg = state.config;
    if (destination === "ntfy") {
      credentialField("keys-ntfy", "key-ntfy", "clear-ntfy", "ntfy access token", "Publishing permission for the selected topic.", state.keys.ntfy);
      $("notify-enabled").checked = !!cfg.Notify;
      set("notify-server", cfg.Notify?.Server); set("notify-topic", cfg.Notify?.Topic);
      $("notify-alerts").checked = cfg.Notify?.Alerts !== false;
      $("notify-reports").checked = cfg.Notify?.Reports !== false;
      for (const id of ["notify-server", "notify-topic"]) $(id).required = !!cfg.Notify;
    } else {
      credentialField("keys-webhook", "key-webhook", "clear-webhook", "Webhook URL", "HTTPS endpoint. Blank keeps the saved URL.", state.keys.webhook);
      $("webhook-enabled").checked = !!cfg.Webhook;
      set("webhook-format", cfg.Webhook?.Format || "discord");
      $("webhook-alerts").checked = cfg.Webhook?.Alerts !== false;
      $("webhook-reports").checked = cfg.Webhook?.Reports !== false;
      syncWebhook();
    }
    renderDelivery();
  }
  async function saveNotification(destination, config, keys) {
    await api("/api/admin/config", { revision: state.revision, config, keys });
    state = await api("/api/admin/state");
    renderNotification(destination);
    dirtyForms.delete(destination === "ntfy" ? "notifications-form" : "webhook-form");
    dirty = dirtyForms.size > 0;
    message(destination === "ntfy" ? "ntfy settings saved." : "Webhook settings saved.");
  }
  function selectTab(group, selected) {
    for (const tab of $(group).querySelectorAll('[role="tab"]')) {
      const active = tab.id === selected;
      tab.setAttribute("aria-selected", String(active)); tab.tabIndex = active ? 0 : -1;
      $(tab.getAttribute("aria-controls")).hidden = !active;
    }
  }
  function wireTabs(group, select) {
    const tabs = [...$(group).querySelectorAll('[role="tab"]')];
    tabs.forEach((tab, index) => {
      tab.addEventListener("click", () => select(tab));
      tab.addEventListener("keydown", event => {
        if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
        event.preventDefault();
        const next = event.key === "Home" ? 0 : event.key === "End" ? tabs.length - 1 : (index + (event.key === "ArrowLeft" ? -1 : 1) + tabs.length) % tabs.length;
        select(tabs[next]); tabs[next].focus();
      });
    });
  }
  function setConnectionView(view) {
    selectTab("connection-tabs", "connection-" + view + "-tab");
  }
  function renderIconOptions() {
    $("bundled-icons").replaceChildren(
      ...(state.icons || []).map((icon) => {
        const option = document.createElement("option");
        option.value = icon.name;
        option.label = icon.label + " - " + icon.category;
        return option;
      }),
    );
  }
  function updateIconPreview() {
    const icon = (state.icons || []).find(
      (icon) => icon.name === v("app-icon"),
    );
    $("icon-preview").hidden = !icon;
    if (icon) {
      $("icon-preview-image").src =
        "/api/admin/icons/" + encodeURIComponent(icon.name);
      $("icon-preview-label").textContent = icon.label + " - bundled icon";
    } else {
      $("icon-preview-image").removeAttribute("src");
      $("icon-preview-label").textContent = "";
    }
    for (const button of $("icon-choices").querySelectorAll("button")) {
      button.setAttribute(
        "aria-pressed",
        String(button.dataset.icon === v("app-icon")),
      );
    }
  }
  function renderIconChoices() {
    const query = v("icon-search").toLowerCase();
    const icons = (state.icons || []).filter((icon) =>
      [icon.name, icon.label, icon.category].some((text) =>
        text.toLowerCase().includes(query),
      ),
    );
    $("icon-count").textContent =
      icons.length +
      " bundled service icon" +
      (icons.length === 1 ? "" : "s") +
      (query ? " found" : " available");
    $("icon-choices").replaceChildren(
      ...icons.map((icon) => {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "icon-option";
        button.dataset.icon = icon.name;
        button.setAttribute("aria-label", "Use " + icon.label + " icon");
        button.setAttribute(
          "aria-pressed",
          String(icon.name === v("app-icon")),
        );
        const image = document.createElement("img");
        image.src = "/api/admin/icons/" + encodeURIComponent(icon.name);
        image.width = 36;
        image.height = 36;
        image.alt = "";
        image.loading = "lazy";
        image.decoding = "async";
        const label = document.createElement("span");
        label.textContent = icon.label;
        const category = document.createElement("small");
        category.textContent = icon.category;
        button.append(image, label, category);
        button.addEventListener("click", () => {
          set("app-icon", icon.name);
          $("app-icon").dispatchEvent(new Event("input", { bubbles: true }));
        });
        return button;
      }),
    );
  }
  async function editApp(index, preferredType) {
    if (!await discard()) return;
    appOrder?.load(state.config.Apps, state.config.Sections);
    setApplicationsView("manage", false);
    $("message").hidden = true;
    editingApp = index;
    appIdTouched = index !== null;
    appIconTouched = index !== null;
    const app = index === null ? {} : state.config.Apps[index];
    $("app-form").reset();
    $("app-form").noValidate = true;
    $("app-form").classList.toggle("is-editing", index !== null);
    const editing = index !== null;
    $("edit-application-type").hidden = !editing;
    $("edit-application-type").open = false;
    (editing ? $("edit-application-type") : document.querySelector('[data-app-step="-1"]')).append($("application-type-fields"));
    $("builder-tabs").classList.toggle("applications-tabs", editing);
    $("builder-tabs").setAttribute("role", editing ? "tablist" : "navigation");
    $("builder-tabs").setAttribute("aria-label", editing ? "Application settings" : "Application setup steps");
    document.querySelectorAll("[data-step]").forEach(button => {
      const step = Number(button.dataset.step);
      button.hidden = editing && step === -1;
      button.textContent = editing && step !== -1 ? editingStages[step] : applicationStages[step + 1];
      if (editing && step !== -1) {
        button.id = "app-settings-tab-" + step; button.setAttribute("role", "tab"); button.setAttribute("aria-controls", "app-settings-panel-" + step);
      } else { button.removeAttribute("role"); button.removeAttribute("aria-controls"); button.removeAttribute("aria-selected"); button.removeAttribute("tabindex"); }
    });
    document.querySelectorAll("[data-app-step]").forEach(panel => {
      const step = Number(panel.dataset.appStep);
      if (editing && step !== -1) { panel.id = "app-settings-panel-" + step; panel.setAttribute("role", "tabpanel"); panel.setAttribute("aria-labelledby", "app-settings-tab-" + step); }
      else { panel.removeAttribute("role"); panel.removeAttribute("aria-labelledby"); }
    });
    $("app-review-summary").hidden = editing;
    $("app-save").textContent = editing ? "Save changes" : "Save application";
    $("cancel-app").textContent = editing ? "Cancel" : "Cancel application";
    builderPreviewOpen = false;
    $("app-form").hidden = false;
    $("app-list").hidden = true;
    $("add-app").hidden = true;
    $("app-form-title").textContent =
      index === null ? "Add application" : "Edit application";
    for (const [id, key] of [
      ["name", "Name"],
      ["id", "ID"],
      ["url", "URL"],
      ["description", "Description"],
      ["section", "Section"],
      ["icon", "Icon"],
      ["check", "Check"],
      ["maintenance", "Maintenance"],
    ])
      set("app-" + id, app[key]);
    $("icon-search").value = "";
    $("icon-browser").open = false;
    renderIconChoices();
    updateIconPreview();
    $("app-id").readOnly = index !== null;
    set("app-access", (app.Access || ["everyone"]).join(", "));
    $("app-disabled").checked = !!app.Disabled;
    $("app-alerts").checked = app.Alerts !== false;
    $("app-homescreen").checked = app.HomeScreen !== false;
    const m = app.Mobile || {};
    for (const [id, key] of [
      ["ios", "ios"],
      ["android", "android"],
      ["iosapp", "iosApp"],
      ["androidapp", "androidApp"],
      ["label", "openLabel"],
      ["server", "server"],
      ["note", "note"],
    ])
      set("mobile-" + id, m[key]);
    const g = app.Guide || {};
    set("guide-intro", g.intro);
    set("guide-steps", (g.steps || []).join("\n"));
    set("guide-ios", (g.ios || []).join("\n"));
    set("guide-android", (g.android || []).join("\n"));
    set("guide-label", g.actionLabel);
    set("guide-note", g.note);
    document.querySelector(`input[name="app-behavior"][value="${app.Guide ? "guide" : "web"}"]`).checked = true;
    $("mobile-enabled").checked = !!app.Mobile;
    populateAudience(app.Access || ["everyone"]);
    $("app-advanced").open = editing;
    const kind = preferredType || (connections("ebooks").some(c => c.App === app.ID) ? "ebooks" : connections("requests").some(c => c.App === app.ID) ? "requests" : "web");
    document.querySelector(`input[name="application-type"][value="${kind}"]`).checked = true;
    loadBuilderConnections(app.ID);
    if (index === null && preferredType) applyTypeDefaults(kind);
    syncExperience();
    updateAppPreview();
    showBuilderStep(index === null && !preferredType ? -1 : 0);
    dirty = false;
    dirtyForms.delete("app-form");
    if (builderStep === -1) document.querySelector('input[name="application-type"]:checked').focus();
    else $("app-name").focus();
    $("app-form").scrollIntoView({ block: "start", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
  }
  async function editUser(user) {
    if (!await discard()) return;
    userDialogOpener = document.activeElement;
    editingUser = user || null;
    userPhotoEditor = null;
    $("user-photo").hidden = !user; $("user-photo-new").hidden = !!user;
    if (user) userPhotoEditor = window.anchorAvatar.editor($("user-photo"), {
      user: () => editingUser,
      endpoint: () => "/api/admin/users/" + encodeURIComponent(user.id) + "/photo",
      perform: work,
      saved(updated) {
        editingUser = { ...editingUser, picture: updated.picture || "", custom_picture: updated.custom_picture, photo_revision: updated.photo_revision };
        state.users = state.users.map(person => person.id === updated.id ? updated : person);
        $("user-list").replaceChildren(...state.users.map(personCard));
      },
    });
    $("user-form").reset();
    $("user-form-error").hidden = true;
    $("user-form-title").textContent = user ? "Edit user" : "Add user";
    set("user-name", user?.name);
    set("user-username", user?.username);
    set("user-tailscale", user?.tailscale);
    set("user-groups", (user?.groups || []).join(", "));
    $("user-enabled").checked = user?.enabled !== false;
    $("user-local").checked = user ? user.has_password : true;
    syncUserLocalSignIn();
    $("user-id").textContent = user
      ? "Stable user ID: " + user.id
      : "A stable ID is generated when you save.";
    $("user-password-help").textContent = user
      ? "Leave blank to keep the existing password. Changes to local credentials end password sessions; Tailscale sign-in stays active."
      : "Set a password to enable local sign-in. For Tailscale only, turn local sign-in off and link an email.";
    dirty = false;
    dirtyForms.clear();
    $("user-dialog").showModal();
    $("user-dialog-body").scrollTo(0, 0);
    $("user-name").focus();
    window.anchorPasswordWarnings?.refresh();
  }
  function syncUserLocalSignIn() {
    const enabled = $("user-local").checked;
    $("user-local-fields").hidden = !enabled;
    for (const id of ["user-username", "user-password"]) $(id).disabled = !enabled;
    $("user-username").required = enabled;
    $("user-password").required = enabled && !editingUser?.has_password;
    if (!enabled) set("user-password", "");
    window.anchorPasswordWarnings?.refresh();
  }
  $("user-local").addEventListener("change", syncUserLocalSignIn);
  const featureTypes = {
    requests: { root: "Requests", many: "RequestConnections", name: "Seerr", icon: "seerr" },
    ebooks: { root: "Ebooks", many: "EbookLibraries", name: "Ebooks", icon: "audiobookshelf" },
  };
  function connections(kind, cfg = state.config) {
    const type = featureTypes[kind];
    return [...(cfg[type.root] ? [cfg[type.root]] : []), ...(cfg[type.many] || [])];
  }
  function removeConnections(cfg, id) {
    for (const type of Object.values(featureTypes)) {
      if (cfg[type.root]?.App === id) cfg[type.root] = null;
      cfg[type.many] = (cfg[type.many] || []).filter(c => c.App !== id);
    }
  }
  function connectionProviders(kind, connection) {
    return kind === "requests" ? [
      ["Seerr", "seerr", connection.Seerr, true], ["Plex", "plex", connection.Plex, true],
      ["Sonarr", "sonarr", connection.Sonarr, false], ["Radarr", "radarr", connection.Radarr, false],
    ] : [["Audiobookshelf", "abs", connection.Server, true]];
  }
  function credentialField(hostID, inputID, clearID, name, help, saved) {
    const host = $(hostID); host.replaceChildren();
    const label = document.createElement("label"); label.className = "field";
    const caption = document.createElement("span"); caption.textContent = name;
    const input = document.createElement("input"); input.type = "password"; input.id = inputID; input.autocomplete = "new-password";
    input.placeholder = saved ? "Saved · leave blank to keep" : "Not configured";
    const hint = document.createElement("small"); hint.textContent = help; label.append(caption, input, hint);
    const clear = document.createElement("label"); clear.className = "check";
    const checkbox = document.createElement("input"); checkbox.type = "checkbox"; checkbox.id = clearID; checkbox.disabled = !saved;
    checkbox.addEventListener("change", () => { input.disabled = checkbox.checked; if (checkbox.checked) input.value = ""; });
    clear.append(checkbox, document.createTextNode("Remove saved credential")); host.append(label, clear);
  }
  function builderSecretName(kind, provider, id = v("app-id")) {
    return id && state.config[featureTypes[kind].root]?.App === id ? provider : `${kind}/${id}/${provider}`;
  }
  function loadBuilderConnections(id) {
    const r = connections("requests").find(c => c.App === id) || {}, e = connections("ebooks").find(c => c.App === id) || {};
    for (const [field, key] of [["seerr", "Seerr"], ["plex", "Plex"], ["sonarr", "Sonarr"], ["radarr", "Radarr"]]) set("builder-" + field, r[key]);
    set("builder-abs-server", e.Server); set("builder-abs-library", e.LibraryID);
    $("builder-calendar").open = true;
    for (const [provider, title] of [["seerr", "Seerr API key"], ["plex", "Plex token"], ["sonarr", "Sonarr API key"], ["radarr", "Radarr API key"], ["abs", "Audiobookshelf API key"]]) {
      const kind = provider === "abs" ? "ebooks" : "requests";
      credentialField("builder-key-" + provider, "builder-key-input-" + provider, "builder-clear-" + provider, title, "Used only by this connection. Blank keeps a saved credential.", state.keys[builderSecretName(kind, provider, id)]);
    }
  }
  function applyTypeDefaults(kind) {
    if (editingApp !== null) return;
    $("app-homescreen").checked = kind !== "ebooks";
    const defaults = featureTypes[kind];
    if (!v("app-name") || ["Seerr", "Ebooks"].includes(v("app-name"))) {
      set("app-name", defaults?.name || ""); set("app-id", newAppID(v("app-name")));
    }
    if (!appIconTouched) { set("app-icon", defaults?.icon || ""); updateIconPreview(); }
  }
  document.querySelectorAll('input[name="application-type"]').forEach(input => input.addEventListener("change", () => { applyTypeDefaults(applicationType()); syncExperience(); updateAppPreview(); }));
  function hasCombinedFeatures() {
    const id = v("app-id");
    return connections("requests").some(c => c.App === id) && connections("ebooks").some(c => c.App === id);
  }
  function builderKinds() {
    const kind = applicationType();
    return kind === "ebooks" && hasCombinedFeatures() ? ["requests", "ebooks"] : kind === "web" ? [] : [kind];
  }
  function syncBuilderConnections() {
    const kind = applicationType(), kinds = builderKinds();
    $("builder-existing-combination").hidden = !(kind === "ebooks" && hasCombinedFeatures());
    $("setup-title").textContent = kind === "requests" ? "Connect requests and upcoming releases" : kind === "ebooks" ? "Connect your ebook library" : "What happens when someone taps it?";
    for (const [type, panelID, fields] of [["requests", "builder-requests", ["builder-seerr", "builder-plex"]], ["ebooks", "builder-ebooks", ["builder-abs-server", "builder-abs-library"]]]) {
      const panel = $(panelID); panel.hidden = !kinds.includes(type);
      panel.querySelectorAll("input").forEach(field => {
        const provider = field.id.replace("builder-clear-", "");
        field.disabled = panel.hidden || field.id.startsWith("builder-clear-") && !state.keys[builderSecretName(type, provider)];
      });
      for (const id of fields) $(id).required = kinds.includes(type);
    }
    for (const provider of ["seerr", "plex", "sonarr", "radarr", "abs"]) {
      const type = provider === "abs" ? "ebooks" : "requests", clear = $("builder-clear-" + provider), field = $("builder-key-input-" + provider);
      field.disabled = !kinds.includes(type) || clear.checked;
      const used = provider === "abs" || provider === "seerr" || provider === "plex" || !!v("builder-" + provider);
      field.required = kinds.includes(type) && used && !state.keys[builderSecretName(type, provider)] && !clear.checked;
    }
  }
  function phoneOrigin(url) { try { return new URL(url).origin; } catch { return url; } }
  function saveBuilderConnections(cfg, id) {
    const kinds = builderKinds(), legacy = Object.fromEntries(Object.keys(featureTypes).map(kind => [kind, cfg[featureTypes[kind].root]?.App === id]));
    const keys = {};
    // Scoped credentials are pruned by the server. Legacy singular sources use
    // unqualified names, so clear only the removed source's own credentials.
    for (const kind of Object.keys(featureTypes)) if (legacy[kind] && !kinds.includes(kind)) {
      for (const provider of kind === "requests" ? ["seerr", "plex", "sonarr", "radarr"] : ["abs"]) keys[provider] = "";
    }
    removeConnections(cfg, id);
    for (const kind of kinds) {
      const connection = kind === "requests" ? { App: id, Seerr: v("builder-seerr"), Plex: v("builder-plex"), Sonarr: v("builder-sonarr"), Radarr: v("builder-radarr") } : { App: id, Server: v("builder-abs-server"), LibraryID: v("builder-abs-library") };
      if (legacy[kind]) cfg[featureTypes[kind].root] = connection;
      else cfg[featureTypes[kind].many].push(connection);
      for (const provider of kind === "requests" ? ["seerr", "plex", "sonarr", "radarr"] : ["abs"]) {
        const name = builderSecretName(kind, provider, id);
        if ($("builder-clear-" + provider).checked) keys[name] = "";
        else if ($("builder-key-input-" + provider).value) keys[name] = $("builder-key-input-" + provider).value;
      }
    }
    return keys;
  }
  function selectedBehavior() {
    return document.querySelector('input[name="app-behavior"]:checked')?.value || "web";
  }
  function applicationType() {
    return document.querySelector('input[name="application-type"]:checked')?.value || "web";
  }
  function syncExperience() {
    const kind = applicationType();
    $("tile-behavior").hidden = kind === "ebooks";
    $("tile-behavior").querySelectorAll('input[type="radio"]').forEach(input => input.disabled = kind === "ebooks");
    $("guide-fields").hidden = kind === "ebooks" || selectedBehavior() !== "guide";
    $("mobile-fields").hidden = !$("mobile-enabled").checked;
    for (const id of ["guide-fields", "mobile-fields"]) {
      const panel = $(id);
      panel.querySelectorAll("input, textarea").forEach(field => field.disabled = panel.hidden);
    }
    syncBuilderConnections();
  }
  function populateAudience(access) {
    const options = [["everyone", "Everyone with an Anchor account"], ["group:admin", "Administrators only"]];
    const groups = new Set(state.users.flatMap(u => u.groups));
    for (const group of groups) if (group.toLowerCase() !== "admin") options.push(["group:" + group, "Group: " + group]);
    for (const user of state.users) options.push(["user:" + user.id, user.name]);
    options.push(["custom", "Choose multiple groups or people…"]);
    $("app-audience").replaceChildren(...options.map(([value, name]) => {
      const o = document.createElement("option"); o.value = value; o.textContent = name; return o;
    }));
    const single = access.length === 1 ? access[0] : "";
    const selected = single === "everyone" ? "everyone" : options.find(([value]) => value === "group:" + single || value === "user:" + single)?.[0] || "custom";
    $("app-audience").value = selected;
    $("custom-access").hidden = selected !== "custom";
  }
  $("app-audience").addEventListener("change", () => {
    const audience = v("app-audience");
    $("custom-access").hidden = audience !== "custom";
    if (audience !== "custom") set("app-access", audience === "everyone" ? "everyone" : audience.slice(audience.indexOf(":") + 1));
    updateAppPreview();
  });
  function showBuilderStep(step) {
    builderStep = step;
    document.querySelectorAll("[data-app-step]").forEach(panel => panel.hidden = Number(panel.dataset.appStep) !== step);
    document.querySelectorAll("[data-step]").forEach(button => {
      if (editingApp !== null && Number(button.dataset.step) !== -1) {
        button.setAttribute("aria-selected", String(Number(button.dataset.step) === step)); button.tabIndex = Number(button.dataset.step) === step ? 0 : -1;
      }
      if (editingApp === null && Number(button.dataset.step) === step) button.setAttribute("aria-current", "step");
      else button.removeAttribute("aria-current");
    });
    $("builder-back").hidden = editingApp !== null || step === -1;
    $("builder-next").hidden = editingApp !== null || step === 3;
    $("app-save").hidden = editingApp === null && step !== 3;
    $("builder-next").textContent = step < 3 ? "Next: " + applicationStages[step + 2] : "Next";
    $("builder-back").textContent = step > -1 ? "Back: " + applicationStages[step] : "Back";
    $("builder-progress").hidden = editingApp !== null;
    $("builder-progress").textContent = editingApp !== null ? editingStages[step] : `Application step ${step + 2} of 5`;
    $("app-review").textContent = `${v("app-name") || "Your application"} will appear in ${v("app-section") || "Apps"}. ${applicationType() === "ebooks" ? "Tapping it opens your searchable ebook library in Anchor." : selectedBehavior() === "guide" ? "Tapping it opens your welcome guide." : "Tapping it opens the application."}${applicationType() === "requests" ? " A separate Request status button shows requests and upcoming releases." : ""}`;
    syncBuilderPreview();
    syncSetupActions();
  }
  function syncBuilderPreview() {
    const open = builderPreviewOpen && builderStep !== -1;
    $("builder-preview").hidden = !open;
    $("toggle-builder-preview").hidden = builderStep === -1;
    $("toggle-builder-preview").textContent = open ? "Hide preview" : "Show preview";
    $("toggle-builder-preview").setAttribute("aria-expanded", String(open));
    $("builder-layout").classList.toggle("has-preview", open);
  }
  $("toggle-builder-preview").addEventListener("click", () => {
    builderPreviewOpen = !builderPreviewOpen;
    syncBuilderPreview();
  });
  function validateBuilderStep(step) {
    const panel = document.querySelector(`[data-app-step="${step}"]`);
    for (const field of panel.querySelectorAll("input, select, textarea")) {
      if (!field.checkValidity()) {
        showBuilderStep(step);
        field.closest("details")?.setAttribute("open", "");
        field.reportValidity();
        return false;
      }
    }
    return true;
  }
  function moveBuilder(target) {
    if (editingApp === null && target > builderStep) {
      for (let step = -1; step < target; step++) if (!validateBuilderStep(step)) return;
    }
    if (target === 2) {
      const source = applicationType() === "requests" ? "builder-seerr" : applicationType() === "ebooks" ? "builder-abs-server" : null;
      if (source && !v(source)) set(source, phoneOrigin(v("app-url")));
      syncBuilderConnections();
    }
    showBuilderStep(target);
    if (editingApp === null) document.querySelector(`[data-app-step="${target}"] h3`)?.focus();
  }
  document.querySelectorAll("[data-app-step] h3").forEach(h => h.tabIndex = -1);
  document.querySelectorAll("[data-step]").forEach(button => button.addEventListener("click", () => moveBuilder(Number(button.dataset.step))));
  $("builder-back").addEventListener("click", () => moveBuilder(builderStep - 1));
  $("builder-next").addEventListener("click", () => moveBuilder(builderStep + 1));
  $("app-form").addEventListener("invalid", (event) => {
    const panel = event.target.closest("[data-app-step]");
    if (panel) showBuilderStep(Number(panel.dataset.appStep));
    event.target.closest("details")?.setAttribute("open", "");
  }, true);
  $("app-form").addEventListener("keydown", event => {
    if (editingApp === null && event.key === "Enter" && event.target.tagName === "INPUT" && builderStep < 3 && !["checkbox", "radio"].includes(event.target.type)) {
      event.preventDefault(); moveBuilder(builderStep + 1);
    }
  });
  function updateAppPreview() {
    const kind = applicationType(), guide = kind !== "ebooks" && selectedBehavior() === "guide", mobile = $("mobile-enabled").checked;
    $("preview-name").textContent = v("app-name") || "Your application";
    $("preview-description").textContent = v("app-description");
    $("preview-section").textContent = v("app-section") || "Apps";
    $("preview-letter").textContent = (v("app-name") || "A").charAt(0).toUpperCase();
    const icon = v("app-icon");
    const bundled = state.icons.find(i => i.name === icon);
    const image = $("preview-icon");
    const src = bundled ? "/api/admin/icons/" + encodeURIComponent(icon) : /^https?:\/\//i.test(icon) ? icon : icon ? "/icons/" + encodeURIComponent(icon) : "";
    image.hidden = !src;
    $("preview-letter").hidden = !!src;
    if (src) { if (image.getAttribute("src") !== src) image.src = src; } else image.removeAttribute("src");
    $("preview-requests-wrap").hidden = kind !== "requests" && !(kind === "ebooks" && hasCombinedFeatures());
    $("preview-mobile").hidden = !mobile;
    $("preview-home").hidden = !$("app-homescreen").checked || mobile;
    $("preview-behavior").textContent = kind === "ebooks" ? "Tap the tile to browse and download ebooks inside Anchor." : kind === "requests" ? "Try Request status below the tile to see recent requests and the release calendar." : guide ? "Tap the tile to see your welcome guide." : mobile && (v("mobile-iosapp") || v("mobile-androidapp")) ? "On a matching phone, the tile tries the installed app first. Other devices open the website." : "Tap the tile to open the application URL.";
    $("preview-audience").textContent = "Visible to: " + audienceText({ Access: split(v("app-access")) });
  }
  function phoneServer() {
    if (v("mobile-server")) return v("mobile-server");
    try { return new URL(v("app-url")).origin; } catch { return ""; }
  }
  $("preview-icon").addEventListener("error", () => { $("preview-icon").hidden = true; $("preview-letter").hidden = false; });
  $("app-form").addEventListener("input", () => { syncExperience(); updateAppPreview(); });
  $("app-form").addEventListener("change", () => { syncExperience(); updateAppPreview(); });
  function previewRequestStatus(content) {
    $("experience-title").textContent = "Request status";
    content.innerHTML = `<p class="muted small request-sample-note">Sample titles · this preview does not contact your servers.</p>
      <p class="muted small request-sample-updated" role="status">Updated just now</p>
      <div class="req-tabs" role="group" aria-label="What to show"><button class="req-tab" type="button" data-tab="requests" aria-pressed="true">Requests</button><button class="req-tab" type="button" data-tab="coming" aria-pressed="false">Coming up</button></div>
      <div class="search req-search"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></svg><input type="search" placeholder="Search your requests" aria-label="Search your requests" /></div>
      <ul class="req-list" tabindex="0" aria-label="Media titles"></ul>
      <p class="muted request-sample-nomatch" hidden>No titles match that.</p>
      <div class="req-foot"><button type="button" class="btn secondary">Refresh now</button></div>`;
    const view = window.anchorRequestView, list = content.querySelector(".req-list"), search = content.querySelector("input");
    const tabs = content.querySelector(".req-tabs"), updated = content.querySelector(".request-sample-updated");
    const hasComing = !!(v("builder-sonarr") || v("builder-radarr"));
    tabs.hidden = !hasComing;
    const date = days => new Date(Date.now() + days * 86400000).toISOString().slice(0, 10);
    const rows = [
      { title: "Example movie", kind: "movie", year: 2025, state: "ready" },
      { title: "Example show", kind: "show", year: 2026, state: "partial", ready: 5, downloading: true },
      { title: "A new adventure", kind: "movie", year: 2026, state: "downloading" },
      { title: "The next chapter", kind: "movie", year: 2026, state: "soon", date: date(14) },
      { title: "Shōgun", kind: "show", year: 2024, state: "partial", ready: 3, stuck: true },
      { title: "A fresh start", kind: "show", year: 2026, state: "soon", date: date(7) },
      { title: "Almost there", kind: "movie", year: 2026, state: "preparing" },
      { title: "A family request", kind: "movie", year: 2026, state: "pending" },
      { title: "Another request", kind: "show", year: 2026, state: "declined" },
      { title: "Still looking", kind: "movie", year: 2026, state: "notfound" },
      { title: "Waiting for confirmation", kind: "movie", year: 2026, state: "unconfirmed" },
    ];
    const coming = [
      { title: "Example show", kind: "show", year: 2026, date: date(1), note: "episode" },
      { title: "A fresh start", kind: "show", year: 2026, date: date(7), note: "premiere", season: 2 },
      { title: "Two at once", kind: "show", year: 2026, date: date(8), note: "episodes", count: 2 },
      { title: "Example movie", kind: "movie", year: 2026, date: date(14), note: "home" },
      { title: "The big screen", kind: "movie", year: 2026, date: date(21), note: "cinemas" },
      { title: "Later this season", kind: "show", year: 2026, date: date(35), note: "episode" },
    ].filter(row => row.kind === "show" ? v("builder-sonarr") : v("builder-radarr"));
    let tab = "requests";
    function filter() {
      const words = view.plain(search.value).split(/\s+/).filter(Boolean);
      let shown = 0;
      for (const row of list.children) {
        row.hidden = !words.every(word => row.dataset.search.includes(word));
        row.classList.toggle("req-first", !row.hidden && shown === 0);
        if (!row.hidden) shown++;
      }
      content.querySelector(".request-sample-nomatch").hidden = shown > 0;
    }
    function render() {
      list.replaceChildren(...(tab === "coming" ? coming.map(row => view.reqItem(row, view.comingWording(row), "info")) : rows.map(row => view.reqRow(row, false))));
      list.scrollTop = 0;
      search.parentElement.hidden = list.children.length < 6;
      for (const button of tabs.children) button.setAttribute("aria-pressed", String(button.dataset.tab === tab));
      search.placeholder = tab === "coming" ? "Search coming up" : "Search your requests";
      search.setAttribute("aria-label", search.placeholder);
      filter();
    }
    search.addEventListener("input", filter);
    for (const button of tabs.children) button.addEventListener("click", () => {
      if (tab === button.dataset.tab) return;
      tab = button.dataset.tab; search.value = ""; render();
    });
    content.querySelector(".req-foot button").addEventListener("click", () => {
      updated.textContent = "Updated just now · sample refreshed; no servers contacted.";
      render();
    });
    render();
  }
  function previewExperience(kind) {
    const content = $("experience-content"); content.replaceChildren();
    $("experience-preview").classList.toggle("req-sheet", kind === "requests");
    $("experience-preview").classList.toggle("request-preview", kind === "requests");
    const addText = text => { const p = document.createElement("p"); p.textContent = text; content.append(p); };
    const addSteps = (title, steps) => {
      if (!steps.length) return;
      const heading = document.createElement("h3"); heading.textContent = title;
      const list = document.createElement("ol"); list.className = "steps";
      for (const text of steps) { const li = document.createElement("li"); li.textContent = text; list.append(li); }
      content.append(heading, list);
    };
    $("experience-title").textContent = v("app-name") || "Your application";
    if (kind === "home") {
      addText("People get device-specific steps for adding a shortcut to their home screen. The shortcut opens this service through Anchor and respects their access.");
    } else if (kind === "mobile") {
      $("experience-title").textContent += " on your phone";
      const stores = document.createElement("div"); stores.className = "store-links";
      for (const [id, name] of [["mobile-ios", "App Store (iPhone)"], ["mobile-android", "Google Play (Android)"]]) {
        if (!v(id)) continue;
        const store = document.createElement("span"); store.className = "btn secondary"; store.textContent = name; stores.append(store);
      }
      if (stores.childElementCount) { addText("Install the app"); content.append(stores); }
      addText("When it asks for a server address, enter:");
      const server = document.createElement("code"); server.className = "preview-server";
      server.textContent = phoneServer() || "Your application server address"; content.append(server);
      if (v("mobile-note")) addText(v("mobile-note"));
      addText("Sign in with your username and password.");
      if ($("app-homescreen").checked) addText("Prefer a shortcut? Add to home screen is offered here too.");
    } else if (kind === "requests") {
      previewRequestStatus(content);
    } else if (applicationType() === "ebooks") {
      addText("Sample data · this preview does not contact your library or download files.");
      const searchLabel = document.createElement("label"); searchLabel.className = "field"; searchLabel.textContent = "Search books or authors";
      const search = document.createElement("input"); search.type = "search"; searchLabel.append(search);
      const list = document.createElement("ul"); list.className = "preview-sample-list";
      const samples = [["Example ebook", "Sample author"], ["Another story", "Another author"]];
      const render = () => {
        list.replaceChildren();
        for (const [name, author] of samples.filter(row => row.join(" ").toLowerCase().includes(search.value.toLowerCase()))) {
          const li = document.createElement("li"), title = document.createElement("strong"), note = document.createElement("p"), download = document.createElement("button");
          title.textContent = name; note.textContent = author + " · EPUB"; download.type = "button"; download.className = "secondary"; download.textContent = "Download sample";
          download.addEventListener("click", () => { download.textContent = "Preview only · no file downloaded"; });
          li.append(title, note, download); list.append(li);
        }
      };
      search.addEventListener("input", render); content.append(searchLabel, list); render();
    } else if (selectedBehavior() === "guide") {
      if (v("guide-intro")) addText(v("guide-intro"));
      addSteps("Getting started", lines(v("guide-steps")));
      addSteps("On iPhone", lines(v("guide-ios"))); addSteps("On Android", lines(v("guide-android")));
      if (v("guide-note")) addText(v("guide-note"));
      const action = document.createElement("span"); action.className = "btn"; action.textContent = v("guide-label") || "Open the application"; content.append(action);
      addText("Opens: " + v("app-url"));
    } else {
      addText("This tile opens: " + (v("app-url") || "Enter an application URL in App details."));
      if ($("mobile-enabled").checked && (v("mobile-iosapp") || v("mobile-androidapp"))) addText("On a matching phone, Anchor tries the native launch link first and falls back to its store page when configured.");
    }
    const dialog = $("experience-preview"); dialog.showModal(); dialog.querySelector("button").focus();
  }
  for (const [id, kind] of [["preview-open", "tile"], ["preview-mobile", "mobile"], ["preview-home", "home"], ["preview-requests", "requests"]]) $(id).addEventListener("click", () => previewExperience(kind));
  let previewTrigger;
  $("experience-preview").addEventListener("close", () => previewTrigger?.focus());
  $("experience-preview").addEventListener("click", event => { if (event.target === event.currentTarget) event.currentTarget.close(); });
  for (const id of ["preview-open", "preview-mobile", "preview-home", "preview-requests"]) $(id).addEventListener("click", event => previewTrigger = event.currentTarget);

  function renderSecurity() {
    const summary = connectionSummary(authStatus, location.origin);
    for (const key of ["mode", "address", "transport", "signin"]) {
      $("connection-"+key).textContent = summary[key];
      $("review-connection-"+key).textContent = summary[key];
    }
    $("review-connection-note").textContent = summary.note;
    $("security-connection").textContent = summary.note;
    $("connection-user").textContent = authStatus?.user?.name || "";
  }
  function connectionSummary(status, origin) {
    const modes = { lan: "HTTP LAN", proxy: "HTTPS reverse proxy", tailscale: "Tailscale Serve" };
    return {
      mode: modes[status?.mode] || "Unknown",
      address: origin,
      transport: origin.startsWith("https://") ? "HTTPS · encrypted" : "HTTP · unencrypted",
      signin: status?.method === "tailscale" ? "Tailscale identity" : "Local password",
      note: status?.mode === "lan" ? "HTTP LAN mode is enabled. After setup, use Tailscale Serve or an HTTPS reverse proxy for encrypted access."
        : status?.mode === "tailscale" ? "Tailscale mode is configured. Automatic sign-in is verified only when accessing Anchor through Serve."
        : "HTTPS reverse proxy mode is configured. Anchor uses local password sign-in.",
    };
  }
  function serveCommands(context, targetPort, httpsPort, container) {
    const port = (value) => /^\d+$/.test(String(value)) && Number(value) >= 1 && Number(value) <= 65535;
    if (!port(targetPort) || !port(httpsPort)) throw new Error("Enter whole port numbers between 1 and 65535.");
    if (!["host", "container", "dev"].includes(context)) throw new Error("Choose where Tailscale runs.");
    if (context === "dev" && Number(targetPort) < 1024) throw new Error("The development launcher requires a Anchor port of at least 1024.");
    if (context === "container" && !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/.test(container)) throw new Error("Enter a container name using letters, numbers, dots, underscores or hyphens.");
    const prefix = context === "container" ? `docker exec ${container} tailscale` : "tailscale";
    const target = Number(targetPort), https = Number(httpsPort);
    return {
      status: `${prefix} serve status`,
      setup: context === "dev" ? `.\\dev.ps1 -Port ${target} -Tailscale -TailscalePort ${https}` : `${prefix} serve --bg --https=${https} http://127.0.0.1:${target}`,
      environment: `ANCHOR_MODE=tailscale\nANCHOR_LISTEN=127.0.0.1:${target}`,
    };
  }
  const serveInstructions = {
    host: [
      "Install Tailscale on the server hosting Anchor and connect it to your tailnet. Tailscale runs on the server; it does not need to be installed in Anchor's image.",
      "Configure Anchor and the server's Tailscale to share localhost. For Docker, check the platform-specific networking notes below. Preserve /config, apply the Anchor environment values and restart Anchor. Update an old ANCHOR_PUBLIC_URL to your HTTPS Serve origin, or remove it for Tailscale mode.",
      "Run the commands below in your server's terminal. Open the HTTPS address printed by Serve from a Tailscale-connected device. Publishing Anchor at a LAN IP alone does not enable automatic identity.",
    ],
    container: [
      "Connect the Tailscale container to your tailnet and persist its state. Configure it to share Anchor's network environment so both services use the same localhost.",
      "Keep Anchor's /config persistent and apply the environment values below. Update or remove an old ANCHOR_PUBLIC_URL, then restart the deployment. Keep Anchor's HTTP port private. See the platform-specific notes for examples.",
      "Enter the name of the container running Tailscale. Run the generated docker exec commands on the Docker host. Both services must share localhost; joining an ordinary Docker bridge network is insufficient.",
    ],
    dev: [
      "Connect your development PC to Tailscale. This option uses the project's dev.ps1 launcher; open PowerShell in the Anchor repository and stop an existing launcher session with Ctrl+C.",
      "Run the setup command below. The launcher configures Anchor and a temporary Serve session, prints the HTTPS address, and removes its Serve session when you stop it. Your development database is preserved.",
    ],
  };
  function updateServeCommands() {
    const context = v("serve-context");
    $("serve-container-field").hidden = context !== "container";
    $("serve-container").required = context === "container";
    $("serve-container").disabled = context !== "container";
    $("serve-environment-block").hidden = context === "dev";
    document.querySelectorAll("[data-serve-platform]").forEach(note => { note.hidden = note.dataset.servePlatform !== context; });
    $("serve-instructions").replaceChildren(...(serveInstructions[context] || []).map(text => {
      const li = document.createElement("li"); li.textContent = text; return li;
    }));
    $("serve-run-location").textContent = context === "host" ? "Run in: your server's terminal. Check existing mappings before starting Serve."
      : context === "container" ? "Run in: terminal on the Docker host. docker exec runs Tailscale inside the named container."
      : "Run in: PowerShell in the Anchor repository. Use a second terminal to check Serve status while the launcher is running.";
    $("serve-copy-result").textContent = "";
    try {
      const commands = serveCommands(context, v("serve-target-port"), v("serve-https-port"), v("serve-container"));
      set("serve-command", commands.setup); set("serve-status-command", commands.status);
      $("serve-environment").textContent = commands.environment;
      $("serve-command-error").hidden = true;
      $("copy-serve-command").disabled = false; $("copy-serve-status").disabled = false;
    } catch (error) {
      set("serve-command", ""); set("serve-status-command", ""); $("serve-environment").textContent = "";
      $("serve-command-error").textContent = error.message; $("serve-command-error").hidden = false;
      $("copy-serve-command").disabled = true; $("copy-serve-status").disabled = true;
    }
  }
  $("tailscale-commands").addEventListener("submit", event => event.preventDefault());
  $("tailscale-commands").addEventListener("input", updateServeCommands);
  $("serve-context").addEventListener("change", () => {
    if (!$("serve-target-port").dataset.edited) set("serve-target-port", v("serve-context") === "dev" ? "18080" : "8080");
    updateServeCommands();
  });
  $("serve-target-port").addEventListener("input", () => { $("serve-target-port").dataset.edited = "true"; });
  for (const [buttonID, fieldID] of [["copy-serve-command", "serve-command"], ["copy-serve-status", "serve-status-command"]]) {
    $(buttonID).addEventListener("click", async () => {
      const text = $(fieldID).value;
      if (!text) return;
      try {
        if (!navigator.clipboard?.writeText) throw new Error("Clipboard unavailable");
        await navigator.clipboard.writeText(text);
        $("serve-copy-result").textContent = "Command copied. Run it in the terminal indicated above.";
      } catch {
        $(fieldID).focus(); $(fieldID).select();
        $("serve-copy-result").textContent = "Command selected. Copy it with Ctrl+C or your device's Copy action.";
      }
    });
  }
  function renderTailscale() {
    if (tailscaleTestRevision !== null && tailscaleTestRevision !== state.revision) {
      tailscaleTestGeneration++;
      tailscaleTestRevision = null;
      showTailscaleCheck("stale", { title: "Settings changed", detail: "Run the test again to check the latest accounts, groups and applications." });
      $("tailscale-preview-result").hidden = true;
    }
    const selected = v("tailscale-preview-person");
    $("tailscale-preview-person").replaceChildren(...state.users.filter(user => user.enabled).map(user => {
      const option = document.createElement("option"); option.value = user.id; option.textContent = user.name; return option;
    }));
    $("tailscale-preview-person").value = state.users.some(user => user.enabled && user.id === selected) ? selected : authStatus.user.id;
    const previous = v("tailscale-user");
    $("tailscale-user").replaceChildren(...state.users.map(user => {
      const option = document.createElement("option"); option.value = user.id; option.textContent = user.name; return option;
    }));
    $("tailscale-user").value = state.users.some(user => user.id === previous) ? previous : authStatus.user.id;
    $("use-tailscale-identity").hidden = !state.tailscale_identity;
    $("tailscale-identity-actions").hidden = !state.tailscale_identity;
    $("use-tailscale-identity").textContent = state.tailscale_identity ? "Use current Tailscale email: "+state.tailscale_identity : "Use my current Tailscale identity";
    renderTailscaleStatus(authStatus);
    if (!$("tailscale-dialog").open) editTailscaleIdentity();
    if (tailscaleGuideOpen === null) tailscaleGuideOpen = state.users.some(user => user.tailscale) || authStatus.method === "tailscale";
    setTailscaleGuide(tailscaleGuideOpen);
  }
  function setTailscaleGuide(open) {
    tailscaleGuideOpen = open;
    $("tailscale-guide").hidden = !open;
    $("start-tailscale").setAttribute("aria-expanded", String(open));
    $("start-tailscale").textContent = open ? "Hide setup steps" : state.users.some(user => user.tailscale) ? "Manage Tailscale" : "Set up Tailscale";
  }
  $("start-tailscale").addEventListener("click", () => setTailscaleGuide(!tailscaleGuideOpen));
  function tailscalePersonState(user, status = authStatus) {
    if (!user.enabled) return "Account paused · automatic sign-in unavailable";
    if (!user.tailscale) return "No Tailscale email linked";
    if (status?.method === "tailscale" && status.user?.id === user.id) {
      return status.simulated ? "Simulated sign-in on this device" : location.protocol === "https:"
        ? "Automatic sign-in verified on this device" : "Identity received over HTTP · HTTPS not verified";
    }
    return "Email linked · automatic sign-in not verified here";
  }
  function renderTailscalePeople(status = authStatus) {
    $("tailscale-people").replaceChildren(...state.users.map(user => {
      const card = document.createElement("div"); card.className = "tailscale-person";
      const text = document.createElement("div"), name = document.createElement("strong"); name.textContent = user.name;
      const email = document.createElement("p"); email.className = "help"; email.textContent = user.tailscale || "No linked email";
      const note = document.createElement("p"); note.className = "help"; note.textContent = tailscalePersonState(user, status);
      text.append(name, email, note);
      const edit = document.createElement("button"); edit.type = "button"; edit.className = "secondary";
      edit.textContent = user.tailscale ? "Edit link" : "Link email"; edit.dataset.tailscaleUser = user.id;
      edit.setAttribute("aria-label", `${edit.textContent} for ${user.name}`);
      edit.addEventListener("click", () => openTailscaleIdentity(user.id)); card.append(text, edit); return card;
    }));
  }
  async function openTailscaleIdentity(id) {
    clearTailscaleTest();
    if (!await discard()) return;
    dirty = false; dirtyForms.clear();
    tailscaleDialogOpener = document.activeElement;
    $("tailscale-user").value = id;
    $("tailscale-form-error").hidden = true;
    editTailscaleIdentity();
    $("tailscale-dialog").showModal();
    $("tailscale-dialog-body").scrollTo(0, 0);
    $("tailscale-email").focus();
  }
  function editTailscaleIdentity() {
    const user = state.users.find(user => user.id === v("tailscale-user"));
    if (!user) return;
    $("tailscale-user").dataset.selected = user.id;
    $("tailscale-save-status").textContent = "";
    $("tailscale-form-error").hidden = true;
    set("tailscale-email", user.tailscale);
    $("tailscale-account-state").textContent = `${user.enabled ? "Account enabled" : "Account disabled"} · Local recovery password ${user.has_password ? "set" : "not set"}`;
    $("tailscale-dialog-person-state").textContent = tailscalePersonState(user);
  }
  $("tailscale-user").addEventListener("change", async event => {
    const select = event.target, next = select.value;
    if (dirtyForms.has("tailscale-form")) {
      select.value = select.dataset.selected;
      if (!await window.anchorConfirm({ title: "Discard identity changes?", message: "Your unsaved Tailscale identity changes will be lost when you switch people.", confirmLabel: "Discard changes", cancelLabel: "Keep editing", danger: true })) return;
      select.value = next;
    }
    editTailscaleIdentity(); dirtyForms.delete("tailscale-form"); dirty = dirtyForms.size > 0;
  });
  $("tailscale-form").addEventListener("submit", event => {
    event.preventDefault();
    work(async () => {
      const user = state.users.find(user => user.id === v("tailscale-user"));
      await api("/api/admin/tailscale", { revision: state.revision, id: user.id, email: v("tailscale-email") });
      dirty = false; dirtyForms.clear();
      const status = await api("/api/auth/status");
      if (!status.authenticated) {
        $("tailscale-dialog").close();
        $("tailscale-save-status").textContent = "Identity saved. Your session has expired; sign in in another tab to continue. Your setup progress is kept.";
        renderTailscaleStatus(status); return;
      }
      window.anchorCSRF = status.csrf; authStatus = status;
      window.anchorRefreshSessionControls?.(status);
      await refresh();
      $("tailscale-dialog").close();
      $("tailscale-save-status").textContent = "Tailscale identity saved. " + (wizardActive ? "Continue setup with your current login. " : "") + "Open the Serve HTTPS address to test automatic sign-in.";
    });
  });
  function tailscaleReadiness(status, users, origin = "", observed = "") {
    if (!status?.authenticated) return { state: "expired", title: "Sign-in needs attention", text: "Your session has expired.", next: "Sign in in another tab and return here. Your saved settings and setup progress are kept." };
    if (status.simulated) return { state: "simulated", title: "Development simulation", text: "This browser is using a simulated Tailscale identity.", next: "Use a real Tailscale-connected device and Serve HTTPS address to verify private access and automatic sign-in." };
    if (status.method === "tailscale") return origin.startsWith("https://")
      ? { state: "verified", title: "Automatic sign-in verified", text: `Automatic sign-in verified for ${status.user?.name || "your account"} on this device.`, next: "You can use this address on devices with the required Tailscale access. Each person must test their own linked account; access from outside your home has not been checked here." }
      : { state: "insecure", title: "HTTPS still needs checking", text: "Anchor received a linked Tailscale identity, but this browser is using HTTP.", next: "Open the HTTPS address printed by Tailscale Serve and check again." };
    const linked = users.filter(user => user.enabled && user.tailscale).length;
    if (status.mode !== "tailscale") return { state: "mode", title: "Anchor is using password sign-in", text: `Automatic Tailscale sign-in is not enabled in this deployment. ${linked} enabled ${linked === 1 ? "person has" : "people have"} a saved email link.`, next: "Complete Step 1: Connect Anchor to Tailscale, or keep using your current sign-in and return later." };
    if (observed) return { state: "unlinked", title: "This device's identity needs linking", text: "Tailscale supplied an identity, but this session is using an Anchor password.", next: "Link the detected email in Step 2: Link people to their Anchor accounts, then reopen the HTTPS Serve address. Check paused accounts and email spelling if a link already exists." };
    return linked ? { state: "linked", title: "Emails linked; sign-in not verified", text: `${linked} enabled ${linked === 1 ? "person has" : "people have"} a saved Tailscale email. This browser is using an Anchor password.`, next: "Open Anchor's HTTPS address printed by Serve from a Tailscale-connected device, then check this device's sign-in." }
      : { state: "pending", title: "Choose how to connect", text: "No enabled Anchor accounts have a linked Tailscale email. This browser is using an Anchor password.", next: "Complete Step 1: Connect Anchor and Step 2: Link people, or continue with your current sign-in and return to Administration → Tailscale later." };
  }
  function renderTailscaleStatus(status) {
    const result = tailscaleReadiness(status, state.users, location.origin, state.tailscale_identity);
    $("tailscale-verification").textContent = result.text;
    $("tailscale-verification").dataset.state = result.state;
    $("tailscale-next").textContent = result.next;
    $("review-tailscale-status").textContent = result.text + " " + result.next;
    const facts = $("tailscale-facts"); facts.replaceChildren();
    for (const [label, value] of [
      ["Anchor deployment", connectionSummary(status, location.origin).mode],
      ["This browser", location.protocol === "https:" ? "HTTPS · encrypted" : "HTTP · unencrypted"],
      ["Signed in with", !status?.authenticated ? "Session expired" : status.simulated ? "Simulated Tailscale identity" : status.method === "tailscale" ? "Tailscale" : "Anchor password"],
      ["Linked people", `${state.users.filter(user => user.enabled && user.tailscale).length} enabled accounts`],
    ]) addFact(facts, label, value);
    if (state.tailscale_identity) addFact(facts, "Observed on this device", state.tailscale_identity);
    $("tailscale-connect-status").textContent = status?.method === "tailscale" && !status.simulated
      ? "A linked Tailscale identity reached Anchor on this request. " + (location.protocol === "https:" ? "This browser is using HTTPS." : "Open the Serve HTTPS address to finish checking.")
      : "Anchor cannot confirm your server's Tailscale installation or network access from this browser. Use the instructions below if the connection is not already configured.";
    if (!v("tailscale-address") && location.protocol === "https:" && location.hostname.endsWith(".ts.net")) set("tailscale-address", location.origin);
    renderTailscalePeople(status);
    return result;
  }
  function showTailscaleCheck(state, result) {
    const copy = {
      checking: ["Checking sign-in…", "Waiting for the Tailscale test tab to report the recognized account and applications."],
      error: ["Couldn't complete the sign-in test", "Check that this device is connected to Tailscale, then open Anchor using its full HTTPS Tailscale Serve address. Check Step 1 if it still cannot be reached."],
      unsaved: ["Save your changes first", "Save or cancel your changes, then test the saved setup."],
      timeout: ["The Tailscale test hasn't returned a result", "The address may be unreachable. Check that this device is connected to Tailscale, that the address and port are correct, and that Serve is running. Then start a new test. A missing result does not prove Tailscale is disconnected."],
    };
    const report = result ? { ...result, state } : { state, title: copy[state][0], detail: copy[state][1] };
    window.anchorTailscaleTest.render($("tailscale-check-result"), report, report.address || "");
  }
  function clearTailscaleTest() {
    tailscaleTestGeneration++;
    tailscaleTestRevision = null;
    $("tailscale-check-result").hidden = true;
  }
  $("tailscale-form").addEventListener("input", clearTailscaleTest);
  $("tailscale-address").addEventListener("input", clearTailscaleTest);
  $("verify-tailscale").addEventListener("click", async () => {
    if (dirty) { showTailscaleCheck("unsaved"); return; }
    const generation = ++tailscaleTestGeneration;
    tailscaleTestRevision = state.revision;
    showTailscaleCheck("checking");
    try {
      const result = await window.anchorTailscaleTest.read();
      if (generation === tailscaleTestGeneration) showTailscaleCheck(result.state, result);
    } catch (error) {
      if (generation === tailscaleTestGeneration) showTailscaleCheck("error", { title: "Couldn't complete the sign-in test", detail: error instanceof TypeError || error.name === "AbortError" ? "Couldn't reach Anchor. Check that this device is connected to Tailscale and open Anchor's full HTTPS Tailscale Serve address, then try again." : error.message });
    }
  });
  $("open-tailscale-test").addEventListener("click", async () => {
    if (dirty) { showTailscaleCheck("unsaved"); return; }
    let address;
    try {
      address = new URL(v("tailscale-address"));
      if (address.protocol !== "https:" || !address.hostname.endsWith(".ts.net") || address.username || address.password || address.search || address.hash || !["", "/"].includes(address.pathname)) throw new Error();
    } catch {
      showTailscaleCheck("error", { title: "Enter Anchor's Tailscale HTTPS address", detail: "Copy the full HTTPS address printed by Tailscale Serve, including its port if shown. For example: https://anchor.example.ts.net:8443." });
      $("tailscale-address").focus(); return;
    }
    // Open synchronously to avoid popup blocking after the API call. Remove
    // the opener before navigating; results travel through Anchor's API.
    const tab = window.open("about:blank", "_blank");
    if (!tab) { showTailscaleCheck("error", { title: "Allow the test tab to open", detail: "Your browser blocked the test tab. Allow popups for Anchor and try again." }); return; }
    tab.opener = null;
    tab.document.body.textContent = "Preparing Anchor's sign-in test…";
    const generation = ++tailscaleTestGeneration;
    tailscaleTestRevision = state.revision;
    showTailscaleCheck("checking");
    try {
      const test = await api("/api/admin/tailscale-test", { address: address.origin });
      if (generation !== tailscaleTestGeneration) { tab.close(); return; }
      tab.location.replace(test.url);
      for (let attempt = 0; attempt < 25; attempt++) {
        await new Promise(resolve => setTimeout(resolve, 1200));
        if (generation !== tailscaleTestGeneration) return;
        const result = await api(`/api/admin/tailscale-test?ticket=${encodeURIComponent(test.ticket)}`);
        if (generation !== tailscaleTestGeneration) return;
        if (result.report) { showTailscaleCheck(result.report.state, result.report); return; }
      }
      showTailscaleCheck("timeout");
    } catch (error) {
      if (generation === tailscaleTestGeneration) showTailscaleCheck("error", { title: "Couldn't complete the sign-in test", detail: error.message });
    }
  });
  for (const [button, section] of [["tailscale-fix-connection", "tailscale-connect-step"], ["tailscale-fix-people", "tailscale-people-step"]]) {
    $(button).addEventListener("click", () => {
      setTailscaleGuide(true);
      if (button === "tailscale-fix-connection") $("serve-deployment").open = true;
      const heading = $(section).querySelector("h2"); heading.tabIndex = -1; heading.focus(); heading.scrollIntoView({ block: "start", behavior: "smooth" });
    });
  }
  $("tailscale-preview-person").addEventListener("change", () => { $("tailscale-preview-result").hidden = true; });
  $("tailscale-preview").addEventListener("click", () => work(async () => {
    if (dirty) { showTailscaleCheck("unsaved"); return; }
    const personId = v("tailscale-preview-person");
    await refresh();
    const person = state.users.find(user => user.enabled && user.id === personId);
    if (!person) throw new Error("This account changed. Choose an enabled person and preview again.");
    const preview = await api(`/api/me?as=${encodeURIComponent(person.id)}`);
    if (preview.login !== person.id && !preview.viewingAs) throw new Error("This account changed. Reload settings and preview again.");
    window.anchorTailscaleTest.render($("tailscale-preview-result"), { state: "preview", title: "Configured access preview", detail: "Based on saved Anchor permissions. This does not verify this person's Tailscale connection or sign-in.", name: person.name, groups: person.groups, sections: preview.sections });
  }));
  $("use-tailscale-identity").addEventListener("click", () => {
    if (!state.tailscale_identity) return;
    set("tailscale-email", state.tailscale_identity);
    dirty = true; dirtyForms.add("tailscale-form");
    $("tailscale-dialog-person-state").textContent = "Current Tailscale email filled in. Save to link it to the selected person. Check that this is their own Tailscale account.";
  });
  $("copy-tailscale-address").addEventListener("click", async () => {
    if (!v("tailscale-address")) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("Clipboard unavailable");
      await navigator.clipboard.writeText(v("tailscale-address"));
      $("tailscale-address-result").textContent = "Address copied. For automatic sign-in, connect the receiving device to Tailscale and link that person's Tailscale email to an enabled Anchor account.";
    } catch {
      $("tailscale-address").focus(); $("tailscale-address").select();
      $("tailscale-address-result").textContent = "Address selected. Copy it with Ctrl+C or your device's Copy action.";
    }
  });
  function renderDelivery() {
    for (const dest of ["ntfy", "webhook"]) {
      const status = state.delivery?.[dest];
      $("delivery-"+dest).textContent = status ? `${status.state === "sent" ? "Accepted" : "Failed"}: ${status.message} (${new Date(status.at).toLocaleString()})` : "No delivery attempted since Anchor started.";
      $("test-"+dest).disabled = dest === "ntfy" ? !state.config.Notify : !state.config.Webhook;
    }
  }
  function syncWebhook() {
    $("key-webhook").required = $("webhook-enabled").checked && (!state?.keys.webhook || $("clear-webhook").checked);
  }
  function wizardText(text, tag = "p") {
    const p = document.createElement(tag); p.textContent = text; $("wizard-guide").append(p); return p;
  }
  function setWizardActive(active) {
    active = !!(active && state?.setup && !state.setup.completed);
    wizardActive = active;
    $("setup-wizard").hidden = !active; $("wizard-body").hidden = !active;
    $("open-setup").hidden = active || !state?.setup || state.setup.completed;
    document.body.classList.toggle("in-setup", active);
    $("wizard-skip-top").hidden = !active || wizardSteps[state.setup.step][1] !== "tailscale";
    $("tailscale-later-help").hidden = !active;
    syncSetupActions();
  }
  function applicationEditorActive() {
    return !$("app-form").hidden && applicationsView === "manage" && !document.querySelector('[data-view="apps"]').hidden;
  }
  function measureSetupActions() {
    document.documentElement.style.setProperty("--setup-action-height", $("setup-actions").hidden ? "0px" : Math.ceil($("setup-actions").getBoundingClientRect().height) + "px");
    document.documentElement.style.setProperty("--setup-navigation-height", $("setup-wizard").hidden ? "0px" : Math.ceil($("setup-wizard").getBoundingClientRect().height) + "px");
  }
  function syncSetupActions() {
    const editing = applicationEditorActive();
    $("setup-actions").hidden = !editing && !wizardActive;
    $("builder-actions").hidden = !editing;
    $("wizard-footer").hidden = editing || !wizardActive;
    $("setup-actions").setAttribute("aria-label", editing ? "Application editor actions" : "Anchor setup actions");
    $("setup-action-title").textContent = editing ? "Application editor" : "Anchor setup";
    $("setup-action-detail").textContent = editing
      ? editingApp !== null ? `${editingStages[builderStep]} · ${v("app-name") || "Application"}` : `${applicationStages[builderStep + 1]} · Application step ${builderStep + 2} of 5`
      : wizardActive ? `${wizardSteps[state.setup.step][0]} · Setup step ${state.setup.step + 1} of ${wizardSteps.length}` : "";
    $("setup-wizard").dataset.paused = String(editing);
    $("wizard-steps").querySelectorAll("button").forEach(button => { button.disabled = busy || editing; });
    document.body.classList.toggle("has-setup-actions", editing || wizardActive);
    measureSetupActions();
  }
  if (window.ResizeObserver) {
    const actionLayout = new window.ResizeObserver(measureSetupActions);
    actionLayout.observe($("setup-actions")); actionLayout.observe($("setup-wizard"));
  }
  window.addEventListener("resize", measureSetupActions);
  function renderWizard() {
    const step = state.setup.step, [title, panel, intro] = wizardSteps[step];
    setWizardActive(wizardActive);
    if (!wizardActive) return;
    $("wizard-count").textContent = `Anchor setup · ${title}${panel === "tailscale" ? " (optional)" : ""} · ${step+1}\u00a0of\u00a0${wizardSteps.length}`;
    $("wizard-title").textContent = title; $("wizard-intro").textContent = intro;
    $("wizard-progress").value = step+1;
    $("wizard-progress-fill").style.width = `${100 * step / (wizardSteps.length-1)}%`;
    $("wizard-back").disabled = step === 0;
    $("wizard-skip").hidden = panel === "review";
    $("wizard-next").textContent = panel === "review" ? "Finish Anchor setup" : "Next: " + wizardSteps[step + 1][0];
    $("wizard-back").textContent = step > 0 ? "Back: " + wizardSteps[step - 1][0] : "Previous setup step";
    $("wizard-steps").replaceChildren(...wizardSteps.map(([label], i) => {
      const li = document.createElement("li");
      if (i < step) li.className = "is-complete";
      const button = document.createElement("button"); button.type = "button";
      button.setAttribute("aria-label", `${label}${wizardSteps[i][1] === "tailscale" ? " (optional)" : ""}, step ${i+1} of ${wizardSteps.length}`);
      button.disabled = busy || applicationEditorActive();
      if (i === step) button.setAttribute("aria-current", "step");
      const dot = document.createElement("span"); dot.className = "wizard-dot"; dot.setAttribute("aria-hidden", "true"); dot.textContent = i < step ? "✓" : String(i+1);
      const caption = document.createElement("span"); caption.className = "wizard-step-label"; caption.textContent = label;
      if (wizardSteps[i][1] === "tailscale") { const optional = document.createElement("span"); optional.className = "wizard-optional"; optional.textContent = "(optional)"; caption.append(optional); }
      button.append(dot, caption); button.addEventListener("click", () => moveWizard(i)); li.append(button); return li;
    }));
    if (panel !== "review") return;
    $("wizard-guide").replaceChildren();
    const own = state.users.find(user => user.id === authStatus.user.id);
    const facts = document.createElement("dl"); facts.className = "connection-summary";
    for (const [label, value] of [
      ["Administrator", own?.name || authStatus.user.name],
      ["People", String(state.users.filter(user => user.enabled).length)],
      ["Applications", String(state.config.Apps.length)],
      ["Notifications", [state.config.Notify && "ntfy", state.config.Webhook && state.config.Webhook.Format].filter(Boolean).join(" and ") || "Set up later"],
    ]) addFact(facts, label, value);
    $("wizard-guide").append(facts);
    wizardText(`Set up later: ${(state.setup.skipped || []).map(i => wizardSteps[i][0]).join(", ") || "none"}.`);
    renderTailscaleStatus(authStatus);
  }
  function focusWizardBody() {
    $("wizard-title").focus({ preventScroll: true });
    $("wizard-title").scrollIntoView({ block: "start" });
  }
  $("wizard-backups").addEventListener("click", async () => {
    if (await showPanel("transfer")) { setWizardActive(false); message("Download a private database backup below. Reopen Setup wizard to finish."); }
  });
  async function moveWizard(step, skip=false, complete=false) {
    if (!state?.setup || state.setup.completed) { setWizardActive(false); return; }
    if (applicationEditorActive()) { message("Save or cancel this application before continuing Anchor setup.", true); return; }
    if (dirty) { message("Save or cancel your form changes before continuing setup.",true); return; }
    await work(async()=>{
      const skipped=new Set(state.setup.skipped || []);
      if (skip) skipped.add(state.setup.step); else if (step>state.setup.step || complete) skipped.delete(state.setup.step);
      state.setup=await api("/api/admin/setup", {...state.setup,step,completed:complete,skipped:[...skipped]});
      setWizardActive(!complete);
      if (complete) { await showPanel("apps"); message("Setup finished. You can change these settings in Administration at any time."); }
      else { await showPanel(wizardSteps[step][1]); renderWizard(); focusWizardBody(); }
    });
  }
  $("wizard-back").addEventListener("click",()=>moveWizard(Math.max(0,state.setup.step-1)));
  $("wizard-next").addEventListener("click",()=>moveWizard(Math.min(wizardSteps.length-1,state.setup.step+1),false,state.setup.step===wizardSteps.length-1));
  $("wizard-skip").addEventListener("click",()=>moveWizard(Math.min(wizardSteps.length-1,state.setup.step+1),true));
  $("wizard-skip-top").addEventListener("click", () => {
    if (wizardActive && wizardSteps[state.setup.step][1] === "tailscale") moveWizard(state.setup.step + 1, true);
  });
  $("wizard-pause").addEventListener("click",()=>{setWizardActive(false); message("Setup paused. Saved settings and progress are kept. Reopen Setup wizard to continue.");});
  $("open-setup").addEventListener("click",async ()=>{
    if (!state?.setup || state.setup.completed) return;
    if (dirty) { message("Save or cancel your changes before opening setup.",true); return; }
    setWizardActive(true); await showPanel(wizardSteps[state.setup.step][1]); renderWizard(); focusWizardBody();
  });
  document
    .querySelectorAll("[data-panel]")
    .forEach((b) =>
      b.addEventListener("click", async () => { if (await showPanel(b.dataset.panel)) { setWizardActive(false); } }),
    );
  const approvedSubmissions = new WeakSet(), pendingSubmissions = new WeakSet();
  document.querySelectorAll("form").forEach((f) => {
    // Native dialog forms dismiss a preview; they do not save configuration
    // or discard the surrounding editor's draft.
    if (f.method === "dialog" || ["tailscale-commands", "order-move-form"].includes(f.id)) return;
    f.addEventListener("input", (event) => {
      if (event.target.id === "tailscale-user" || f.id === "tailscale-commands" || event.target.closest("[data-photo-editor]")) return;
      dirty = true;
      dirtyForms.add(f.id);
    });
    f.addEventListener(
      "submit",
      (e) => {
        if (approvedSubmissions.delete(f)) return;
        const others = [...dirtyForms].filter(id => id !== f.id && !(notificationForms.includes(f.id) && notificationForms.includes(id)));
        if (!others.length) return;
        e.preventDefault();
        e.stopImmediatePropagation();
        if (pendingSubmissions.has(f)) return;
        const submitter = e.submitter;
        pendingSubmissions.add(f);
        window.anchorConfirm({ title: "Save and discard other changes?", message: "Saving this form will discard unsaved changes in other forms.", confirmLabel: "Save and discard", cancelLabel: "Keep editing", danger: true }).then(confirmed => {
          if (!confirmed || !f.isConnected) return;
          approvedSubmissions.add(f);
          f.requestSubmit(submitter || undefined);
          approvedSubmissions.delete(f);
        }).finally(() => pendingSubmissions.delete(f));
      },
      true,
    );
  });
  // Browser tab close/reload cannot show an application dialog. Keep draft
  // confirmation on navigation initiated through Anchor instead.
  document.addEventListener("click", async event => {
    const link = event.target.closest?.("a[href]");
    if (!dirty || event.defaultPrevented || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || !link || link.target === "_blank" || link.hasAttribute("download")) return;
    const destination = new URL(link.href, location.href);
    const sameDocument = destination.origin === location.origin && destination.pathname === location.pathname && destination.search === location.search;
    if (!["http:", "https:"].includes(destination.protocol) || sameDocument && link.getAttribute("href").includes("#")) return;
    event.preventDefault();
    if (await window.anchorConfirm({ title: "Leave this page?", message: "Your unsaved changes will be lost if you leave this page.", confirmLabel: "Leave page", cancelLabel: "Keep editing", danger: true })) {
      dirty = false; dirtyForms.clear();
      location.assign(destination.href);
    }
  });
  $("add-app").addEventListener("click", () => editApp(null));
  for (const name of ["manage", "arrange"]) {
    const tab = $("applications-"+name+"-tab");
    tab.addEventListener("click", () => setApplicationsView(name));
    tab.addEventListener("keydown", async event => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const next = event.key === "Home" ? "manage" : event.key === "End" ? "arrange" : name === "manage" ? "arrange" : "manage";
      if (await setApplicationsView(next)) $("applications-"+next+"-tab").focus();
    });
  }
  wireTabs("notification-tabs", tab => selectTab("notification-tabs", tab.id));
  wireTabs("connection-tabs", tab => selectTab("connection-tabs", tab.id));
  document.querySelectorAll("[data-step]").forEach(button => button.addEventListener("keydown", event => {
    if (editingApp === null || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const step = Number(button.dataset.step), next = event.key === "Home" ? 0 : event.key === "End" ? 3 : (step + (event.key === "ArrowLeft" ? -1 : 1) + 4) % 4;
    showBuilderStep(next); document.querySelector(`[data-step="${next}"]`).focus();
  }));
  $("add-user").addEventListener("click", () => editUser(null));
  $("cancel-app").addEventListener("click", async () => {
    if (await discard()) {
      $("app-form").hidden = true;
      $("app-list").hidden = false;
      $("add-app").hidden = false;
      dirty = false;
      dirtyForms.clear();
      syncSetupActions();
      $("add-app").focus();
    }
  });
  async function cancelUser() {
    if (!busy && await discard()) $("user-dialog").close();
  }
  $("cancel-user").addEventListener("click", cancelUser);
  $("close-user").addEventListener("click", cancelUser);
  $("user-dialog").addEventListener("cancel", event => {
    event.preventDefault();
    cancelUser();
  });
  function wireEditorKeyboard(dialog) { dialog.addEventListener("keydown", event => {
    if (event.key !== "Tab") return;
    const controls = [...dialog.querySelectorAll("button, input, select, textarea, [tabindex]")]
      .filter(control => !control.disabled && control.tabIndex >= 0 && control.getClientRects().length);
    if (!controls.length) return;
    const index = controls.indexOf(document.activeElement);
    event.preventDefault();
    const next = index < 0 ? (event.shiftKey ? controls.length - 1 : 0)
      : (index + (event.shiftKey ? -1 : 1) + controls.length) % controls.length;
    controls[next].focus();
    controls[next].scrollIntoView({ block: "nearest", behavior: "auto" });
  }); }
  wireEditorKeyboard($("user-dialog"));
  wireEditorKeyboard($("tailscale-dialog"));
  async function cancelTailscaleIdentity() {
    if (!busy && await discard()) $("tailscale-dialog").close();
  }
  $("cancel-tailscale").addEventListener("click", cancelTailscaleIdentity);
  $("close-tailscale").addEventListener("click", cancelTailscaleIdentity);
  $("tailscale-dialog").addEventListener("cancel", event => { event.preventDefault(); cancelTailscaleIdentity(); });
  $("tailscale-dialog").addEventListener("close", () => {
    const id = v("tailscale-user");
    $("tailscale-form").reset(); $("tailscale-form-error").hidden = true;
    dirtyForms.delete("tailscale-form"); dirty = dirtyForms.size > 0;
    const opener = tailscaleDialogOpener?.isConnected ? tailscaleDialogOpener
      : [...$("tailscale-people").querySelectorAll("button")].find(button => button.dataset.tailscaleUser === id) || $("start-tailscale");
    opener.focus(); tailscaleDialogOpener = null;
  });
  $("user-dialog").addEventListener("close", () => {
    const userID = editingUser?.id;
    $("user-form").reset();
    $("user-form-error").hidden = true;
    editingUser = null; userPhotoEditor = null;
    dirtyForms.delete("user-form");
    dirty = dirtyForms.size > 0;
    const opener = userDialogOpener?.isConnected ? userDialogOpener
      : [...$("user-list").querySelectorAll("[data-user-id]")].find(button => button.dataset.userId === userID) || $("add-user");
    opener.focus();
    userDialogOpener = null;
  });
  $("app-id").addEventListener("input", () => (appIdTouched = true));
  $("icon-search").addEventListener("input", renderIconChoices);
  $("app-icon").addEventListener("input", () => {
    appIconTouched = true;
    updateIconPreview();
  });
  function newAppID(name) {
    const base = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 60) || "app";
    let id = base, n = 2;
    while (state.config.Apps.some(app => app.ID === id)) { const suffix = "-" + n++; id = base.slice(0, 64 - suffix.length) + suffix; }
    return id;
  }
  $("app-name").addEventListener("input", () => {
    if (editingApp === null && !appIconTouched) {
      const match = matchingIcon(v("app-name"));
      set("app-icon", match?.name || featureTypes[applicationType()]?.icon || "");
      updateIconPreview();
    }
    if (editingApp === null && !appIdTouched) {
      set(
        "app-id",
        newAppID(v("app-name")),
      );
    }
  });
  $("app-form").addEventListener("submit", (e) => {
    e.preventDefault();
    if (editingApp === null && builderStep !== 3) { moveBuilder(builderStep + 1); return; }
    for (let step = editingApp === null ? -1 : 0; step <= 3; step++) if (!validateBuilderStep(step)) return;
    work(async () => {
      if (editingApp !== null) {
        const id = state.config.Apps[editingApp].ID, kinds = builderKinds();
        const removed = Object.keys(featureTypes).filter(kind => !kinds.includes(kind) && connections(kind).some(connection => connection.App === id));
        if (removed.length && !await window.anchorConfirm({ title: "Remove application connections?", message: "Changing this application type will remove its " + removed.map(kind => kind === "requests" ? "Seerr" : "Ebook").join(" and ") + " connections and their saved credentials.", confirmLabel: "Save and remove connections", cancelLabel: "Keep editing", danger: true })) return;
      }
      const config = clone(state.config),
        app = {
          Name: v("app-name"),
          ID: v("app-id"),
          URL: v("app-url"),
          Description: v("app-description"),
          Section: v("app-section") || "Apps",
          Icon: v("app-icon"),
          Access: split(v("app-access")),
          Check: v("app-check"),
          Maintenance: v("app-maintenance"),
          Disabled: $("app-disabled").checked,
          Alerts: $("app-alerts").checked,
          HomeScreen: $("app-homescreen").checked,
        };
      const m = {
        ios: v("mobile-ios"),
        android: v("mobile-android"),
        iosApp: v("mobile-iosapp"),
        androidApp: v("mobile-androidapp"),
        openLabel: v("mobile-label"),
        server: v("mobile-server"),
        note: v("mobile-note"),
      };
      if ($("mobile-enabled").checked && !m.server) m.server = phoneServer();
      app.Mobile = $("mobile-enabled").checked ? m : null;
      const g = {
        intro: v("guide-intro"),
        steps: lines(v("guide-steps")),
        ios: lines(v("guide-ios")),
        android: lines(v("guide-android")),
        actionLabel: v("guide-label"),
        note: v("guide-note"),
      };
      app.Guide = applicationType() !== "ebooks" && selectedBehavior() === "guide" ? g : null;
      if (editingApp === null) config.Apps.push(app);
      else config.Apps[editingApp] = app;
      const keys = saveBuilderConnections(config, app.ID);
      await save(config, "Application saved.", keys);
      $("add-app").focus();
    });
  });
  $("user-form").addEventListener("submit", (e) => {
    e.preventDefault();
    if (userPhotoEditor?.dirty()) { message("Choose Upload photo to save your selected photo, or clear the selection before saving account details.", true); return; }
    work(async () => {
      await api("/api/admin/users", {
        revision: state.revision,
        user: {
          id: editingUser?.id || "",
          name: v("user-name"),
          username: $("user-local").checked ? v("user-username") : editingUser?.username || "",
          tailscale: v("user-tailscale"),
          groups: split(v("user-groups")),
          enabled: $("user-enabled").checked,
        },
        password: $("user-local").checked ? $("user-password").value : "",
        disable_local: !$("user-local").checked,
      });
      dirty = false;
      const status = await api("/api/auth/status");
      window.anchorCSRF = status.csrf;
      if (!status.authenticated) {
        location.replace("/account.html");
        return;
      }
      authStatus = status;
      window.anchorRefreshSessionControls?.(status);
      await refresh();
      message("User saved.");
    });
  });
  function keyChanges(names) {
    const values = {};
    for (const key of names) {
      if ($("clear-" + key).checked) values[key] = "";
      else if ($("key-" + key).value) values[key] = $("key-" + key).value;
    }
    return values;
  }
  $("notify-enabled").addEventListener("change", () => {
    for (const id of ["notify-server", "notify-topic"]) $(id).required = $("notify-enabled").checked;
  });
  $("notifications-form").addEventListener("submit", (e) => {
    e.preventDefault();
    work(async () => {
      const c = clone(state.config);
      c.Notify = $("notify-enabled").checked ? {
        Server: v("notify-server"), Topic: v("notify-topic"),
        Alerts: $("notify-alerts").checked, Reports: $("notify-reports").checked,
      } : null;
      await saveNotification("ntfy", c, keyChanges(["ntfy"]));
    });
  });
  $("webhook-enabled").addEventListener("change",syncWebhook);
  $("webhook-form").addEventListener("submit",event=>{
    event.preventDefault(); work(async()=>{
      const c=clone(state.config);
      c.Webhook=$("webhook-enabled").checked ? {Format:v("webhook-format"),Alerts:$("webhook-alerts").checked,Reports:$("webhook-reports").checked} : null;
      await saveNotification("webhook", c, keyChanges(["webhook"]));
    });
  });
  for (const destination of ["ntfy","webhook"]) $("test-"+destination).addEventListener("click",()=>work(async()=>{
    if (dirtyForms.has(destination === "ntfy" ? "notifications-form" : "webhook-form")) throw new Error("Save this client’s changes before sending a test notification.");
    try { const result=await api("/api/admin/notifications/test",{destination}); message(result.message); }
    finally { const latest=await api("/api/admin/state"); state.delivery=latest.delivery; renderDelivery(); }
  }));
  $("settings-form").addEventListener("submit", (e) => {
    e.preventDefault();
    work(async () => {
      const c = clone(state.config);
      c.Title = v("settings-title");
      c.Subtitle = v("settings-subtitle");
      c.Contact = v("settings-contact");
      c.CheckInterval = v("settings-interval");
      c.Sections = lines(v("settings-sections"));
      c.Notice = v("notice-message")
        ? {
            Message: v("notice-message"),
            Style: v("notice-style"),
            Access: split(v("notice-access")),
          }
        : null;
      await save(c, "Dashboard settings saved.");
    });
  });
  $("download-backup").addEventListener("click", () =>
    work(async () => {
      const response = await fetch("/api/admin/backup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: "{}",
      });
      if (!response.ok) {
        const result = await response.json();
        throw new Error(result.message || "Backup failed.");
      }
      const url = URL.createObjectURL(await response.blob());
      const a = document.createElement("a");
      a.href = url;
      a.download = "anchor-backup.db";
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      message("Database backup downloaded. Keep it private.");
    }),
  );
  $("import-file").addEventListener("change", async () => {
    try {
      const file = $("import-file").files[0];
      if (file) {
        if (file.size > 1024 * 1024)
          throw new Error("Choose a YAML file smaller than 1 MiB.");
        $("import-yaml").value = await file.text();
        $("import-preview").hidden = true;
        imported = null;
        dirty = true;
      }
    } catch (e) {
      message(e.message, true);
    }
  });
  $("import-yaml").addEventListener("input", () => {
    imported = null;
    $("import-preview").hidden = true;
  });
  $("import-form").addEventListener("submit", (e) => {
    e.preventDefault();
    work(async () => {
      const preview = await api("/api/admin/import/preview", {
        yaml: $("import-yaml").value,
      });
      imported = preview.config;
      importRevision = state.revision;
      const before = new Map(state.config.Apps.map((a) => [a.ID, a])),
        after = new Map((imported.Apps || []).map((a) => [a.ID, a]));
      const changes = [
        `Title: ${imported.Title}`,
        `Applications: ${before.size} -> ${after.size}`,
        `Sections: ${(imported.Sections || []).join(", ")}`,
      ];
      for (const [id, a] of after) {
        if (!before.has(id)) changes.push("Add: " + a.Name);
        else if (JSON.stringify(before.get(id)) !== JSON.stringify(a))
          changes.push("Update: " + a.Name);
      }
      for (const [id, a] of before)
        if (!after.has(id)) changes.push("Remove: " + a.Name);
      $("import-summary").textContent = changes.join("\n");
      $("import-preview").hidden = false;
      message("YAML validated. Review the replacement before applying.");
    });
  });
  $("apply-import").addEventListener("click", async () => {
    if (!imported) return;
    if (!await window.anchorConfirm({ title: "Replace dashboard settings?", message: "Replace dashboard settings with this validated import? Accounts and keys are preserved.", confirmLabel: "Apply import", cancelLabel: "Cancel", danger: true })) return;
    await work(async () => {
      await api("/api/admin/config", {
        revision: importRevision,
        config: imported,
      });
      await refresh();
      message("Imported dashboard applied.");
      imported = null;
      $("import-preview").hidden = true;
      dirty = false;
    });
  });
  window.anchorBeforeSignOut = () => !busy && discard();
  window.anchorReady
    .then(async (status) => {
      if (!status || !status.authenticated) {
        location.replace("/account.html");
        return;
      }
      if (!status.admin) {
        const heading = document.createElement("h1");
        heading.textContent = "Administration";
        $("loading").replaceChildren(heading, document.createTextNode("Administrator access is required."));
        return;
      }
      authStatus = status;
      updateServeCommands();
      await refresh();
      setWizardActive(!state.setup.completed);
      if (wizardActive) { await showPanel(wizardSteps[state.setup.step][1]); renderWizard(); }
      else { await showPanel("apps"); }
    })
    .catch((e) => message(e.message, true));
})();
