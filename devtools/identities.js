(() => {
  "use strict";
  const form = document.getElementById("identity-form"), select = document.getElementById("identity"), button = form.querySelector("button"), message = document.getElementById("identity-message");
  let state;
  function describe() {
    const user = state.people.find(user => user.id === select.value);
    document.getElementById("identity-details").textContent = user
      ? `${user.enabled ? "Enabled" : "Disabled — sign-in should be rejected"} · Groups: ${user.groups.join(", ") || "none"} · ${user.tailscale || "No Tailscale email linked — sign-in should be rejected"}`
      : select.value === "unlinked" ? "An invented Tailscale identity with no linked Anchor account. Sign-in should be rejected."
      : "No identity or previous local login is sent. Anchor should request sign-in.";
  }
  select.addEventListener("change", describe);
  form.addEventListener("submit", async event => {
    event.preventDefault(); button.disabled = true; message.textContent = "";
    try {
      const response = await fetch("/__dev/identity", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ID: select.value, CSRF: state.csrf }), signal: AbortSignal.timeout(10000) });
      const result = await response.json();
      if (!response.ok) throw new Error(result.message || "Could not select this identity.");
      location.assign("/");
    } catch (error) { message.textContent = error.message; button.disabled = false; }
  });
  (async () => {
    try {
      const response = await fetch("/__dev/identity-state", { cache: "no-store", signal: AbortSignal.timeout(10000) });
      if (!response.ok) throw new Error("Could not load Anchor people. Reload to try again.");
      state = await response.json(); select.replaceChildren();
      for (const user of [...state.people, { id: "unlinked", name: "Unlinked Tailscale identity" }, { id: "signed-out", name: "Signed out" }]) {
        const option = document.createElement("option"); option.value = user.id; option.textContent = user.name;
        select.append(option);
      }
      select.value = state.selected;
      if (!select.value) select.value = "signed-out";
      select.disabled = button.disabled = false; describe();
    } catch (error) { message.textContent = error.message; }
  })();
})();
