(() => {
  "use strict";
  const target = document.getElementById("test-result"), button = document.getElementById("test-again");
  const ticket = new URLSearchParams(location.hash.slice(1)).get("ticket") || "";
  // Keep the temporary run identifier out of copied links and browser history.
  history.replaceState(null, "", location.pathname);
  let submitted = false;
  async function test() {
    button.disabled = true;
    window.anchorTailscaleTest.render(target, { state: "checking", title: "Checking sign-in…", detail: "Checking this device's Tailscale identity and Anchor permissions." });
    try {
      const report = await window.anchorTailscaleTest.read(submitted ? "" : ticket);
      submitted = true;
      window.anchorTailscaleTest.render(target, report, location.origin);
    } catch (error) {
      window.anchorTailscaleTest.render(target, { state: "error", title: "Couldn't complete the sign-in test", detail: error instanceof TypeError || error.name === "AbortError" ? "Couldn't reach Anchor. Check that this device is connected to Tailscale and open Anchor using its full HTTPS Tailscale Serve address, then try again." : error.message });
    } finally { button.disabled = false; }
  }
  button.addEventListener("click", test);
  document.getElementById("copy-test-link").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(new URL("/tailscale-test.html", location.origin).href);
      document.getElementById("test-link-status").textContent = "Test link copied. Each person should open it from their own Tailscale-connected device.";
    } catch { document.getElementById("test-link-status").textContent = "Copy this page's address from your browser to share the test."; }
  });
  test();
})();
