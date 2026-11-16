(() => {
  "use strict";
  const maxBytes = 2 * 1024 * 1024;
  function pictureSource(value) {
    if (!value) return "";
    try {
      const url = new URL(value, location.origin);
      if (url.username || url.password) return "";
      if (url.origin === location.origin && value.startsWith("/api/people/") && /^\/api\/people\/[^/]+\/photo$/.test(url.pathname) && /^[a-f0-9]{64}$/.test(url.searchParams.get("v") || "")) return url.pathname + url.search;
      if (url.origin !== location.origin && /^https:\/\//.test(value) && url.protocol === "https:") return url.href;
    } catch {}
    return "";
  }
  function render(host, user, imageID) {
    const name = user?.name || user?.login || "";
    const picture = pictureSource(user?.picture);
    const key = JSON.stringify([name, picture]);
    if (host.dataset.avatarKey === key) return;
    host.dataset.avatarKey = key;
    host.classList.add("profile-avatar");
    host.setAttribute("role", "img"); host.setAttribute("aria-label", name ? "Profile image for " + name : "Profile image");
    host.title = name;
    host.textContent = "";
    const initials = document.createElement("span");
    initials.textContent = name.trim().split(/\s+/).slice(0, 2).map(word => Array.from(word)[0] || "").join("").toUpperCase() || "?";
    initials.setAttribute("aria-hidden", "true");
    const image = document.createElement("img");
    if (imageID) image.id = imageID;
    image.alt = ""; image.referrerPolicy = "no-referrer"; image.decoding = "async";
    image.hidden = !picture; initials.hidden = !!picture;
    image.addEventListener("error", () => { image.hidden = true; initials.hidden = false; });
    host.append(initials, image);
    if (picture) image.src = picture;
  }
  function editor(host, options) {
    host.textContent = ""; host.dataset.photoEditor = "true"; host.classList.add("photo-editor");
    const preview = options.preview || document.createElement("div"); preview.classList.add("photo-editor-preview");
    if (options.preview) host.classList.add("photo-editor-single-preview");
    const controls = document.createElement("div"); controls.className = "photo-editor-controls";
    const title = document.createElement("strong"); title.textContent = "Profile photo";
    const help = document.createElement("p"); help.className = "help"; help.textContent = "PNG or JPEG, up to 2 MB. Photos are cropped to a square and save separately from account details. Use default to restore your Tailscale photo or initials.";
    const label = document.createElement("label"); label.className = "field"; label.append(document.createTextNode("Choose photo"));
    const input = document.createElement("input"); input.type = "file"; input.accept = "image/png,image/jpeg,.png,.jpg,.jpeg"; label.append(input);
    const actions = document.createElement("div"); actions.className = "photo-editor-actions";
    const upload = document.createElement("button"), reset = document.createElement("button");
    for (const button of [upload, reset]) { button.type = "button"; button.className = "secondary"; }
    upload.textContent = "Upload photo"; reset.textContent = "Use default photo";
    const status = document.createElement("p"); status.className = "help"; status.setAttribute("role", "status");
    actions.append(upload, reset); controls.append(title, help, label, actions, status);
    if (!options.preview) host.append(preview);
    host.append(controls);
    let file = null, busy = false;
    function sync() {
      const user = options.user();
      render(preview, user);
      upload.disabled = busy || !file; reset.disabled = busy || !user.custom_picture; input.disabled = busy;
    }
    function feedback(text, error = false) { status.textContent = text; status.classList.toggle("photo-error", error); status.setAttribute("role", error ? "alert" : "status"); }
    input.addEventListener("change", () => {
      file = input.files?.[0] || null;
      if (file && (file.size > maxBytes || (file.type && !["image/png", "image/jpeg"].includes(file.type)))) { file = null; input.value = ""; feedback("Choose a PNG or JPEG photo up to 2 MB.", true); }
      else feedback(file ? file.name + " selected. Choose Upload photo to save it." : "");
      sync();
    });
    const perform = options.perform || (async action => action());
    async function change(remove) {
      if (busy || (!remove && !file)) return;
      if (remove && !await window.anchorConfirm({ title: "Use default profile photo?", message: "Remove the uploaded photo and use the Tailscale photo or initials instead?", confirmLabel: "Use default photo", cancelLabel: "Keep photo" })) return;
      const selected = file, user = options.user();
      let saved = false;
      await perform(async () => {
        busy = true; sync(); feedback("Saving profile photo…");
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), 20_000);
        try {
          const body = { revision: user.photo_revision || 0 };
          if (!remove) body.image = await new Promise((resolve, reject) => {
            const reader = new FileReader();
            const abort = () => { reader.abort(); reject(new Error("The photo could not be read.")); };
            controller.signal.addEventListener("abort", abort, { once: true });
            reader.onload = () => { controller.signal.removeEventListener("abort", abort); resolve(String(reader.result).split(",")[1]); };
            reader.onerror = () => { controller.signal.removeEventListener("abort", abort); reject(new Error("The photo could not be read.")); };
            reader.readAsDataURL(selected);
          });
          const response = await fetch(options.endpoint(), { method: remove ? "DELETE" : "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), signal: controller.signal });
          const result = await response.json();
          if (!response.ok) throw new Error(result.message || "The photo could not be saved.");
          await options.saved(result.user);
          file = null; input.value = "";
          saved = true;
          feedback(remove ? "Default profile photo restored." : "Profile photo saved.");
        } catch (error) { feedback(controller.signal.aborted ? "Anchor did not respond in time. Try again." : error.message, true); }
        finally { clearTimeout(timer); busy = false; }
      });
      sync();
      if (saved) input.focus();
    }
    upload.addEventListener("click", () => change(false)); reset.addEventListener("click", () => change(true));
    sync();
    return { dirty: () => !!file, busy: () => busy };
  }
  window.anchorAvatar = { render, editor };
})();
