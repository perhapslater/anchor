(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  let status, dirty = false, photoEditor = null;
  function message(text, error = false) {
    $("message").hidden = false;
    $("message").textContent = text;
    $("message").classList.toggle("error", error);
  }
  async function send(path, body) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 20_000);
    try {
      const r = await fetch(path, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      const result = await r.json();
      if (!r.ok) throw new Error(result.message || "Could not save.");
      return result;
    } catch (e) {
      if (controller.signal.aborted)
        throw new Error("Anchor did not respond in time. Please try again.");
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }
  async function submit(form, action) {
    const button = form.querySelector("button[type=submit]");
    button.disabled = true;
    try {
      await action();
    } catch (e) {
      message(e.message, true);
    } finally {
      button.disabled = false;
    }
  }
  $("setup").addEventListener("submit", (e) => {
    e.preventDefault();
    submit(e.currentTarget, async () => {
      await send("/api/auth/setup", {
        Token: $("setup-token").value.trim(),
        Name: $("setup-name").value,
        Username: $("setup-username").value,
        Password: $("setup-password").value,
      });
      location.replace("/manage.html");
    });
  });
  $("login").addEventListener("submit", (e) => {
    e.preventDefault();
    submit(e.currentTarget, async () => {
      await send("/api/auth/login", {
        Username: $("username").value,
        Password: $("password").value,
      });
      location.replace("/");
    });
  });
  $("change-password").addEventListener("input", () => { dirty = true; });
  window.anchorBeforeSignOut = () => !photoEditor?.busy() && (!(dirty || photoEditor?.dirty()) || window.anchorConfirm({ title: "Discard unsaved changes?", message: "Your unsaved account changes will be lost.", confirmLabel: "Discard changes", cancelLabel: "Keep editing", danger: true }));
  $("change-password").addEventListener("submit", (e) => {
    e.preventDefault();
    submit(e.currentTarget, async () => {
      const result = await send("/api/auth/account", {
        Username: $("local-username").value.trim(),
        Current: $("current-password").value,
        Password: $("new-password").value,
      });
      dirty = false;
      if (result.reauthenticate) { location.replace("/account.html"); return; }
      $("current-password").value = $("new-password").value = "";
      window.anchorPasswordWarnings?.refresh();
      message(result.message);
    });
  });
  window.anchorReady
    .then((current) => {
      status = current;
      if (!status)
        throw new Error("Could not connect to Anchor. Reload to try again.");
      if (status.setup_required) {
        $("heading").textContent = "Welcome aboard.";
        $("intro").textContent =
          "Create the first administrator to start adding your applications.";
        $("setup").hidden = false;
      } else if (!status.authenticated) {
        $("heading").textContent = "Welcome back.";
        $("intro").textContent =
          status.mode === "tailscale" ? "Sign in with your Anchor password, or use automatic sign-in at your administrator's Tailscale HTTPS address." : "Sign in with your Anchor login and password.";
        $("login").hidden = false;
      } else {
        $("heading").textContent = "Your account";
        $("profile").hidden = false;
        $("profile-name").textContent = status.user.name;
        window.anchorAvatar.render($("account-avatar"), status.user);
        $("account-photo").hidden = status.admin;
        if (!status.admin) photoEditor = window.anchorAvatar.editor($("account-photo"), {
          preview: $("account-avatar"),
          user: () => status.user, endpoint: () => "/api/auth/photo",
          saved(user) { status.user = user; window.anchorAvatar.render($("account-avatar"), user); },
        });
        $("profile-method").textContent =
          status.simulated ? "Signed in with a simulated Tailscale identity for local testing."
            : status.method === "tailscale" ? "Signed in automatically through Tailscale as " + status.user.name + "." : "Signed in with your Anchor password.";
        $("profile-tailscale").hidden = status.method !== "tailscale";
        $("profile-tailscale").textContent = status.simulated
          ? "This simulation does not verify a real Tailscale connection."
          : "Anchor recognizes the Tailscale account connected on this device. Your Anchor groups determine which applications you can see. To use another person's account, change the connected Tailscale account or device. Ending a local session does not disconnect Tailscale.";
        $("admin-controls").hidden = !status.admin;
        $("local-credentials").hidden = status.admin || !status.user.has_password;
        $("local-disabled").hidden = status.admin || status.user.has_password;
        $("local-username").value = status.user.username;
        $("current-password-field").hidden = status.method === "tailscale";
        $("current-password").required = status.method !== "tailscale";
        $("current-password").disabled = status.method === "tailscale";
        $("credential-help").textContent = status.method === "tailscale"
          ? "Your Tailscale identity verifies this change. Leave the new password blank to keep it. Your Tailscale sign-in stays active."
          : "Enter your current password to save changes. Leave the new password blank to keep it. Changing local credentials ends password sessions.";
      }
    })
    .catch((e) => {
      $("heading").textContent = "Connection unavailable";
      message(e.message, true);
    });
})();
