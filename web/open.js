// Anchor's home-screen pages: the guide for adding an app to the home screen,
// and the page a shortcut shows when its app isn't working. Plain JavaScript.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const ua = navigator.userAgent;
  const isIOS = /iPad|iPhone|iPod/.test(ua) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  const isAndroid = /Android/.test(ua);
  const isSamsung = /SamsungBrowser/.test(ua);
  // Safari's version is iOS's version: Safari 26 is iOS 26.
  const safari = Number((ua.match(/Version\/(\d+)/) || [])[1]) || 0;
  const installed = matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;

  // ---------- The steps for the phone in hand ----------
  function showSteps() {
    const phone = isIOS ? "iphone" : isAndroid ? (isSamsung ? "samsung" : "chrome") : "";
    if (!phone) return;
    $("steps-generic").hidden = true;
    $("steps-" + phone).hidden = false;
    // iOS 26 opens every shortcut as a web app unless the person says no.
    $("ios26-step").hidden = !(isIOS && safari >= 26);
  }

  // ---------- Inside the installed Anchor app ----------
  // A phone can't add a shortcut from inside an installed web app; it has to be
  // done from a browser. Getting there differs by phone.
  //
  // iPhone: an x-safari- link opens Safari. Apple doesn't document it and it
  // doesn't work on every iOS, so it's a try: if Anchor is still on screen a
  // moment later, the steps below are shown.
  //
  // Android: there's nothing to try. The installed app owns every Anchor
  // address, so one that isn't typed into a browser (a link, the "open"
  // suggestion on selected text, an intent) opens the app again instead of a
  // browser. The address has to be pasted into the browser's address bar.
  const guideAddress = () => `${location.origin}${location.pathname}?add=1`;

  function showManual(afterTry) {
    $("browser-url").textContent = guideAddress();
    $("manual-lead").hidden = !afterTry;
    $("browser-manual").hidden = false;
  }

  async function copyAddress() {
    const btn = $("copy-address");
    try {
      await navigator.clipboard.writeText(guideAddress());
    } catch {
      // Older browsers: select the address so it can be copied by hand.
      const r = document.createRange();
      r.selectNodeContents($("browser-url"));
      const s = getSelection();
      s.removeAllRanges();
      s.addRange(r);
      document.execCommand && document.execCommand("copy");
    }
    btn.textContent = "Copied!";
    btn.classList.add("copied");
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("copied"); }, 2000);
  }

  function openInSafari() {
    let left = false;
    const leave = () => { left = true; };
    document.addEventListener("visibilitychange", leave, { once: true });
    addEventListener("pagehide", leave, { once: true });
    location.href = "x-safari-" + location.href;
    setTimeout(() => { if (!left) showManual(true); }, 1500);
  }

  function guide() {
    if (installed && (isIOS || isAndroid)) {
      $("steps-generic").hidden = true;
      $("in-app").hidden = false;
      $("browser-name").textContent = isIOS ? "Safari" : "Chrome or Samsung Internet";
      $("copy-address").addEventListener("click", copyAddress);
      if (isIOS) {
        $("browser-btn").textContent = "Open in Safari";
        $("browser-btn").hidden = false;
        $("browser-btn").addEventListener("click", openInSafari);
      } else {
        showManual(false);
      }
      return;
    }
    showSteps();
    // A phone saves the address that's showing when the shortcut is made, so
    // swap this page's address for the one that opens the app.
    history.replaceState(null, "", location.pathname);
  }

  // ---------- "Not working right now" ----------
  // Reload when something changes, so the app opens by itself once it's back.
  function waitForApp() {
    if ("EventSource" in window) new EventSource("/api/events").onmessage = () => location.reload();
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "visible") location.reload();
    });
  }

  if ($("open").dataset.mode === "guide") guide(); else waitForApp();

  // The browser this page opens in can then show "Anchor can't connect" for the shortcut.
  if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(() => {});
})();
