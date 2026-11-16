// Request rows and wording shared by the dashboard and setup preview.
(() => {
  "use strict";
  const REQ_TONES = { ready: "up", partial: "up", downloading: "info", soon: "info", notfound: "warn", unconfirmed: "warn" };
  const POSTER_NAME = /^(plex|tmdbmovie|tmdbtv|movie|show)\/[1-9][0-9]{0,8}$/;

  function formatDate(ymd) {
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(typeof ymd === "string" ? ymd : "");
    if (!m) return "";
    const d = new Date(+m[1], +m[2] - 1, +m[3]);
    if (isNaN(d) || d.getMonth() !== +m[2] - 1) return "";
    const opts = { weekday: "short", month: "short", day: "numeric" };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
    return d.toLocaleDateString([], opts);
  }

  function requestWording(row) {
    const date = formatDate(row.date);
    switch (row.state) {
      case "ready": return { main: "Ready to watch" };
      case "partial": {
        const n = Number(row.ready) || 0;
        const main = `${n} episode${n === 1 ? "" : "s"} ready`;
        if (row.downloading) return { main, sub: "Downloading now" };
        if (row.stuck) return { main, sub: "The rest aren't found yet" };
        if (date) return { main, sub: `Next episode airs ${date}` };
        return { main };
      }
      case "downloading": return { main: "Downloading now" };
      case "soon":
        if (row.kind === "show") {
          return date ? { main: `Premieres ${date}` } : { main: "Not out yet", sub: "No premiere date announced yet" };
        }
        return { main: "Not out yet", sub: date ? `Expected ${date}` : "No release date announced yet" };
      case "preparing": return { main: "Getting it ready" };
      case "notfound": return { main: "Not found yet · check back later" };
      case "pending": return { main: "Waiting for approval" };
      case "declined": return { main: "Not approved" };
      case "unconfirmed": return { main: "Can't confirm yet" };
      default: return { main: "Checking…" };
    }
  }

  function comingWording(row) {
    const main = formatDate(row.date) || "Soon";
    switch (row.note) {
      case "premiere": return { main, sub: `Season ${Number(row.season) || 1} premiere` };
      case "episodes": return { main, sub: `${Number(row.count) || 2} new episodes` };
      case "episode": return { main, sub: "New episode" };
      case "cinemas": return { main, sub: "In cinemas" };
      case "home": return { main, sub: "Home release" };
      default: return { main };
    }
  }

  function plain(s) {
    return String(s || "").normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();
  }

  function ageText(seconds) {
    const s = Math.max(0, Math.round(seconds));
    if (s < 45) return "just now";
    const m = Math.round(s / 60);
    if (m < 60) return m <= 1 ? "a minute ago" : `${m} minutes ago`;
    const h = Math.round(m / 60);
    if (h < 48) return h === 1 ? "an hour ago" : `${h} hours ago`;
    return `${Math.round(h / 24)} days ago`;
  }

  function posterTile(row, posterURL) {
    const show = row.kind === "show";
    const kind = show ? "show" : "movie";
    const tile = document.createElement("span");
    tile.className = "req-poster";
    tile.dataset.kind = kind;
    const svg = "http://www.w3.org/2000/svg";
    const icon = document.createElementNS(svg, "svg");
    icon.setAttribute("viewBox", "0 0 24 24");
    icon.setAttribute("aria-hidden", "true");
    const use = document.createElementNS(svg, "use");
    use.setAttribute("href", show ? "#kind-show" : "#kind-movie");
    icon.append(use);
    tile.append(icon);
    const names = (Array.isArray(row.posters) ? row.posters : []).filter((n) => typeof n === "string" && POSTER_NAME.test(n)).slice(0, 3);
    if (names.length) {
      const img = document.createElement("img");
      img.alt = "";
      img.width = 48;
      img.height = 72;
      img.loading = "lazy"; // only the ones scrolled into view are asked for
      img.decoding = "async";
      let at = 0;
      img.addEventListener("load", () => tile.classList.add("has-poster"));
      img.addEventListener("error", () => {
        at++; // that one didn't come: try the next place, and if none is left, keep the tile
        if (at < names.length) img.src = posterURL(names[at]);
        else img.remove();
      });
      img.src = posterURL(names[0]);
      tile.append(img);
    }
    return tile;
  }

  function reqItem(row, say, tone, posterURL) {
    const li = document.createElement("li");
    li.className = "req-row";
    const line = document.createElement("div");
    line.className = "req-line";
    const body = document.createElement("div");
    body.className = "req-body";
    const name = document.createElement("div");
    name.className = "req-name";
    name.textContent = String(row.title || "");
    const type = document.createElement("div");
    type.className = "req-type";
    type.textContent = [row.kind === "show" ? "TV" : "Movie", row.year ? String(row.year) : ""].filter(Boolean).join(" · ");

    const state = document.createElement("div");
    state.className = "req-state";
    state.dataset.tone = tone;
    const dot = document.createElement("span");
    dot.className = "dot";
    dot.setAttribute("aria-hidden", "true");
    const words = document.createElement("span");
    words.className = "req-words";
    const main = document.createElement("span");
    main.className = "req-main";
    main.textContent = say.main;
    words.append(main);
    if (say.sub) {
      const sub = document.createElement("span");
      sub.className = "req-sub";
      sub.textContent = say.sub;
      words.append(sub);
    }
    state.append(dot, words);
    body.append(name, type, state);
    line.append(posterTile(row, posterURL), body);
    li.append(line);
    li.dataset.search = plain(row.title);
    return li;
  }

  function reqRow(row, admin, posterURL) {
    const li = reqItem(row, requestWording(row), REQ_TONES[row.state] || "quiet", posterURL);
    if (admin && row.detail) {
      const d = document.createElement("div");
      d.className = "req-detail small muted";
      d.textContent = String(row.detail);
      li.append(d);
    }
    return li;
  }

  window.anchorRequestView = { formatDate, requestWording, comingWording, plain, ageText, posterTile, reqItem, reqRow };
})();
