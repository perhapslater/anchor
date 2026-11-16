(() => {
  "use strict";
  function render(target, report, dashboardOrigin = "") {
    target.replaceChildren();
    target.hidden = false;
    target.dataset.state = report.state;
    const headingLevel = target.id === "test-result" ? 2 : 3;
    const heading = document.createElement(`h${headingLevel}`);
    heading.textContent = report.state === "verified" && report.name ? `Automatic sign-in verified for ${report.name}` : report.title;
    const detail = document.createElement("p"); detail.textContent = report.detail;
    target.append(heading, detail);
    const facts = document.createElement("dl"); facts.className = "connection-summary";
    for (const [label, value] of [
      ["Tailscale identity", report.identity], ["Anchor account", report.name],
      ["Groups", report.name ? (report.groups || []).join(", ") || "No groups assigned" : ""],
      ["Address tested", report.address],
      ["Checked", report.checked_at ? new Date(report.checked_at).toLocaleString() : ""],
    ]) {
      if (!value) continue;
      const row = document.createElement("div"), term = document.createElement("dt"), definition = document.createElement("dd");
      term.textContent = label; definition.textContent = value; row.append(term, definition); facts.append(row);
    }
    if (facts.children.length) target.append(facts);
    if (report.state === "verified" || report.state === "preview") {
      const apps = (report.sections || []).flatMap(section => (section.apps || []).map(app => ({ ...app, section: section.name })));
      const title = document.createElement(`h${headingLevel + 1}`); title.textContent = `Available in Anchor (${apps.length})`; target.append(title);
      if (apps.length) {
        const list = document.createElement("ul"); list.className = "tailscale-test-apps";
        for (const app of apps) {
          const item = document.createElement("li"), name = document.createElement("strong"), status = document.createElement("span");
          name.textContent = app.name;
          status.textContent = `${app.section} · ${app.maintenance ? "Maintenance" : app.locked ? "Currently unavailable" : app.status === "up" ? "Responding to Anchor's server check" : "Reachability not confirmed"}`;
          item.append(name, status); list.append(item);
        }
        target.append(list);
      } else {
        const empty = document.createElement("p"); empty.textContent = "No applications are available to this account. Check the application's access rules and this person's groups."; target.append(empty);
      }
      const note = document.createElement("p"); note.className = "help";
      note.textContent = "This list checks Anchor's permissions. Server health checks do not verify access from this device or sign you into applications.";
      target.append(note);
    }
    if (report.state === "verified" && dashboardOrigin) {
      const link = document.createElement("a"); link.href = new URL("/", dashboardOrigin).href;
      link.target = "_blank"; link.rel = "noopener noreferrer"; link.textContent = "Open this account's dashboard"; target.append(link);
    }
  }
  async function read(ticket = "") {
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 15000);
    try {
      // A local password cookie must not make this test pass.
      const query = ticket && location.protocol === "https:" ? `?ticket=${encodeURIComponent(ticket)}` : "";
      const response = await fetch(`/api/tailscale-test${query}`, { cache: "no-store", credentials: "omit", signal: controller.signal });
      const report = await response.json();
      if (!response.ok) throw new Error(report.message || "Anchor couldn't complete the sign-in test.");
      report.address = location.origin;
      if (location.protocol !== "https:" && report.state === "verified") {
        report.state = "insecure"; report.title = "Open Anchor's HTTPS Tailscale address";
        report.detail = "A linked Tailscale identity was received, but this browser is using HTTP. Open the full HTTPS address printed by Serve and test again.";
        delete report.sections;
      }
      return report;
    } finally { clearTimeout(timer); }
  }
  window.anchorTailscaleTest = { render, read };
})();
