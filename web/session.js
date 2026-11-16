(() => {
  "use strict";
  function connectionNotice(status) {
    let dismissed = false;
    try { dismissed = localStorage.getItem("anchor.httpNoticeDismissed") === "1"; } catch { /* storage may be unavailable */ }
    let note = document.getElementById("http-note");
    if (!status.insecure || dismissed) {
      if (note) note.hidden = true;
      return;
    }
    if (!note) {
      note = document.createElement("aside");
      note.id = "http-note";
      note.className = "http-note";
      note.setAttribute("aria-label", "Connection security");
      document.body.prepend(note);
    }
    const text = document.createElement("span");
    text.textContent = "HTTP LAN mode: this connection is unencrypted. Use HTTPS or Tailscale on networks you do not trust.";
    const close = document.createElement("button");
    close.type = "button";
    close.className = "notice-close";
    close.setAttribute("aria-label", "Dismiss connection notice");
    close.textContent = "×";
    close.addEventListener("click", () => {
      note.hidden = true;
      try { localStorage.setItem("anchor.httpNoticeDismissed", "1"); } catch { /* dismissal still applies to this page */ }
    });
    note.replaceChildren(text, close);
    note.hidden = false;
  }
  const originalFetch = window.fetch.bind(window);
  window.fetch = (input, options = {}) => {
    const url = new URL(
      typeof input === "string" || input instanceof URL
        ? String(input)
        : input.url,
      location.href,
    );
    const method = (options.method || input.method || "GET").toUpperCase();
    if (
      url.origin === location.origin &&
      !["GET", "HEAD", "OPTIONS"].includes(method)
    ) {
      const headers = new Headers(options.headers || input.headers || {});
      if (window.anchorCSRF) headers.set("X-CSRF-Token", window.anchorCSRF);
      options = { ...options, headers };
    }
    return originalFetch(input, options);
  };
  window.anchorReady = (async () => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 20_000);
    let status;
    try {
      const response = await fetch("/api/auth/status", { cache: "no-store", signal: controller.signal });
      if (!response.ok) return null;
      status = await response.json();
      if (response.headers?.get("X-Anchor-Dev-Identities") === "1") {
        status.simulated = true;
        const note = document.createElement("aside"); note.className = "dev-identity-note";
        note.setAttribute("aria-label", "Development identity simulation");
        const text = document.createElement("span"); text.textContent = "Simulated Tailscale identity: " + (status.authenticated ? status.user.name : "signed out or unlinked") + ".";
        const link = document.createElement("a"); link.href = "/__dev/identities"; link.textContent = "Change test user";
        note.append(text, link); document.body.prepend(note);
      }
    } finally {
      clearTimeout(timer);
    }
    window.anchorCSRF = status.csrf;
    window.anchorMode = status.mode;
    window.anchorAuthMethod = status.method;
    connectionNotice(status);
    if (document.body.dataset.page === "dashboard") {
      if (status.setup_required || !status.authenticated) {
        location.replace("/account.html");
        return status;
      }
      const nav = document.createElement("nav");
      nav.className = "auth-nav";
      nav.setAttribute("aria-label", "Account");
      if (status.admin) {
        const link = document.createElement("a");
        link.href = "/manage.html";
        link.textContent = "Administration";
        nav.append(link);
      }
      const account = document.createElement("a");
      account.href = "/account.html";
      account.textContent = "My account";
      account.hidden = !!status.admin;
      nav.append(account);
      document.querySelector(".top")?.insertBefore(nav, document.getElementById("who"));
    }
    if (status.authenticated) {
      const nav = document.querySelector(".auth-nav");
      if (nav) {
        const logout = document.createElement("button");
        logout.id = "sign-out"; logout.type = "button"; logout.className = "secondary";
        logout.textContent = "Sign out"; logout.hidden = status.method !== "local";
        logout.addEventListener("click", async () => {
          if (logout.disabled || await window.anchorBeforeSignOut?.() === false) return;
          logout.disabled = true;
          const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 20_000);
          try {
            const response = await fetch("/api/auth/logout", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}", signal: controller.signal });
            const result = await response.json();
            if (!response.ok) throw new Error(result.message || "Could not sign out. Try again.");
            location.replace("/account.html");
          } catch (error) {
            let message = document.getElementById("message");
            if (!message) { message = document.createElement("p"); message.id = "message"; message.setAttribute("role", "alert"); document.body.prepend(message); }
            message.className = "message error"; message.hidden = false;
            message.textContent = controller.signal.aborted ? "Anchor did not respond in time. Try signing out again." : error.message;
          } finally { clearTimeout(timer); logout.disabled = false; }
        });
        nav.append(logout);
        window.anchorRefreshSessionControls = (latest) => {
          window.anchorAuthMethod = latest.method;
          logout.hidden = !latest.authenticated || latest.method !== "local";
        };
        window.anchorRefreshSessionControls(status);
      }
    }
    return status;
  })().catch(() => null);
})();
