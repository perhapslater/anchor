// Anchor service worker: lets Anchor install as a phone app and keeps the
// page shell available if the connection blips. The app list itself is
// always fetched fresh, because it depends on who is looking.
// Anchor replaces the token with a digest of the embedded shell at startup.
const CACHE = "anchor-shell-__ANCHOR_SHELL_VERSION__";
const SHELL = ["/", "/style.css", "/request-view.js", "/app.js", "/avatar.js", "/session.js", "/manifest.webmanifest", "/brand/anchor.svg"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL.map(path => new Request(path, { cache: "reload" })))).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("anchor-shell-") && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/api/")) return;
  // Serve the shell installed together as one release. A newer worker gets a
  // separate cache, and existing controlled pages reload when it takes over.
  if (SHELL.includes(url.pathname)) {
    e.respondWith(caches.open(CACHE).then(c => c.match(url.pathname)).then(hit => hit || fetch(e.request)));
    return;
  }
  // Authorized shortcuts always reach Anchor. Offline navigations can show
  // the public recovery shell; API responses and shortcut pages are never saved.
  const isPage = e.request.mode === "navigate";
  if (!isPage) return;
  e.respondWith(
    fetch(e.request)
      .catch(() =>
        caches.open(CACHE).then(c => c.match("/"))
          .then((hit) => hit || Response.error())
      )
  );
});
