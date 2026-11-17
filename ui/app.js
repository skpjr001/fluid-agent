// fluid-agent console. Plain JS, no build step. All DOM is built with
// textContent / createElement, never innerHTML, so payload data cannot inject markup.
"use strict";

// ---------------------------------------------------------------- helpers

const $app = document.getElementById("app");
const MASK = "********";
// External path prefix (e.g. "/fluid-admin"), filled in by the server.
const BASE = (() => {
  const b = document.querySelector('meta[name="fluid-base"]')?.content || "";
  return b.includes("{{") ? "" : b.replace(/\/$/, "");
})();
// appPath is the route inside the app, without the base prefix.
function appPath() {
  const p = location.pathname;
  return BASE && (p === BASE || p.startsWith(BASE + "/")) ? p.slice(BASE.length) || "/" : p;
}

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (k === "checked") el.checked = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces an element's children, skipping null/false (replaceChildren would print "null").
function fill(el, ...children) {
  el.replaceChildren(...children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false));
  return el;
}

const SVGNS = "http://www.w3.org/2000/svg";
const ICONS = {
  home: "M3 10.5 12 3l9 7.5V21h-6v-6H9v6H3z",
  inbox: "M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6H3v-6z",
  send: "M3 11 21 3l-8 18-2-8z",
  board: "M4 4h4v16H4zM10 4h4v10h-4zM16 4h4v13h-4z",
  list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  hook: "M9 7a3 3 0 1 1 6 0c0 2-3 3-3 6v2a3 3 0 1 0 3 3",
  route: "M6 3v12a3 3 0 0 0 3 3h9M15 15l3 3-3 3M6 3l-3 3M6 3l3 3",
  key: "M15 7a4 4 0 1 1-4 4M11 11 3 19v2h3l1-1v-2h2v-2h2l1-1",
  shield: "M12 3 4 6v6c0 5 4 8 8 9 4-1 8-4 8-9V6z",
  gear: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19 12l2-1-1-3-2 .5-1.5-1.5L17 5l-3-1-1 2h-2L10 4 7 5l.5 2L6 8.5 4 8l-1 3 2 1v0l-2 1 1 3 2-.5L7.5 17 7 19l3 1 1-2h2l1 2 3-1-.5-2 1.5-1.5 2 .5 1-3z",
  plus: "M12 5v14M5 12h14",
  refresh: "M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7",
  close: "M6 6l12 12M18 6 6 18",
  retry: "M4 4v6h6M20 20v-6h-6M5 15a7 7 0 0 0 12 3M19 9A7 7 0 0 0 7 6",
  edit: "M4 20h4L20 8l-4-4L4 16zM14 6l4 4",
  trash: "M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3",
  logout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
  menu: "M4 6h16M4 12h16M4 18h16",
  down: "M12 5v14M6 13l6 6 6-6",
  up: "M12 19V5M6 11l6-6 6 6",
  logs: "M5 4h14v16H5zM8 8h8M8 12h8M8 16h5",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  lock: "M6 11h12v10H6zM8 11V7a4 4 0 0 1 8 0v4",
};
function icon(name) {
  const s = document.createElementNS(SVGNS, "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("fill", "none");
  s.setAttribute("stroke", "currentColor");
  s.setAttribute("stroke-width", "2");
  s.setAttribute("stroke-linecap", "round");
  s.setAttribute("stroke-linejoin", "round");
  s.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(SVGNS, "path");
  p.setAttribute("d", ICONS[name] || "");
  s.append(p);
  return s;
}

const COLORS = ["#0c66e4", "#6e5dc6", "#22a06b", "#c9372c", "#e56910", "#1d7f8c", "#943d73", "#5b7f24"];
function avatar(name, small) {
  name = name || "?";
  let n = 0;
  for (const ch of name) n = (n * 31 + ch.charCodeAt(0)) >>> 0;
  const el = h("span", { class: "avatar" + (small ? " sm" : ""), title: name }, name.slice(0, 2).toUpperCase());
  el.style.background = COLORS[n % COLORS.length];
  return el;
}

function lozenge(text, kind) { return h("span", { class: "lz " + (kind || "") }, text); }
const STATUS_KIND = { delivered: "success", ok: "success", pending: "info", inflight: "purple", failed: "danger",
  dead: "danger", partial: "warn", error: "danger", warn: "warn", info: "", debug: "" };
function statusLozenge(s) { return lozenge(s === "inflight" ? "in flight" : s, STATUS_KIND[s] || ""); }
function httpLozenge(code) {
  if (!code) return h("span", { class: "muted" }, "—");
  const kind = code >= 500 ? "danger" : code >= 400 ? "warn" : code >= 300 ? "info" : "success";
  return lozenge(String(code), kind);
}
function dirTag(d) {
  if (d === "inbound") return h("span", { class: "dir in", title: "Incoming call" }, icon("down"), "IN");
  if (d === "outbound") return h("span", { class: "dir out", title: "Outgoing call" }, icon("up"), "OUT");
  return h("span", { class: "muted" }, "—");
}

function ago(ts) {
  const d = (Date.now() - new Date(ts).getTime()) / 1000;
  if (d < 5) return "just now";
  if (d < 60) return Math.floor(d) + "s ago";
  if (d < 3600) return Math.floor(d / 60) + "m ago";
  if (d < 86400) return Math.floor(d / 3600) + "h ago";
  return Math.floor(d / 86400) + "d ago";
}
// ---- date & time: always shown in the viewer's local time zone ----
const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || "local";
const TZ_SHORT = (() => {
  try { return new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date()).find((x) => x.type === "timeZoneName").value; }
  catch { return TZ; }
})();
const DT_FMT = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });
function validTime(ts) { if (!ts) return null; const d = new Date(ts); return isNaN(d) || d.getFullYear() < 2000 ? null : d; }
function fmtDT(ts) { const d = validTime(ts); return d ? DT_FMT.format(d) + " " + TZ_SHORT : ""; }
// when renders a local date-time; the tooltip adds "5m ago" and UTC.
function when(ts) {
  const d = validTime(ts);
  if (!d) return h("span", { class: "muted" }, "—");
  return h("span", { class: "dt", title: ago(ts) + " · " + d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC") }, DT_FMT.format(d));
}
function bytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}
function eventKey(ev) { return (ev.route || "ev").toUpperCase().slice(0, 10) + "-" + ev.id.slice(-6).toUpperCase(); }
function pretty(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

function toast(msg, err) {
  const t = h("div", { class: "toast" + (err ? " err" : "") }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), err ? 7000 : 3500);
}

// ---------------------------------------------------------------- API

const state = { token: null, me: null, live: false, stream: null, page: null, status: null };

try { state.token = sessionStorage.getItem("fluid-token"); } catch { /* storage blocked */ }

class ApiError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body) {
  const opts = { method, headers: { Authorization: "Bearer " + state.token } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(BASE + path, opts);
  if (resp.status === 401) { logout(); throw new ApiError(401, "Session expired"); }
  if (resp.status === 204) return null;
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!resp.ok) throw new ApiError(resp.status, (data && data.error) || resp.statusText);
  return data;
}

// download fetches a file with the session token and saves it.
async function download(path, fallbackName) {
  const resp = await fetch(BASE + path, { headers: { Authorization: "Bearer " + state.token } });
  if (resp.status === 401) { logout(); throw new ApiError(401, "Session expired"); }
  if (!resp.ok) { let m = resp.statusText; try { m = (await resp.json()).error || m; } catch { /* not JSON */ } throw new ApiError(resp.status, m); }
  const name = (/filename="([^"]+)"/.exec(resp.headers.get("Content-Disposition") || "") || [])[1] || fallbackName;
  saveBlob(await resp.blob(), name);
}
function saveBlob(blob, name) {
  const u = URL.createObjectURL(blob);
  const a = h("a", { href: u, download: name });
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(u), 10000);
}
// csvCell quotes a value and neutralises spreadsheet formulas (= + - @).
function csvCell(v) {
  let s = v === null || v === undefined ? "" : Array.isArray(v) ? v.join(" ") : String(v);
  if (typeof v === "string" && /^[=+\-@\t\r]/.test(s)) s = "'" + s;
  return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}
// isoLocal formats a time with the viewer's UTC offset: 2026-11-17T20:31:43+05:30
function isoLocal(ts) {
  const d = validTime(ts); if (!d) return "";
  const p = (n) => String(Math.abs(n)).padStart(2, "0"), off = -d.getTimezoneOffset();
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}${off >= 0 ? "+" : "-"}${p(Math.trunc(off / 60))}:${p(off % 60)}`;
}
function stamp() { return isoLocal(new Date()).slice(0, 19).replace(/[-:]/g, "").replace("T", "-"); }

const isEditor = () => state.me && state.me.role === "editor";

function logout() {
  try { sessionStorage.removeItem("fluid-token"); } catch { /* ignore */ }
  state.token = null;
  state.me = null;
  stopLive();
  renderLogin();
}

// ---------------------------------------------------------------- live stream

// The stream is read with fetch() so the token stays in a header.
async function startLive() {
  if (state.stream) return;
  const ctrl = new AbortController();
  state.stream = ctrl;
  setLiveIndicator(true);
  try {
    const resp = await fetch(BASE + "/api/stream?topics=logs,events,deliveries", {
      headers: { Authorization: "Bearer " + state.token }, signal: ctrl.signal });
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, i);
        buf = buf.slice(i + 2);
        let ev = "message", data = "";
        for (const line of chunk.split("\n")) {
          if (line.startsWith("event: ")) ev = line.slice(7);
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (data && state.page && state.page.onLive) {
          try { state.page.onLive(ev, JSON.parse(data)); } catch (e) { console.error(e); }
        }
      }
    }
  } catch (e) {
    if (e.name !== "AbortError") console.warn("stream ended", e);
  }
  if (state.stream === ctrl) {
    state.stream = null;
    if (state.live) setTimeout(startLive, 3000); // reconnect
  }
  setLiveIndicator(false);
}
function stopLive() {
  if (state.stream) state.stream.abort();
  state.stream = null;
  setLiveIndicator(false);
}
function setLiveIndicator(on) {
  const el = document.getElementById("live");
  if (el) {
    el.classList.toggle("on", on && state.live);
    el.lastChild.textContent = state.live ? (on ? "Live" : "Reconnecting…") : "Paused";
  }
}
function toggleLive() {
  state.live = !state.live;
  try { localStorage.setItem("fluid-live", state.live ? "1" : "0"); } catch { /* ignore */ }
  state.live ? startLive() : stopLive();
  setLiveIndicator(!!state.stream);
}

// ---------------------------------------------------------------- shell & router

const NAV = [
  { group: "Activity", items: [
    { path: "/", label: "Overview", icon: "home" },
    { path: "/events", label: "Events", icon: "inbox" },
    { path: "/deliveries", label: "Deliveries", icon: "board", badge: "dead" },
    { path: "/logs", label: "Logs", icon: "logs" },
  ] },
  { group: "Configuration", items: [
    { path: "/webhooks", label: "Webhooks", icon: "hook" },
    { path: "/ingest", label: "Ingest rules", icon: "route" },
    { path: "/profiles", label: "Forward profiles", icon: "send" },
    { path: "/proxy", label: "Proxy & clients", icon: "shield" },
    { path: "/access", label: "Access (IP allowlists)", icon: "lock" },
  ] },
  { group: "System", items: [
    { path: "/system", label: "Status", icon: "gear", badge: "config" },
  ] },
];

const ROUTES = [
  [/^\/$/, () => pageOverview()],
  [/^\/events$/, () => pageEvents()],
  [/^\/deliveries$/, () => pageDeliveries()],
  [/^\/logs$/, () => pageLogs()],
  [/^\/webhooks$/, () => pageResources(RES.webhooks)],
  [/^\/ingest$/, () => pageResources(RES.ingest)],
  [/^\/profiles$/, () => pageResources(RES.profiles)],
  [/^\/proxy$/, () => pageProxy()],
  [/^\/access$/, () => pageAccess()],
  [/^\/system$/, () => pageSystem()],
];

function navigate(path, replace) {
  if (replace) history.replaceState(null, "", BASE + path);
  else history.pushState(null, "", BASE + path);
  route();
}
window.addEventListener("popstate", () => route());
document.addEventListener("click", (e) => {
  const a = e.target.closest("a[data-link]");
  if (!a || e.metaKey || e.ctrlKey || e.shiftKey) return;
  e.preventDefault();
  navigate(a.dataset.link);
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    const ov = document.querySelector(".overlay");
    if (ov) ov.remove(); else closeDetail();
  }
});

function link(path, ...children) { return h("a", { href: BASE + path, "data-link": path }, ...children); }

function renderShell() {
  const role = state.me.role;
  fill($app, 
    h("div", { class: "shell", id: "shell" },
      h("header", { class: "topbar" },
        h("button", { class: "btn subtle menu-btn", "aria-label": "Menu",
          onclick: () => document.getElementById("shell").classList.toggle("nav-open") }, icon("menu")),
        link("/", h("span", { class: "logo" }, h("span", { class: "logo-mark" }, "F"), "fluid-agent")),
        h("span", { class: "spacer" }),
        h("input", { class: "global-search", type: "search", placeholder: "Search events (path, vendor, type)…",
          "aria-label": "Search events",
          onkeydown: (e) => { if (e.key === "Enter") navigate("/events?q=" + encodeURIComponent(e.target.value)); } }),
        h("button", { class: "live", id: "live", title: "Toggle live updates", onclick: toggleLive },
          h("span", { class: "live-dot" }), h("span", {}, "Paused")),
        lozenge(role === "editor" ? "Editor" : "Viewer", role === "editor" ? "info" : ""),
        avatar(state.me.name),
        h("button", { class: "btn subtle", title: "Sign out", "aria-label": "Sign out", onclick: logout }, icon("logout"))),
      h("nav", { class: "sidebar", "aria-label": "Main" },
        h("div", { class: "project" }, h("div", { class: "project-icon" }),
          h("div", {}, h("div", { class: "project-name" }, "Relay agent"),
            h("div", { class: "project-sub" }, "Proxy · Webhooks · Ingest"))),
        NAV.map((g) => h("div", { class: "nav-group" }, h("div", { class: "nav-title" }, g.group),
          h("div", { class: "nav" }, g.items.map((it) => link(it.path, icon(it.icon), it.label,
            it.badge ? h("span", { class: "count", "data-badge": it.badge }) : null)))))),
      h("main", { class: "main", id: "main" }),
      h("aside", { class: "detail", id: "detail", "aria-label": "Details" })));
  setLiveIndicator(!!state.stream);
}

async function route() {
  if (!state.me) return;
  closeDetail();
  document.getElementById("shell")?.classList.remove("nav-open");
  const path = appPath();
  for (const a of document.querySelectorAll(".nav a")) {
    const p = a.dataset.link;
    a.classList.toggle("active", p === "/" ? path === "/" : path.startsWith(p));
  }
  const match = ROUTES.find(([re]) => re.test(path));
  const main = document.getElementById("main");
  state.page = null;
  fill(main, h("div", { class: "page" }, h("div", { class: "muted" }, "Loading…")));
  try {
    state.page = (match ? match[1] : () => pageNotFound())() || null;
    const p = await state.page;
    state.page = p || null;
  } catch (e) {
    fill(main, h("div", { class: "page" }, banner("danger", "Could not load this page", e.message)));
  }
  refreshBadges();
}

async function refreshBadges() {
  try {
    const st = await api("GET", "/api/status");
    state.status = st;
    const set = (k, n, danger) => {
      const el = document.querySelector(`[data-badge="${k}"]`);
      if (!el) return;
      fill(el, n ? h("span", { class: "badge" + (danger ? " danger" : "") }, String(n)) : "");
    };
    set("dead", st.deliveries.dead + st.deliveries.failed, true);
    set("config", Object.keys(st.config.errors || {}).length, true);
  } catch { /* ignore */ }
}

function page(title, opts, ...body) {
  const main = document.getElementById("main");
  const el = h("div", { class: "page" },
    h("div", { class: "crumbs" }, "Relay agent", opts.crumb ? [" / ", opts.crumb] : null),
    h("div", { class: "page-head" }, h("h1", {}, title), opts.actions || null),
    opts.desc ? h("p", { class: "page-desc" }, opts.desc) : null,
    ...body);
  fill(main, el);
  document.title = title + " · fluid-agent";
  return el;
}

function banner(kind, title, detail, prose) {
  return h("div", { class: "banner " + kind }, h("div", {}, h("strong", {}, title),
    detail ? (prose ? h("div", {}, detail) : h("pre", {}, detail)) : null));
}

function emptyState(title, text, action) {
  return h("div", { class: "empty" }, h("h3", {}, title), h("div", {}, text), action ? h("div", { class: "more" }, action) : null);
}

function viewerNote() {
  return isEditor() ? null : banner("info", "View only", "You are signed in with a viewer token. Secrets are hidden and changes are disabled; ask for an editor token to change configuration.", true);
}

// ---------------------------------------------------------------- detail panel

function openDetail(keyText, title, ...body) {
  const d = document.getElementById("detail");
  fill(d, 
    h("div", { class: "detail-head" }, h("span", { class: "key" }, keyText), h("span", { class: "spacer" }),
      h("button", { class: "btn subtle", "aria-label": "Close", onclick: closeDetail }, icon("close"))),
    h("div", { class: "detail-body" }, h("h2", {}, title), ...body));
  d.classList.add("open");
  d.querySelector("button").focus();
}
function closeDetail() {
  const d = document.getElementById("detail");
  if (d) d.classList.remove("open");
  for (const r of document.querySelectorAll("tr.selected")) r.classList.remove("selected");
}
function props(pairs) {
  return h("dl", { class: "props" }, pairs.filter(Boolean).map(([k, v]) => [h("dt", {}, k), h("dd", {}, v ?? "—")]));
}

// ---------------------------------------------------------------- data table
//
// dataTable({ id, cols, fetch | rows, onRow, empty, pageSize, defaultSort })
//   col: { key, label, render, v (value for client-side sort/filter), sort, filter, num, time }
//   filter: "text" | "num" | "time" | [[value, label], ...] (exact match)
//   fetch({offset, limit, sort, filters}) -> {items, total}   (server-side paging)
//   rows: [...]                                               (client-side paging)
// Click a header to sort, Shift+click to add a secondary sort. State lives in the URL.

const PAGE_SIZES = [10, 25, 50, 100, 200];
const TIME_PRESETS = [["", "Any time"], ["15m..", "Last 15 min"], ["1h..", "Last hour"], ["24h..", "Last 24 h"], ["7d..", "Last 7 days"], ["30d..", "Last 30 days"], ["custom", "Custom range…"]];
const DUR = { s: 1e3, m: 6e4, h: 36e5, d: 864e5 };

function parseInstantJS(s) {
  const m = /^(\d+)([smhd])$/.exec(s);
  if (m) return Date.now() - m[1] * DUR[m[2]];
  const t = Date.parse(s);
  return isNaN(t) ? null : t;
}
function matchTime(ts, f) {
  const t = new Date(ts).getTime();
  if (!validTime(ts)) return false;
  const [a, b] = f.includes("..") ? f.split("..") : [f, ""];
  const lo = a ? parseInstantJS(a) : null, hi = b ? parseInstantJS(b) : null;
  return (lo === null || t >= lo) && (hi === null || t < hi);
}
function matchNum(v, f) {
  if (v === "" || v === null || v === undefined) return false;
  v = Number(v); f = f.replace(/\s/g, "");
  let m;
  if ((m = /^([1-9])xx$/i.exec(f))) return v >= m[1] * 100 && v < m[1] * 100 + 100;
  if ((m = /^(>=|<=|>|<|=)(-?\d+(?:\.\d+)?)$/.exec(f))) {
    const n = Number(m[2]);
    return { ">=": v >= n, "<=": v <= n, ">": v > n, "<": v < n, "=": v === n }[m[1]];
  }
  if ((m = /^(\d+)-(\d+)$/.exec(f))) return v >= +m[1] && v <= +m[2];
  return String(v) === f;
}
function cellValue(c, r) { const v = c.v ? c.v(r) : r[c.key]; return v === undefined || v === null ? "" : v; }
function cmp(a, b) {
  if (typeof a === "number" && typeof b === "number") return a - b;
  const ta = Date.parse(a), tb = Date.parse(b);
  if (typeof a === "string" && typeof b === "string" && /^\d{4}-\d\d-\d\dT/.test(a) && /^\d{4}-\d\d-\d\dT/.test(b)) return ta - tb;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}
// datetime-local input value <-> ISO (UTC) string
function localInputToISO(v) { return v ? new Date(v).toISOString() : ""; }
function isoToLocalInput(s) {
  const d = validTime(s); if (!d) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function dataTable(o) {
  const P = o.id ? o.id + "." : "";
  const url = new URLSearchParams(location.search);
  const st = {
    page: Math.max(1, parseInt(url.get(P + "page"), 10) || 1),
    size: PAGE_SIZES.includes(+url.get(P + "size")) ? +url.get(P + "size") : (o.pageSize || 25),
    sort: (url.get(P + "sort") ?? (o.defaultSort || []).join(",")).split(",").filter(Boolean)
      .map((x) => { const [k, d] = x.split(":"); return { k, d: d === "desc" ? "desc" : "asc" }; }),
    filters: { ...(o.initialFilters || {}) },
  };
  for (const c of o.cols) if (c.filter) { const v = url.get(P + "f." + c.key); if (v) st.filters[c.key] = v; }
  let data = { items: [], total: 0 }, seq = 0;

  const save = () => {
    const q = new URLSearchParams(location.search);
    for (const k of [...q.keys()]) if (k.startsWith(P + "f.") || [P + "page", P + "size", P + "sort"].includes(k)) q.delete(k);
    if (st.page > 1) q.set(P + "page", st.page);
    if (st.size !== (o.pageSize || 25)) q.set(P + "size", st.size);
    const srt = st.sort.map((x) => x.k + ":" + x.d).join(",");
    if (srt !== (o.defaultSort || []).join(",")) q.set(P + "sort", srt);
    for (const [k, v] of Object.entries(st.filters)) if (v) q.set(P + "f." + k, v);
    const qs = q.toString();
    history.replaceState(null, "", location.pathname + (qs ? "?" + qs : ""));
  };

  // header: labels (sortable) + one filter control per column, built once
  const thFor = new Map();
  const headRow = h("tr", {}, o.cols.map((c) => {
    const label = [c.label, c.time ? h("span", { class: "tz", title: "Shown in your time zone: " + TZ }, TZ_SHORT) : null];
    if (!c.sort) return h("th", { class: c.num ? "num" : "" }, label);
    const ind = h("span", { class: "sort-ind" });
    const th = h("th", { class: "sortable" + (c.num ? " num" : ""), tabindex: "0", "aria-sort": "none",
      title: "Click to sort, Shift+click to add a secondary sort" }, label, ind);
    const toggle = (multi) => {
      const i = st.sort.findIndex((x) => x.k === c.key);
      if (!multi) {
        const cur = i >= 0 ? st.sort[i].d : null;
        st.sort = cur === null ? [{ k: c.key, d: c.time || c.num ? "desc" : "asc" }] : cur === (c.time || c.num ? "desc" : "asc")
          ? [{ k: c.key, d: c.time || c.num ? "asc" : "desc" }] : [];
      } else if (i < 0) st.sort.push({ k: c.key, d: c.time || c.num ? "desc" : "asc" });
      else if (st.sort[i].d === (c.time || c.num ? "desc" : "asc")) st.sort[i].d = st.sort[i].d === "asc" ? "desc" : "asc";
      else st.sort.splice(i, 1);
      st.page = 1; load();
    };
    th.addEventListener("click", (e) => toggle(e.shiftKey));
    th.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(e.shiftKey); } });
    thFor.set(c.key, { th, ind });
    return th;
  }));
  let timer;
  const setFilter = (k, v, now) => {
    clearTimeout(timer);
    const apply = () => { if ((st.filters[k] || "") === v) return; if (v) st.filters[k] = v; else delete st.filters[k]; st.page = 1; load(); };
    if (now) apply(); else timer = setTimeout(apply, 350);
  };
  const filterCell = (c) => {
    if (!c.filter) return h("th", {});
    const cur = st.filters[c.key] || "";
    if (Array.isArray(c.filter)) {
      return h("th", {}, select([["", "All"], ...c.filter.map(([v, t]) => ["=" + v, t])], cur, (v) => setFilter(c.key, v, true), "Filter " + c.label));
    }
    if (c.filter === "time") {
      const from = h("input", { type: "datetime-local", "aria-label": c.label + " from" });
      const to = h("input", { type: "datetime-local", "aria-label": c.label + " to" });
      const custom = h("div", { class: "dt-range hidden" }, from, h("span", { class: "muted" }, "to"), to);
      const isPreset = TIME_PRESETS.some(([v]) => v === cur);
      const sel = select(TIME_PRESETS, isPreset ? cur : cur ? "custom" : "", (v) => {
        custom.classList.toggle("hidden", v !== "custom");
        if (v !== "custom") setFilter(c.key, v, true); else from.focus();
      }, "Filter " + c.label);
      if (cur && !isPreset) {
        const [a, b] = cur.split("..");
        from.value = isoToLocalInput(a); to.value = isoToLocalInput(b);
        custom.classList.remove("hidden");
      }
      const applyRange = () => {
        const a = localInputToISO(from.value), b = localInputToISO(to.value);
        setFilter(c.key, a || b ? a + ".." + b : "", true);
      };
      from.addEventListener("change", applyRange); to.addEventListener("change", applyRange);
      return h("th", { class: "f-time" }, sel, custom);
    }
    const inp = h("input", { type: "search", value: cur, "aria-label": "Filter " + c.label,
      placeholder: c.filter === "num" ? "200, 5xx, >=400" : "Filter…",
      title: c.filter === "num" ? "A number, 5xx, >=400, <100 or 200-299" : "Contains (case-insensitive); start with = for an exact match" });
    inp.addEventListener("input", () => setFilter(c.key, inp.value.trim()));
    inp.addEventListener("keydown", (e) => { if (e.key === "Enter") setFilter(c.key, inp.value.trim(), true); });
    return h("th", {}, inp);
  };
  const hasFilters = o.cols.some((c) => c.filter);
  const tbody = h("tbody", {});
  const tableEl = h("table", { class: "issues" }, h("thead", {}, headRow, hasFilters ? h("tr", { class: "filters" }, o.cols.map(filterCell)) : null), tbody);
  const pager = h("div", { class: "pager" });
  const chips = h("div", { class: "chips" });
  const exportBtn = h("button", { class: "btn sm", title: "Download every row matching the current search, filters and sort (not just this page)" }, icon("download"), "Export CSV");
  exportBtn.addEventListener("click", async () => {
    exportBtn.disabled = true;
    try {
      if (o.fetch) {
        const q = o.exportParams ? o.exportParams() : new URLSearchParams();
        const srt = st.sort.map((x) => x.k + ":" + x.d).join(",");
        if (srt) q.set("sort", srt);
        for (const [k, v] of Object.entries(st.filters)) if (v) q.set("f." + k, v);
        q.set("format", "csv"); q.set("tz", TZ);
        await download(o.exportPath + "?" + q, (o.exportName || "export") + ".csv");
      } else {
        const cols = o.cols.filter((c) => c.key && c.label);
        const rows = clientRows();
        const lines = [cols.map((c) => csvCell(c.label)).join(",")].concat(rows.map((r) => cols.map((c) => {
          const v = c.csv ? c.csv(r) : cellValue(c, r);
          return csvCell(c.time ? isoLocal(v) : v);
        }).join(",")));
        saveBlob(new Blob(["\ufeff" + lines.join("\r\n") + "\r\n"], { type: "text/csv;charset=utf-8" }), `fluid-${o.exportName || "export"}-${stamp()}.csv`);
      }
      toast("Exported " + data.total.toLocaleString() + " row" + (data.total === 1 ? "" : "s"));
    } catch (e) { toast(e.message, true); }
    finally { exportBtn.disabled = false; }
  });
  const el = h("div", { class: "data-table" }, h("div", { class: "dt-bar" }, chips, exportBtn), h("div", { class: "table-wrap" }, tableEl), pager);

  const clientPage = () => {
    const rows = clientRows();
    return { items: rows.slice((st.page - 1) * st.size, st.page * st.size), total: rows.length };
  };
  function clientRows() {
    let rows = o.rows.slice();
    for (const [k, f] of Object.entries(st.filters)) {
      const c = o.cols.find((x) => x.key === k); if (!c) continue;
      rows = rows.filter((r) => {
        const v = cellValue(c, r);
        if (c.filter === "time") return matchTime(v, f);
        if (c.filter === "num") return matchNum(v, f);
        if (f.startsWith("=")) return String(v) === f.slice(1);
        return String(Array.isArray(v) ? v.join(" ") : v).toLowerCase().includes(f.toLowerCase());
      });
    }
    if (st.sort.length) {
      const cs = st.sort.map((x) => [o.cols.find((c) => c.key === x.k), x.d === "desc" ? -1 : 1]).filter(([c]) => c);
      rows.sort((a, b) => { for (const [c, dir] of cs) { const r = cmp(cellValue(c, a), cellValue(c, b)); if (r) return r * dir; } return 0; });
    }
    return rows;
  }

  const load = async () => {
    save();
    const my = ++seq;
    try {
      data = o.fetch
        ? await o.fetch({ offset: (st.page - 1) * st.size, limit: st.size, sort: st.sort.map((x) => x.k + ":" + x.d).join(","), filters: st.filters })
        : clientPage();
    } catch (e) { if (my === seq) { fill(tbody, h("tr", {}, h("td", { colspan: String(o.cols.length) }, banner("danger", "Could not load", e.message)))); } return; }
    if (my !== seq) return;
    const pages = Math.max(1, Math.ceil(data.total / st.size));
    if (st.page > pages && data.total > 0) { st.page = pages; return load(); }
    draw();
  };

  const draw = () => {
    for (const [k, { th, ind }] of thFor) {
      const i = st.sort.findIndex((x) => x.k === k);
      th.classList.toggle("sorted", i >= 0);
      th.setAttribute("aria-sort", i < 0 ? "none" : st.sort[i].d === "asc" ? "ascending" : "descending");
      fill(ind, i < 0 ? "↕" : [st.sort[i].d === "asc" ? "▲" : "▼", st.sort.length > 1 ? h("sup", {}, String(i + 1)) : null]);
    }
    const active = Object.entries(st.filters).filter(([, v]) => v);
    fill(chips, active.length || st.sort.length ? [
      active.map(([k, v]) => {
        const c = o.cols.find((x) => x.key === k);
        return h("span", { class: "chip" }, h("strong", {}, (c ? c.label : k) + ": "), chipText(c, v),
          h("button", { class: "chip-x", "aria-label": "Remove filter", onclick: () => { delete st.filters[k]; st.page = 1; resetFilterInputs(); load(); } }, "×"));
      }),
      st.sort.length ? h("span", { class: "chip muted" }, "Sorted by ", st.sort.map((x, i) => {
        const c = o.cols.find((y) => y.key === x.k);
        return (i ? ", " : "") + (c ? c.label : x.k) + (x.d === "asc" ? " ↑" : " ↓");
      })) : null,
      h("button", { class: "btn subtle sm", onclick: () => { st.filters = {}; st.sort = (o.defaultSort || []).map((x) => { const [k, d] = x.split(":"); return { k, d }; }); st.page = 1; resetFilterInputs(); load(); } }, "Clear all"),
    ] : null);
    fill(tbody, data.items.length ? data.items.map((r) => tableRow(o.cols, r, o.onRow))
      : h("tr", {}, h("td", { colspan: String(o.cols.length), class: "empty-cell" },
        emptyState(active.length ? "Nothing matches these filters" : (o.empty || ["Nothing here yet", ""])[0],
          active.length ? "Change or clear the column filters." : (o.empty || ["", ""])[1]))));
    drawPager();
    o.onDraw && o.onDraw(data);
  };
  const chipText = (c, v) => {
    if (c && c.filter === "time") {
      const p = TIME_PRESETS.find(([x]) => x === v); if (p) return p[1];
      const [a, b] = v.split("..");
      return (a ? fmtDT(a) : "…") + " → " + (b ? fmtDT(b) : "now");
    }
    if (c && Array.isArray(c.filter)) { const f = c.filter.find(([x]) => "=" + x === v); if (f) return f[1]; }
    return v;
  };
  const resetFilterInputs = () => {
    const row = tableEl.querySelector("tr.filters");
    if (row) row.replaceWith(h("tr", { class: "filters" }, o.cols.map(filterCell)));
  };

  const drawPager = () => {
    const total = data.total, pages = Math.max(1, Math.ceil(total / st.size));
    const from = total ? (st.page - 1) * st.size + 1 : 0, to = Math.min(total, st.page * st.size);
    const go = (p) => { p = Math.min(pages, Math.max(1, p)); if (p !== st.page) { st.page = p; load(); } };
    const btn = (label, p, disabled, cls, aria) => h("button", { class: "btn sm " + (cls || ""), disabled: disabled ? true : undefined,
      "aria-label": aria || label, "aria-current": cls === "on" ? "page" : undefined, onclick: () => go(p) }, label);
    const nums = [];
    const win = new Set([1, pages, st.page - 2, st.page - 1, st.page, st.page + 1, st.page + 2].filter((p) => p >= 1 && p <= pages));
    let prev = 0;
    for (const p of [...win].sort((a, b) => a - b)) {
      if (p - prev > 1) nums.push(h("span", { class: "gap" }, "…"));
      nums.push(btn(String(p), p, false, p === st.page ? "on" : "", "Page " + p));
      prev = p;
    }
    const jump = h("input", { type: "number", min: "1", max: String(pages), value: String(st.page), class: "jump", "aria-label": "Go to page" });
    jump.addEventListener("keydown", (e) => { if (e.key === "Enter") go(parseInt(jump.value, 10) || 1); });
    fill(pager,
      h("div", { class: "pager-info" }, total ? `Showing ${from.toLocaleString()}–${to.toLocaleString()} of ${total.toLocaleString()}` : "No results",
        h("span", { class: "muted" }, " · times in " + TZ)),
      h("div", { class: "pager-ctl" },
        h("label", { class: "muted" }, "Rows ", select(PAGE_SIZES.map((n) => [String(n), String(n)]), String(st.size),
          (v) => { st.size = +v; st.page = 1; load(); }, "Rows per page")),
        btn("«", 1, st.page <= 1, "", "First page"), btn("‹", st.page - 1, st.page <= 1, "", "Previous page"),
        nums,
        btn("›", st.page + 1, st.page >= pages, "", "Next page"), btn("»", pages, st.page >= pages, "", "Last page"),
        h("label", { class: "muted" }, "Go to ", jump, " / " + pages)));
  };

  load();
  return {
    el,
    reload: load,
    setRows(rows) { o.rows = rows; load(); },
    get page() { return st.page; },
    get filtered() { return Object.values(st.filters).some(Boolean); },
    get sorted() { return st.sort.map((x) => x.k + ":" + x.d).join(",") !== (o.defaultSort || []).join(","); },
    query() { // the table's filters as API params (e.g. for a summary of the same rows)
      const q = new URLSearchParams();
      for (const [k, v] of Object.entries(st.filters)) if (v) q.set("f." + k, v);
      return q;
    },
  };
}

// serverList pages an /api list endpoint; extra() adds the page's own params.
function serverList(path, extra) {
  return async ({ offset, limit, sort, filters }) => {
    const q = extra ? extra() : new URLSearchParams();
    q.set("offset", String(offset)); q.set("limit", String(limit));
    if (sort) q.set("sort", sort);
    for (const [k, v] of Object.entries(filters)) if (v) q.set("f." + k, v);
    return api("GET", path + "?" + q);
  };
}

// ---------------------------------------------------------------- table

function table(cols, rows, onRow) {
  if (!rows.length) return null;
  return h("div", { class: "table-wrap" }, h("table", { class: "issues" },
    h("thead", {}, h("tr", {}, cols.map((c) => h("th", { class: c.num ? "num" : "" }, c.label)))),
    h("tbody", {}, rows.map((r) => tableRow(cols, r, onRow)))));
}
function tableRow(cols, r, onRow) {
  const tr = h("tr", { tabindex: "0" }, cols.map((c) => h("td", { class: c.num ? "num" : "" }, c.render(r))));
  if (onRow) {
    const open = () => {
      for (const s of document.querySelectorAll("tr.selected")) s.classList.remove("selected");
      tr.classList.add("selected");
      onRow(r);
    };
    tr.addEventListener("click", open);
    tr.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
  }
  return tr;
}

function select(options, value, onchange, label) {
  return h("select", { "aria-label": label, onchange: (e) => onchange(e.target.value) },
    options.map(([v, t]) => h("option", { value: v, selected: v === value ? true : undefined }, t)));
}
function seg(options, value, onchange) {
  return h("div", { class: "seg", role: "group" }, options.map(([v, t]) =>
    h("button", { class: v === value ? "on" : "", "aria-pressed": v === value ? "true" : "false", onclick: () => onchange(v) }, t)));
}

function queryState(defaults) {
  const q = new URLSearchParams(location.search);
  const s = { ...defaults };
  for (const k of Object.keys(defaults)) if (q.has(k)) s[k] = q.get(k);
  return s;
}
function pushQuery(s, defaults) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(s)) if (v && v !== defaults[k]) q.set(k, v);
  const qs = q.toString();
  history.replaceState(null, "", location.pathname + (qs ? "?" + qs : ""));
}
const SINCE = [["15m", "Last 15 min"], ["1h", "Last hour"], ["24h", "Last 24 hours"], ["7d", "Last 7 days"], ["", "All time"]];

// ---------------------------------------------------------------- overview

async function pageOverview() {
  const [st, dir, routes, hosts, kinds, evs] = await Promise.all([
    api("GET", "/api/status"),
    api("GET", "/api/logs/summary?since=24h&by=direction"),
    api("GET", "/api/logs/summary?since=24h&by=route&direction=inbound"),
    api("GET", "/api/logs/summary?since=24h&by=host&direction=outbound"),
    api("GET", "/api/logs/summary?since=24h&by=kind"),
    api("GET", "/api/events?limit=8"),
  ]);
  const d = Object.fromEntries(dir.items.map((r) => [r.key, r]));
  const errs = dir.items.reduce((a, r) => a + r.errors, 0);
  const cfgErrs = Object.entries(st.config.errors || {});
  const card = (label, value, sub, href) => {
    const c = h("div", { class: "card" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "sub" }, sub));
    return href ? link(href, c) : c;
  };
  const bars = (title, items, empty) => h("div", { class: "panel" }, h("h2", {}, title),
    items.length ? items.slice(0, 8).map((r) => {
      const max = items[0].count || 1;
      const fill = h("i", {});
      fill.style.width = Math.max(2, (r.count / max) * 100) + "%";
      const errFill = h("i", { class: "err" });
      errFill.style.width = (r.errors / max) * 100 + "%";
      return h("div", { class: "bar-row" }, h("span", { class: "name", title: r.key }, r.key || "—"),
        h("div", { class: "bar" }, r.errors > r.count / 2 ? errFill : fill),
        h("span", { class: "num muted" }, String(r.count)));
    }) : h("div", { class: "muted" }, empty));

  const p = page("Overview", { desc: "Traffic through this agent in the last 24 hours." },
    cfgErrs.length ? banner("danger", `${cfgErrs.length} config file(s) failed validation — the previous version is still in use`,
      cfgErrs.map(([f, e]) => f + ": " + e).join("\n")) : null,
    h("div", { class: "cards" },
      card("Inbound calls", String(d.inbound?.count || 0), "webhooks + ingest, 24h", "/logs?direction=inbound&since=24h"),
      card("Outbound calls", String(d.outbound?.count || 0), "proxy, forward, deliveries", "/logs?direction=outbound&since=24h"),
      card("Errors", String(errs), "status ≥ 400 or level error", "/logs?status=error&since=24h"),
      card("Pending deliveries", String(st.deliveries.pending + st.deliveries.inflight), "queued or in flight", "/deliveries?status=pending"),
      card("Dead letters", String(st.deliveries.dead + st.deliveries.failed), "need attention", "/deliveries?view=board"),
      card("Active tunnels", String(st.active_tunnels), "CONNECT sessions", "/logs?kind=proxy_connect")),
    h("div", { class: "grid-2" },
      bars("Top inbound routes", routes.items, "No inbound calls yet."),
      bars("Top outbound hosts", hosts.items, "No outbound calls yet."),
      bars("Activity by kind", kinds.items, "No activity yet."),
      h("div", { class: "panel" }, h("h2", {}, "Recent events"),
        evs.items.length ? evs.items.map((ev) => h("div", { class: "bar-row event-row" },
          link("/events?q=" + encodeURIComponent(ev.id), h("span", { class: "key" }, eventKey(ev))),
          h("span", { class: "name" }, ev.path), statusLozenge(ev.status))) : h("div", { class: "muted" }, "No events yet."))));
  return {
    onLive(topic) { if (topic === "events") { clearTimeout(this.t); this.t = setTimeout(() => route(), 1500); } },
    el: p,
  };
}

// ---------------------------------------------------------------- events

// payloadView shows a captured exchange: RQH, RQB, RSH, RSB.
function payloadView(p, masked, l) {
  if (!p) return null;
  const headerText = (hs) => Object.keys(hs || {}).sort((a, b) => (a[0] === ":") === (b[0] === ":") ? a.localeCompare(b) : a[0] === ":" ? -1 : 1)
    .map((k) => hs[k].map((v) => k + ": " + v).join("\n")).join("\n");
  const bodyText = (part) => part.body ? pretty(part.body) : part.body_base64 ? "(binary, base64)\n" + part.body_base64 : "";
  const sizeNote = (part) => {
    if (!part.size) return "empty";
    return bytes(part.size) + (part.truncated ? ` — first ${bytes(part.stored)} stored (capture_max_body_bytes)` : "");
  };
  const block = (code, title, text, note) => {
    const pre = h("pre", { class: "code payload" }, text || "(none)");
    return h("section", { class: "payload-part" },
      h("div", { class: "payload-head" }, h("span", { class: "pl-code" }, code), h("strong", {}, title),
        note ? h("span", { class: "muted" }, note) : null, h("span", { class: "spacer" }),
        text ? h("button", { class: "btn sm subtle", onclick: () => navigator.clipboard?.writeText(text).then(() => toast("Copied " + code)) }, "Copy") : null),
      pre);
  };
  const out = [h("h3", {}, l.direction === "inbound" ? "Request received and response sent" : "Request sent and response received")];
  if (masked) out.push(banner("info", "Credentials are masked for viewer tokens", "Authorization, cookies, tokens and other known secrets show as ********. Editors see the exact values.", true));
  if (l.attrs && l.attrs.encrypted) out.push(banner("info", "HTTPS tunnel", "The traffic inside a CONNECT tunnel is encrypted end to end; only the CONNECT request and its answer are recorded.", true));
  if (p.request) {
    out.push(block("RQH", "Request headers", headerText(p.request.headers)));
    out.push(block("RQB", "Request body", bodyText(p.request), sizeNote(p.request)));
  }
  if (p.response) {
    out.push(block("RSH", "Response headers", headerText(p.response.headers)));
    out.push(block("RSB", "Response body", bodyText(p.response), sizeNote(p.response)));
  } else if (p.request) {
    out.push(h("p", { class: "muted" }, "No response was received (see the error above)."));
  }
  return out;
}

// legacyFilters turns old-style links (?route=x&status=y) into column filters.
function legacyFilters(map) {
  const q = new URLSearchParams(location.search);
  const out = {};
  for (const [k, fn] of Object.entries(map)) {
    const v = q.get(k);
    if (v) { const [col, val] = fn(v); out[col] = val; q.delete(k); }
  }
  if (Object.keys(out).length) { const qs = q.toString(); history.replaceState(null, "", location.pathname + (qs ? "?" + qs : "")); }
  return out;
}
const exact = (col) => (v) => [col, "=" + v];

async function pageEvents() {
  const initialFilters = legacyFilters({ route: exact("route"), source: exact("source"), status: exact("status"), since: (v) => ["ts", v + ".."] });
  const defaults = { q: "" };
  const f = queryState(defaults);
  const routes = await routeOptions();
  const cols = [
    { key: "id", label: "Key", sort: true, filter: "text", render: (e) => h("span", { class: "key", title: e.id }, eventKey(e)) },
    { key: "ts", label: "Received", time: true, sort: true, filter: "time", render: (e) => when(e.ts) },
    { key: "source", label: "Source", sort: true, filter: [["webhook", "Webhook"], ["ingest", "Ingest"]], render: (e) => lozenge(e.source, e.source === "ingest" ? "purple" : "info") },
    { key: "route", label: "Route", sort: true, filter: routes.map((r) => [r, r]), render: (e) => h("span", { class: "cell-flex" }, avatar(e.route, true), h("span", {}, e.route)) },
    { key: "method", label: "Method", sort: true, filter: "text", render: (e) => h("strong", {}, e.method) },
    { key: "path", label: "Path", sort: true, filter: "text", render: (e) => h("span", { class: "mono", title: e.path }, e.path) },
    { key: "vendor", label: "Vendor", sort: true, filter: "text", render: (e) => e.vendor || h("span", { class: "muted" }, "—") },
    { key: "event_type", label: "Event type", sort: true, filter: "text", render: (e) => e.event_type || h("span", { class: "muted" }, "—") },
    { key: "status", label: "Status", sort: true, filter: [["pending", "Pending"], ["delivered", "Delivered"], ["partial", "Partial"], ["failed", "Failed"]], render: (e) => statusLozenge(e.status) },
    { key: "body_size", label: "Size", num: true, sort: true, filter: "num", render: (e) => bytes(e.body_size) },
    { key: "source_ip", label: "From", sort: true, filter: "text", render: (e) => h("span", { class: "mono" }, e.source_ip || "—") },
  ];
  const t = dataTable({
    cols, initialFilters, defaultSort: ["ts:desc"], pageSize: 25,
    fetch: serverList("/api/events", () => { const q = new URLSearchParams(); if (f.q) q.set("q", f.q); return q; }),
    exportPath: "/api/events", exportName: "events", exportParams: () => { const q = new URLSearchParams(); if (f.q) q.set("q", f.q); return q; },
    onRow: (e) => showEvent(e.id),
    empty: ["No events", "Calls to /hooks/<id> and ingest paths appear here."],
    onDraw: (d) => { if (f.q && d.items.length === 1 && d.items[0].id === f.q) showEvent(f.q); },
  });
  page("Events", {
    desc: "Every inbound call (webhook or ingest), stored before it is acknowledged. Filter any column, sort by several (Shift+click), open one to see its body, headers and deliveries.",
    actions: h("button", { class: "btn", onclick: () => t.reload() }, icon("refresh"), "Refresh"),
  },
    h("div", { class: "toolbar" },
      h("input", { type: "search", placeholder: "Search path, vendor, type…", value: f.q, "aria-label": "Search",
        onchange: (e) => { f.q = e.target.value; pushQuery(f, defaults); t.reload(); } })),
    t.el);
  return {
    onLive(topic) {
      if (topic !== "events" || t.page !== 1) return; // live: refresh the first page (with its filters)
      clearTimeout(this.timer); this.timer = setTimeout(() => t.reload(), 800);
    },
  };
}

async function routeOptions() {
  const [w, i] = await Promise.all([api("GET", "/api/webhooks"), api("GET", "/api/ingest")]);
  return [...w.map((x) => x.id), ...i.map((x) => x.id)].sort();
}

async function showEvent(id) {
  let ev;
  try { ev = await api("GET", "/api/events/" + encodeURIComponent(id)); } catch (e) { toast(e.message, true); return; }
  const body = ev.body ? pretty(ev.body) : ev.body_base64 ? "(binary, base64)\n" + ev.body_base64 : ev.body_purged ? "(body removed after delivery)" : "(empty)";
  const headers = Object.entries(ev.headers || {}).map(([k, v]) => k + ": " + v.join(", ")).join("\n");
  openDetail(eventKey(ev), ev.method + " " + ev.path,
    h("div", { class: "toolbar" }, statusLozenge(ev.status), lozenge(ev.source, ev.source === "ingest" ? "purple" : "info"),
      ev.vendor ? lozenge(ev.vendor, "") : null, ev.event_type ? lozenge(ev.event_type, "") : null),
    props([["Event ID", h("span", { class: "mono" }, ev.id)], ["Route", ev.route], ["Received", new Date(ev.ts).toLocaleString()],
      ["From", ev.source_ip], ["Query", ev.query || null], ["Size", bytes(ev.body_size)],
      ev.traceparent ? ["Trace", h("span", { class: "mono" }, ev.traceparent.split("-")[1] || ev.traceparent)] : null]),
    h("h3", {}, `Deliveries (${ev.deliveries.length})`),
    deliveryTimeline(ev.deliveries, () => showEvent(id)),
    h("h3", {}, "Body"), h("pre", { class: "code" }, body),
    h("h3", {}, "Headers"), h("pre", { class: "code" }, headers || "(none)"));
}

function deliveryTimeline(ds, after) {
  if (!ds.length) return h("div", { class: "muted" }, "No deliveries.");
  return h("ul", { class: "timeline" }, ds.map((d) => h("li", {},
    statusLozenge(d.status),
    h("div", {}, h("div", { class: "url mono", title: d.target }, d.target),
      h("div", { class: "muted" }, `${d.mode} · attempt ${d.attempts}/${d.max_attempts}`,
        d.last_status ? ` · HTTP ${d.last_status}` : "", d.duration_ms ? ` · ${d.duration_ms} ms` : "",
        d.last_error ? " · " + d.last_error : "")),
    retryButton(d, after))));
}

function retryButton(d, after) {
  if (!isEditor() || (d.status !== "dead" && d.status !== "failed")) return h("span", {});
  return h("button", { class: "btn sm", onclick: async (e) => {
    e.stopPropagation();
    try { await api("POST", `/api/deliveries/${d.id}/retry`); toast("Delivery DL-" + d.id + " queued"); after && after(); }
    catch (err) { toast(err.message, true); }
  } }, icon("retry"), "Retry");
}

// ---------------------------------------------------------------- deliveries

async function pageDeliveries() {
  const initialFilters = legacyFilters({ route: exact("route"), status: exact("status") });
  const defaults = { view: "list" };
  const f = queryState(defaults);
  const routes = await routeOptions();
  const STATUSES = [["pending", "Pending"], ["inflight", "In flight"], ["delivered", "Delivered"], ["failed", "Failed (sync)"], ["dead", "Dead letter"]];
  const open = (d) => showEvent(d.event_id);
  let t = null;
  const reload = () => (t ? t.reload() : drawBoard());
  const cols = [
    { key: "id", label: "Key", num: true, sort: true, filter: "num", render: (d) => h("span", { class: "key" }, "DL-" + d.id) },
    { key: "status", label: "Status", sort: true, filter: STATUSES, render: (d) => statusLozenge(d.status) },
    { key: "route", label: "Route", sort: true, filter: routes.map((r) => [r, r]), render: (d) => h("span", { class: "cell-flex" }, avatar(d.route, true), h("span", {}, d.route)) },
    { key: "target", label: "Target", sort: true, filter: "text", render: (d) => h("span", { class: "mono", title: d.target }, d.target) },
    { key: "mode", label: "Mode", sort: true, filter: [["sync", "sync"], ["async", "async"]], render: (d) => lozenge(d.mode, d.mode === "sync" ? "" : "purple") },
    { key: "attempts", label: "Attempts", num: true, sort: true, filter: "num", render: (d) => `${d.attempts}/${d.max_attempts}` },
    { key: "last_status", label: "Last HTTP", num: true, sort: true, filter: "num", render: (d) => httpLozenge(d.last_status) },
    { key: "last_error", label: "Last error", sort: true, filter: "text", render: (d) => d.last_error ? h("span", { class: "muted", title: d.last_error }, d.last_error) : "" },
    { key: "created_at", label: "Created", time: true, sort: true, filter: "time", render: (d) => when(d.created_at) },
    { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (d) => when(d.updated_at) },
    { key: "next_at", label: "Next attempt", time: true, sort: true, filter: "time", render: (d) => d.status === "pending" ? when(d.next_at) : h("span", { class: "muted" }, "—") },
    { key: "", label: "", render: (d) => retryButton(d, reload) },
  ];
  const body = h("div", {});
  const drawBoard = async () => {
    const q = new URLSearchParams(location.search);
    const r = await api("GET", "/api/deliveries?limit=500" + (q.get("f.route") ? "&f.route=" + encodeURIComponent(q.get("f.route")) : ""));
    fill(body, h("div", { class: "board" }, STATUSES.map(([s, title]) => {
      const col = r.items.filter((d) => d.status === s);
      return h("section", { class: "column", "aria-label": title }, h("h3", {}, title, h("span", { class: "badge" }, String(col.length))),
        col.slice(0, 60).map((d) => {
          const c = h("div", { class: "bcard", tabindex: "0" },
            h("div", { class: "t", title: d.target }, d.target.replace(/^https?:\/\//, "")),
            h("div", { class: "m" }, avatar(d.route, true), h("span", {}, d.route), d.last_status ? httpLozenge(d.last_status) : null,
              h("span", { class: "key" }, "DL-" + d.id)),
            h("div", { class: "muted small" }, when(d.updated_at)),
            d.last_error && s !== "delivered" ? h("div", { class: "muted" }, d.last_error) : null);
          c.addEventListener("click", () => open(d));
          c.addEventListener("keydown", (e) => { if (e.key === "Enter") open(d); });
          return c;
        }));
    })));
  };
  if (f.view === "board") { await drawBoard(); }
  else {
    t = dataTable({ cols, initialFilters, defaultSort: ["created_at:desc"], pageSize: 25, fetch: serverList("/api/deliveries"), onRow: open, exportPath: "/api/deliveries", exportName: "deliveries",
      empty: ["No deliveries", "Deliveries are created for every event target."] });
    fill(body, t.el);
  }
  const routeFilter = () => { const v = new URLSearchParams(location.search).get("f.route") || ""; return v.startsWith("=") ? v.slice(1) : ""; };
  page("Deliveries", {
    desc: "Each event is sent to its targets with retries and exponential backoff; deliveries that give up become dead letters.",
    actions: [
      isEditor() ? h("button", { class: "btn", onclick: async () => {
        const rf = routeFilter();
        try { const r = await api("POST", "/api/deliveries/retry-dead" + (rf ? "?route=" + encodeURIComponent(rf) : "")); toast(r.queued + " deliveries queued"); reload(); }
        catch (e) { toast(e.message, true); }
      } }, icon("retry"), "Retry all dead") : null,
      h("button", { class: "btn", onclick: reload }, icon("refresh"), "Refresh"),
    ],
  },
    h("div", { class: "toolbar" },
      seg([["list", "List"], ["board", "Board"]], f.view, (v) => { f.view = v; pushQuery(f, defaults); route(); })),
    body);
  return {
    onLive(topic) {
      if (topic !== "deliveries") return;
      if (t && t.page !== 1) return;
      clearTimeout(this.timer); this.timer = setTimeout(reload, 800);
    },
  };
}

// ---------------------------------------------------------------- logs

const KINDS = ["proxy_http", "proxy_connect", "proxy_denied", "forward", "webhook", "ingest", "delivery", "auth", "admin", "config", "system", "app"];

async function pageLogs() {
  const initialFilters = legacyFilters({ kind: exact("kind"), route: exact("route"), client: exact("client"), host: exact("host"),
    source_ip: exact("source_ip"), status: (v) => ["status", v === "error" ? ">=400" : v], since: (v) => ["ts", v + ".."] });
  if (!Object.keys(initialFilters).length && !location.search.includes("f.ts")) initialFilters.ts = "1h..";
  const defaults = { view: "list", direction: "", level: "", q: "", by: "kind" };
  const f = queryState(defaults);
  const cols = [
    { key: "ts", label: "Time", time: true, sort: true, filter: "time", render: (l) => when(l.ts) },
    { key: "level", label: "Level", sort: true, filter: [["debug", "debug"], ["info", "info"], ["warn", "warn"], ["error", "error"]], render: (l) => lozenge(l.level, STATUS_KIND[l.level]) },
    { key: "direction", label: "Dir", sort: true, filter: [["inbound", "IN"], ["outbound", "OUT"]], render: (l) => dirTag(l.direction) },
    { key: "kind", label: "Kind", sort: true, filter: KINDS.map((k) => [k, k]), render: (l) => h("span", { class: "mono" }, l.kind) },
    { key: "route", label: "Route", sort: true, filter: "text", render: (l) => l.route || h("span", { class: "muted" }, "—") },
    { key: "client", label: "Client", sort: true, filter: "text", render: (l) => l.client ? h("span", { class: "cell-flex" }, avatar(l.client, true), h("span", {}, l.client)) : h("span", { class: "muted" }, "—") },
    { key: "method", label: "Method", sort: true, filter: "text", render: (l) => l.method || "" },
    { key: "status", label: "Status", num: true, sort: true, filter: "num", render: (l) => httpLozenge(l.status) },
    { key: "duration_ms", label: "ms", num: true, sort: true, filter: "num", render: (l) => l.duration_ms ? String(l.duration_ms) : "" },
    { key: "source_ip", label: "Source IP", sort: true, filter: "text", render: (l) => h("span", { class: "mono" }, l.source_ip || "") },
    { key: "host", label: "Host", sort: true, filter: "text", render: (l) => l.host || "" },
    { key: "msg", label: "URL / message", sort: true, filter: "text", render: (l) => h("span", { class: "cell-flex", title: l.url || l.msg },
      l.has_payload ? h("span", { class: "pl-flag", title: "Full request and response captured" }, "RQ/RS") : null, h("span", {}, l.url || l.msg)) },
  ];
  const showLog = async (l) => {
    const meta = props([["Time", fmtDT(l.ts)], ["Client", l.client || null], ["Route", l.route || null], ["Method", l.method || null],
      ["Host", l.host || null], ["URL", l.url ? h("span", { class: "mono" }, l.url) : null], ["Status", l.status ? httpLozenge(l.status) : null],
      ["Duration", l.duration_ms ? l.duration_ms + " ms" : null], ["Bytes in / out", (l.bytes_in || l.bytes_out) ? bytes(l.bytes_in) + " / " + bytes(l.bytes_out) : null],
      ["Source IP", l.source_ip || null], ["Trace", l.trace_id ? h("span", { class: "mono" }, l.trace_id) : null]]);
    const payloadEl = h("div", {}, l.has_payload ? h("div", { class: "muted" }, "Loading request and response…") : null);
    openDetail("#" + l.id, l.msg,
      h("div", { class: "toolbar" }, lozenge(l.level, STATUS_KIND[l.level]), dirTag(l.direction), h("span", { class: "mono" }, l.kind)),
      meta,
      l.attrs && l.attrs.event_id ? h("p", {}, link("/events?q=" + encodeURIComponent(l.attrs.event_id), "Open event " + l.attrs.event_id)) : null,
      payloadEl,
      l.attrs ? [h("h3", {}, "Attributes"), h("pre", { class: "code" }, JSON.stringify(l.attrs, null, 2))] : null);
    if (!l.has_payload) return;
    try {
      const d = await api("GET", "/api/logs/" + l.id);
      fill(payloadEl, payloadView(d.payload, d.masked, l));
    } catch (e) { fill(payloadEl, banner("danger", "Could not load the captured request", e.message)); }
  };
  const pageParams = () => {
    const q = new URLSearchParams();
    for (const k of ["direction", "level", "q"]) if (f[k]) q.set(k, f[k]);
    return q;
  };
  const body = h("div", {});
  let t = null;
  const drawSummary = async () => {
    const q = pageParams();
    for (const [k, v] of new URLSearchParams(location.search)) if (k.startsWith("f.")) q.set(k, v);
    for (const [k, v] of Object.entries(initialFilters)) if (!q.has("f." + k)) q.set("f." + k, v);
    q.set("by", f.by);
    const r = await api("GET", "/api/logs/summary?" + q);
    const rows = r.items, max = rows.length ? rows[0].count : 1;
    const st = dataTable({
      rows, pageSize: 25, defaultSort: ["count:desc"], exportName: "logs-summary-by-" + f.by,
      cols: [
        { key: "key", label: f.by, sort: true, filter: "text", render: (x) => h("div", {}, h("div", {}, x.key || "—"),
          (() => { const b = h("i", {}); b.style.width = Math.max(2, (x.count / max) * 100) + "%"; return h("div", { class: "bar" }, b); })()) },
        { key: "count", label: "Count", num: true, sort: true, filter: "num", render: (x) => String(x.count) },
        { key: "errors", label: "Errors", num: true, sort: true, filter: "num", render: (x) => x.errors ? lozenge(String(x.errors), "danger") : "0" },
        { key: "avg_ms", label: "Avg ms", num: true, sort: true, filter: "num", render: (x) => x.avg_ms.toFixed(0) },
        { key: "bytes_out", label: "Bytes out", num: true, sort: true, filter: "num", render: (x) => bytes(x.bytes_out) },
        { key: "last_seen", label: "Last seen", time: true, sort: true, filter: "time", render: (x) => when(x.last_seen) },
      ],
      onRow: (x) => {
        const col = cols.find((c) => c.key === f.by);
        if (!col || !col.filter) return;
        const q = new URLSearchParams(location.search);
        q.set("f." + f.by, col.filter === "num" ? String(x.key) : "=" + x.key); q.delete("view"); q.delete("by");
        history.replaceState(null, "", location.pathname + "?" + q); route();
      },
      empty: ["Nothing to summarize", "No log entries match these filters."],
    });
    fill(body, banner("info", "Summary of the rows matching the list's filters", "Switch to List to change filters; click a row to list its entries.", true), st.el);
  };
  if (f.view === "summary") await drawSummary();
  else {
    t = dataTable({ cols, initialFilters, defaultSort: ["ts:desc"], pageSize: 50, fetch: serverList("/api/logs", pageParams), onRow: showLog, exportPath: "/api/logs", exportName: "logs", exportParams: pageParams,
      empty: ["No log entries", "Try a wider time range or fewer filters."] });
    fill(body, t.el);
  }
  const reload = () => (t ? t.reload() : drawSummary());
  const setPage = (k, v) => { f[k] = v; pushQuery(f, defaults); reload(); };
  page("Logs", {
    desc: "Every proxy call, forward, webhook, ingest and delivery, stored in SQLite. Incoming calls are marked IN, outgoing calls OUT. Filter any column; Shift+click headers to sort by several.",
    actions: h("button", { class: "btn", onclick: reload }, icon("refresh"), "Refresh"),
  },
    h("div", { class: "toolbar" },
      seg([["list", "List"], ["summary", "Summary"]], f.view, (v) => { f.view = v; pushQuery(f, defaults); route(); }),
      seg([["", "All"], ["inbound", "↓ Incoming"], ["outbound", "↑ Outgoing"]], f.direction, (v) => setPage("direction", v)),
      select([["", "Any level"], ["info", "Info+"], ["warn", "Warn+"], ["error", "Error"]], f.level, (v) => setPage("level", v), "Minimum level"),
      h("input", { type: "search", placeholder: "Search message, URL, attributes…", value: f.q, "aria-label": "Search", onchange: (e) => setPage("q", e.target.value) }),
      f.view === "summary" ? select([["kind", "Group by kind"], ["direction", "Group by direction"], ["route", "Group by route"],
        ["client", "Group by client"], ["host", "Group by host"], ["status", "Group by status"], ["source_ip", "Group by source IP"],
        ["level", "Group by level"], ["method", "Group by method"]], f.by, (v) => setPage("by", v), "Group by") : null),
    body);
  return {
    onLive(topic) {
      if (topic !== "logs" || !t || t.page !== 1) return;
      clearTimeout(this.timer); this.timer = setTimeout(reload, 1000);
    },
  };
}

// ---------------------------------------------------------------- config resources (webhooks, ingest, profiles)

// Who may call a webhook / ingest rule: allowed_ips + source_cidrs, or the restricted default.
function senderList(o) { return [...(o.allowed_ips || []), ...(o.source_cidrs || [])]; }
function sendersCell(o) {
  const l = senderList(o);
  if (!l.length) return lozenge("private only", "warn");
  if (l.includes("*")) return lozenge("anyone", o.auth ? "" : "danger");
  return h("span", { class: "mono", title: l.join("\n") }, l.length > 2 ? l.slice(0, 2).join(", ") + " +" + (l.length - 2) : l.join(", "));
}
function sendersView(o) {
  const l = senderList(o);
  if (!l.length) return h("span", {}, lozenge("private only", "warn"), " loopback and private networks (restricted default)");
  return h("div", { class: "rule-list" }, l.map((r) => h("code", { class: "rule" }, r)));
}
const IP_HELP = "One per line: * (anyone), an IP, a CIDR (52.66.0.0/16) or an IPv4 wildcard (52.66.*.*, 52.66.*, 52.66.1-5.*).";
const ROUTE_FIELDS = [
  { legend: "Who can call it", fields: [
    { k: "allowed_ips", label: "Allowed IPs", type: "list", help: IP_HELP + " Empty: loopback and private networks only (restricted default), so internet senders get 403." },
    { k: "source_cidrs", label: "Source CIDRs (same as allowed IPs)", type: "list", help: "Kept for existing files; combined with Allowed IPs." },
  ] },
  { legend: "Inbound checks", fields: [
    { k: "methods", label: "Allowed methods", type: "list", help: "One per line. Default POST; * allows any." },
    { k: "rate_limit.requests_per_minute", label: "Rate limit (requests / minute / IP)", type: "number", help: "Empty uses the global limit; 0 disables." },
    { k: "rate_limit.burst", label: "Rate limit burst", type: "number" },
  ] },
  { legend: "Authentication (optional)", fields: [
    { k: "auth.token.header", label: "Token header", type: "text", placeholder: "X-Callback-Token" },
    { k: "auth.token.value", label: "Token value", type: "secret" },
    { k: "auth.basic.username", label: "Basic auth username", type: "text" },
    { k: "auth.basic.password", label: "Basic auth password", type: "secret" },
    { k: "auth.hmac.header", label: "HMAC signature header", type: "text", placeholder: "X-Razorpay-Signature" },
    { k: "auth.hmac.secret", label: "HMAC secret", type: "secret" },
    { k: "auth.hmac.algorithm", label: "HMAC algorithm", type: "select", options: [["", "sha256 (default)"], ["sha1", "sha1"], ["sha512", "sha512"]] },
    { k: "auth.hmac.encoding", label: "Signature encoding", type: "select", options: [["", "hex (default)"], ["base64", "base64"]] },
    { k: "auth.hmac.prefix", label: "Signature prefix", type: "text", placeholder: "sha256=" },
  ] },
  { legend: "Delivery", fields: [
    { k: "target_auth.type", label: "Credentials sent to the target", type: "select", options: [["", "None"], ["bearer", "Bearer token"], ["basic", "Basic auth"], ["header", "Custom header"]] },
    { k: "target_auth.token", label: "Bearer token", type: "secret" },
    { k: "target_auth.username", label: "Basic username", type: "text" },
    { k: "target_auth.password", label: "Basic password", type: "secret" },
    { k: "target_auth.header", label: "Header name", type: "text" },
    { k: "target_auth.value", label: "Header value", type: "secret" },
    { k: "add_headers", label: "Extra headers", type: "kv", help: "One per line: Header-Name: value" },
    { k: "max_retries", label: "Max retries", type: "number", help: "Default 0 for sync, 8 for async." },
    { k: "timeout_sec", label: "Timeout per attempt (seconds)", type: "number" },
  ] },
];

const RES = {
  webhooks: {
    path: "/webhooks", api: "/api/webhooks", idKey: "id", title: "Webhooks", singular: "webhook", icon: "hook",
    desc: "Public endpoints at /hooks/<id> relayed to your targets. Sync webhooks pass the target's answer back; async ones reply 202 once stored.",
    template: { id: "", target: "https://", async: false, methods: ["POST"] },
    fields: [{ legend: "Webhook", fields: [
      { k: "id", label: "ID", type: "text", create: true, help: "Lowercase letters, digits, - and _. Public URL: /hooks/<id>" },
      { k: "description", label: "Description", type: "text" },
      { k: "target", label: "Target URL", type: "text", placeholder: "https://dev.internal/callback" },
      { k: "mirrors", label: "Mirror URLs", type: "list", help: "Extra targets, always delivered durably in the background." },
      { k: "async", label: "Async: reply 202 once stored and deliver in the background", type: "bool" },
      { k: "disabled", label: "Disabled (answer 404)", type: "bool" },
    ] }, ...ROUTE_FIELDS],
    cols: [
      { key: "id", label: "ID", sort: true, filter: "text", render: (w) => h("span", { class: "cell-flex" }, avatar(w.id, true), h("span", { class: "key" }, w.id)) },
      { key: "status", label: "Status", v: (w) => w.disabled ? "disabled" : "active", sort: true, filter: [["active", "active"], ["disabled", "disabled"]], render: (w) => w.disabled ? lozenge("disabled") : lozenge("active", "success") },
      { key: "mode", label: "Mode", v: (w) => w.async ? "async" : "sync", sort: true, filter: [["sync", "sync"], ["async", "async"]], render: (w) => lozenge(w.async ? "async" : "sync", w.async ? "purple" : "") },
      { key: "target", label: "Target", sort: true, filter: "text", render: (w) => h("span", { class: "mono", title: w.target }, w.target) },
      { key: "mirrors", label: "Mirrors", num: true, v: (w) => (w.mirrors || []).length, sort: true, filter: "num", render: (w) => String((w.mirrors || []).length) },
      { key: "allowed_ips", label: "Allowed IPs", v: sendersText, sort: true, filter: "text", render: (w) => sendersCell(w) },
      { key: "auth", label: "Auth", v: authText, sort: true, filter: "text", render: (w) => authSummary(w) },
      { key: "description", label: "Description", sort: true, filter: "text", render: (w) => w.description || "" },
      { key: "created_at", label: "Created", time: true, sort: true, filter: "time", render: (w) => when(w.created_at) },
      { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (w) => when(w.updated_at) },
    ],
    detail: (w) => [
      props([["Public path", h("span", { class: "mono" }, "/hooks/" + w.id)], ["Target", h("span", { class: "mono" }, w.target)],
        ["Mirrors", (w.mirrors || []).join("\n") || null], ["Mode", w.async ? "async (202, durable)" : "sync (passthrough)"],
        ["Methods", (w.methods || []).join(", ")], ["Allowed IPs", sendersView(w)],
        ["Inbound auth", authSummary(w)], ["Target auth", w.target_auth ? w.target_auth.type : "none"],
        ["Retries", String(w.max_retries)], ["Timeout", w.timeout_sec + " s"], ["Description", w.description || null]]),
    ],
    related: (w) => "/deliveries?route=" + encodeURIComponent(w.id),
  },
  ingest: {
    path: "/ingest", api: "/api/ingest", idKey: "id", title: "Ingest rules", singular: "ingest rule", icon: "route",
    desc: "Callbacks on any other path are matched against these rules (highest priority first), stored, acknowledged, then delivered to every target.",
    template: { id: "", match: { type: "suffix", value: "/v1/" }, targets: ["https://"], vendor: "", event_type: "" },
    fields: [{ legend: "Rule", fields: [
      { k: "id", label: "ID", type: "text", create: true },
      { k: "description", label: "Description", type: "text" },
      { k: "priority", label: "Priority", type: "number", help: "Higher is tried first." },
      { k: "match.type", label: "Match type", type: "select", options: [["exact", "Exact path"], ["prefix", "Path prefix"], ["suffix", "Path suffix"], ["regex", "Regular expression"]] },
      { k: "match.value", label: "Match value", type: "text", placeholder: "/v1/apppaystatus" },
      { k: "vendor", label: "Vendor label", type: "text", placeholder: "NSE" },
      { k: "event_type", label: "Event type label", type: "text", placeholder: "APP_PAY_STATUS" },
      { k: "targets", label: "Targets", type: "list", help: "One URL per line." },
      { k: "disabled", label: "Disabled", type: "bool" },
    ] }, { legend: "Acknowledgement", fields: [
      { k: "response.status", label: "Status code", type: "number" },
      { k: "response.content_type", label: "Content type", type: "text" },
      { k: "response.body", label: "Body", type: "textarea" },
    ] }, ...ROUTE_FIELDS],
    cols: [
      { key: "id", label: "ID", sort: true, filter: "text", render: (r) => h("span", { class: "cell-flex" }, avatar(r.id, true), h("span", { class: "key" }, r.id)) },
      { key: "status", label: "Status", v: (r) => r.disabled ? "disabled" : "active", sort: true, filter: [["active", "active"], ["disabled", "disabled"]], render: (r) => r.disabled ? lozenge("disabled") : lozenge("active", "success") },
      { key: "priority", label: "Priority", num: true, v: (r) => r.priority || 0, sort: true, filter: "num", render: (r) => String(r.priority || 0) },
      { key: "match", label: "Match", v: (r) => r.match.type + " " + r.match.value, sort: true, filter: "text", render: (r) => h("span", {}, lozenge(r.match.type), " ", h("span", { class: "mono" }, r.match.value)) },
      { key: "vendor", label: "Vendor", sort: true, filter: "text", render: (r) => r.vendor || "" },
      { key: "event_type", label: "Event type", sort: true, filter: "text", render: (r) => r.event_type || "" },
      { key: "targets", label: "Targets", num: true, v: (r) => r.targets.length, csv: (r) => r.targets.join(" "), sort: true, filter: "num", render: (r) => String(r.targets.length) },
      { key: "allowed_ips", label: "Allowed IPs", v: sendersText, sort: true, filter: "text", render: (r) => sendersCell(r) },
      { key: "auth", label: "Auth", v: authText, sort: true, filter: "text", render: (r) => authSummary(r) },
      { key: "created_at", label: "Created", time: true, sort: true, filter: "time", render: (r) => when(r.created_at) },
      { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (r) => when(r.updated_at) },
    ],
    detail: (r) => [props([["Match", r.match.type + " " + r.match.value], ["Priority", String(r.priority || 0)], ["Vendor", r.vendor || null],
      ["Event type", r.event_type || null], ["Targets", r.targets.join("\n")], ["Ack", `${r.response.status} ${r.response.content_type}`],
      ["Ack body", h("span", { class: "mono" }, r.response.body)], ["Allowed IPs", sendersView(r)],
      ["Inbound auth", authSummary(r)], ["Retries", String(r.max_retries)]])],
    related: (r) => "/events?route=" + encodeURIComponent(r.id),
  },
  profiles: {
    path: "/profiles", api: "/api/profiles", idKey: "name", title: "Forward profiles", singular: "profile", icon: "send",
    desc: "Named upstreams for POST /forward. The agent adds the profile's headers (usually provider credentials), so clients never hold them.",
    template: { name: "", base_url: "https://", headers: {} },
    fields: [{ legend: "Profile", fields: [
      { k: "name", label: "Name", type: "text", create: true },
      { k: "description", label: "Description", type: "text" },
      { k: "base_url", label: "Base URL", type: "text", placeholder: "https://api.razorpay.com/v1" },
      { k: "headers", label: "Headers added to every request", type: "kv", secret: true, help: "One per line: Header-Name: value. Values are write-only." },
      { k: "allowed_methods", label: "Allowed methods", type: "list", help: "Empty allows any." },
      { k: "clients", label: "Allowed clients", type: "list", help: "Client names; empty allows every client." },
      { k: "timeout_sec", label: "Timeout (seconds)", type: "number" },
    ] }],
    cols: [
      { key: "name", label: "Name", sort: true, filter: "text", render: (p) => h("span", { class: "cell-flex" }, avatar(p.name, true), h("span", { class: "key" }, p.name)) },
      { key: "base_url", label: "Base URL", sort: true, filter: "text", render: (p) => h("span", { class: "mono" }, p.base_url) },
      { key: "headers", label: "Injected headers", v: (p) => Object.keys(p.headers || {}).join(", "), sort: true, filter: "text", render: (p) => Object.keys(p.headers || {}).join(", ") || h("span", { class: "muted" }, "none") },
      { key: "clients", label: "Clients", v: (p) => (p.clients || []).join(", ") || "all", sort: true, filter: "text", render: (p) => (p.clients || []).join(", ") || "all" },
      { key: "allowed_methods", label: "Methods", v: (p) => (p.allowed_methods || []).join(", ") || "any", sort: true, filter: "text", render: (p) => (p.allowed_methods || []).join(", ") || "any" },
      { key: "created_at", label: "Created", time: true, sort: true, filter: "time", render: (p) => when(p.created_at) },
      { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (p) => when(p.updated_at) },
    ],
    detail: (p) => [props([["Base URL", h("span", { class: "mono" }, p.base_url)], ["Headers", Object.keys(p.headers || {}).join(", ") || "none"],
      ["Clients", (p.clients || []).join(", ") || "all"], ["Methods", (p.allowed_methods || []).join(", ") || "any"],
      ["Timeout", p.timeout_sec ? p.timeout_sec + " s" : "default"], ["Description", p.description || null]]),
      h("h3", {}, "Usage"), h("pre", { class: "code" }, `curl -X POST http://<agent>:8080/forward \\\n  -H "X-Agent-Key: <client token>" \\\n  -d '{"profile":"${p.name}","method":"GET","path":"/..."}'`)],
    related: (p) => "/logs?kind=forward&route=" + encodeURIComponent(p.name),
  },
};

function authText(o) { const a = o.auth || {}; return [a.token && "token", a.basic && "basic", a.hmac && "hmac"].filter(Boolean).join(", ") || "none"; }
function sendersText(o) { const l = senderList(o); return l.length ? l.join(", ") : "private only (default)"; }
function authSummary(o) {
  const a = o.auth || {};
  const parts = [a.token && "token", a.basic && "basic", a.hmac && "hmac"].filter(Boolean);
  return parts.length ? parts.map((p) => lozenge(p, "success")) : h("span", { class: "muted" }, "none");
}

async function pageResources(res) {
  const items = await api("GET", res.api);
  const p = page(res.title, {
    desc: res.desc,
    actions: isEditor() ? h("button", { class: "btn primary", onclick: () => editResource(res, null) }, icon("plus"), "Create " + res.singular) : null,
  }, viewerNote(),
    dataTable({ cols: res.cols, rows: items, onRow: (it) => showResource(res, it), defaultSort: [res.idKey + ":asc"], exportName: res.path.slice(1),
      empty: ["No " + res.title.toLowerCase() + " yet", "They are stored as JSON files in the dynamic directory."] }).el);
  const want = new URLSearchParams(location.search).get("id"); // e.g. from the Access page
  const it = want && items.find((x) => x[res.idKey] === want);
  if (it) setTimeout(() => showResource(res, it), 0);
  return { el: p };
}

function showResource(res, it) {
  const id = it[res.idKey];
  openDetail(res.title.slice(0, -1) + " / " + id, id,
    h("div", { class: "toolbar" },
      isEditor() ? h("button", { class: "btn primary", onclick: () => editResource(res, it) }, icon("edit"), "Edit") : null,
      res.related ? link(res.related(it), h("span", { class: "btn" }, "View activity")) : null,
      isEditor() ? h("button", { class: "btn subtle", onclick: () => deleteResource(res, id) }, icon("trash"), "Delete") : null),
    res.detail(it),
    h("h3", {}, "File"), h("pre", { class: "code" }, JSON.stringify(it, null, 2)));
}

async function deleteResource(res, id) {
  modal("Delete " + res.singular + " " + id + "?",
    h("p", {}, "This removes the JSON file from the dynamic directory. Stored events and logs are kept."),
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn danger", onclick: async () => {
        try { await api("DELETE", res.api + "/" + encodeURIComponent(id)); closeModal(); closeDetail(); toast("Deleted " + id); route(); }
        catch (e) { toast(e.message, true); }
      } }, "Delete")]);
}

// ---------------- generic form <-> object editing

function getPath(o, path) { return path.split(".").reduce((a, k) => (a == null ? undefined : a[k]), o); }
function setPath(o, path, v) {
  const ks = path.split(".");
  let cur = o;
  for (const k of ks.slice(0, -1)) {
    if (typeof cur[k] !== "object" || cur[k] === null) cur[k] = {};
    cur = cur[k];
  }
  cur[ks[ks.length - 1]] = v;
}
// prune drops empty strings, arrays, objects and false, so unset form fields mean "default".
function prune(v) {
  if (Array.isArray(v)) return v.length ? v : undefined;
  if (v && typeof v === "object") {
    const out = {};
    for (const [k, x] of Object.entries(v)) {
      const p = prune(x);
      if (p !== undefined) out[k] = p;
    }
    return Object.keys(out).length ? out : undefined;
  }
  if (v === "" || v === false || v === null || (typeof v === "number" && isNaN(v))) return undefined;
  return v;
}

function fieldInput(f, obj, isNew) {
  const v = getPath(obj, f.k);
  const set = (x) => setPath(obj, f.k, x);
  const disabled = f.create && !isNew;
  let input;
  switch (f.type) {
    case "bool":
      return h("label", { class: "check" }, h("input", { type: "checkbox", checked: !!v, onchange: (e) => set(e.target.checked) }), f.label);
    case "number":
      input = h("input", { type: "number", value: v ?? "", onchange: (e) => set(e.target.value === "" ? "" : Number(e.target.value)) });
      break;
    case "list":
      input = h("textarea", { rows: "3", onchange: (e) => set(e.target.value.split("\n").map((s) => s.trim()).filter(Boolean)) });
      input.value = (v || []).join("\n");
      break;
    case "kv":
      input = h("textarea", { rows: "3", onchange: (e) => {
        const m = {};
        for (const line of e.target.value.split("\n")) {
          const i = line.indexOf(":");
          if (i > 0) m[line.slice(0, i).trim()] = line.slice(i + 1).trim();
        }
        set(m);
      } });
      input.value = Object.entries(v || {}).map(([k, x]) => k + ": " + x).join("\n");
      break;
    case "textarea":
      input = h("textarea", { rows: "3", onchange: (e) => set(e.target.value) });
      input.value = v ?? "";
      break;
    case "select":
      input = select(f.options, v ?? "", set, f.label);
      break;
    case "secret":
      input = h("input", { type: "password", autocomplete: "new-password", value: v ?? "", placeholder: v === MASK ? "unchanged" : "",
        onchange: (e) => set(e.target.value) });
      break;
    default:
      input = h("input", { type: "text", value: v ?? "", placeholder: f.placeholder || "", disabled: disabled || undefined,
        onchange: (e) => set(e.target.value) });
  }
  return h("label", { class: "field" }, h("span", {}, f.label), input,
    f.help ? h("div", { class: "help" }, f.help) : null,
    f.type === "secret" && v === MASK ? h("div", { class: "secret-note" }, "Stored secret is hidden. Leave as is to keep it.") : null);
}

function editResource(res, existing) {
  const isNew = !existing;
  let obj = JSON.parse(JSON.stringify(existing || res.template));
  for (const k of ["created_at", "updated_at", "$schema"]) delete obj[k];
  let tab = "form";
  const err = h("div", { class: "form-error hidden" });
  const body = h("div", {});
  const jsonArea = h("textarea", { rows: "22", spellcheck: "false", "aria-label": "JSON" });
  const drawForm = () => fill(body, ...res.fields.map((g) =>
    h("fieldset", {}, h("legend", {}, g.legend), g.fields.map((f) => fieldInput(f, obj, isNew)))));
  const tabs = h("div", { class: "tabs", role: "tablist" });
  const showTab = (t) => {
    if (t === "json") {
      jsonArea.value = JSON.stringify(prune(obj) || {}, null, 2);
      fill(body, jsonArea, h("div", { class: "help" },
        "Full object as stored in the file. Validated against ", h("a", { href: BASE + "/api/schema/" + (res === RES.ingest ? "ingest" : res === RES.profiles ? "profile" : "webhook") + ".schema.json", target: "_blank", rel: "noopener" }, "the JSON Schema"), " on save."));
    } else {
      if (tab === "json") {
        try { obj = JSON.parse(jsonArea.value || "{}"); } catch (e) { showErr("JSON: " + e.message); return; }
      }
      drawForm();
    }
    tab = t;
    for (const b of tabs.children) b.classList.toggle("on", b.dataset.tab === t);
  };
  const showErr = (m) => { err.textContent = m; err.classList.remove("hidden"); };
  tabs.append(h("button", { "data-tab": "form", class: "on", onclick: () => showTab("form") }, "Form"),
    h("button", { "data-tab": "json", onclick: () => showTab("json") }, "JSON"));
  drawForm();
  const save = async () => {
    err.classList.add("hidden");
    let payload;
    if (tab === "json") {
      try { payload = JSON.parse(jsonArea.value); } catch (e) { showErr("JSON: " + e.message); return; }
    } else payload = prune(obj) || {};
    try {
      if (isNew) await api("POST", res.api, payload);
      else await api("PUT", res.api + "/" + encodeURIComponent(existing[res.idKey]), payload);
      closeModal();
      toast((isNew ? "Created " : "Saved ") + (payload[res.idKey] || existing[res.idKey]));
      route();
    } catch (e) { showErr(e.message); }
  };
  modal((isNew ? "Create " : "Edit ") + res.singular + (isNew ? "" : " " + existing[res.idKey]),
    [tabs, err, body],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"), h("button", { class: "btn primary", onclick: save }, isNew ? "Create" : "Save")]);
}

function modal(title, body, foot) {
  closeModal();
  const ov = h("div", { class: "overlay", role: "dialog", "aria-modal": "true", "aria-label": title },
    h("div", { class: "modal" },
      h("div", { class: "modal-head" }, h("h2", {}, title), h("button", { class: "btn subtle", "aria-label": "Close", onclick: closeModal }, icon("close"))),
      h("div", { class: "modal-body" }, body),
      h("div", { class: "modal-foot" }, foot)));
  ov.addEventListener("mousedown", (e) => { if (e.target === ov) closeModal(); });
  document.body.append(ov);
  const first = ov.querySelector("input:not([disabled]), textarea, select");
  (first || ov.querySelector("button")).focus();
}
function closeModal() { document.querySelector(".overlay")?.remove(); }

// ---------------------------------------------------------------- proxy & clients

async function pageProxy() {
  const p = await api("GET", "/api/proxy");
  const clientCols = [
    { key: "name", label: "Client", sort: true, filter: "text", render: (c) => h("span", { class: "cell-flex" }, avatar(c.name, true), h("span", {}, c.name)) },
    { key: "status", label: "Status", v: (c) => c.disabled ? "disabled" : "active", sort: true, filter: [["active", "active"], ["disabled", "disabled"]], render: (c) => c.disabled ? lozenge("disabled") : lozenge("active", "success") },
    { key: "token", label: "Token", render: (c) => h("span", { class: "mono" }, c.token) },
    { key: "created_at", label: "Created", time: true, sort: true, filter: "time", render: (c) => when(c.created_at) },
    { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (c) => when(c.updated_at) },
    { label: "", render: (c) => isEditor() ? h("span", { class: "cell-flex" },
      h("button", { class: "btn sm", onclick: (e) => { e.stopPropagation(); rotate(c.name); } }, "Rotate"),
      h("button", { class: "btn sm", onclick: async (e) => { e.stopPropagation(); await clientCall("PATCH", c.name, { disabled: !c.disabled }); } }, c.disabled ? "Enable" : "Disable"),
      h("button", { class: "btn sm subtle", onclick: (e) => { e.stopPropagation(); removeClient(c.name); } }, "Delete")) : "" },
  ];
  const showToken = (name, token) => modal("Token for " + name,
    [h("p", {}, "Copy this token now — it will not be shown again."), h("pre", { class: "code" }, token),
      h("p", { class: "help" }, "Use it as the proxy password (http://any:<token>@agent:8080) or the X-Agent-Key header for /forward.")],
    [h("button", { class: "btn", onclick: () => navigator.clipboard?.writeText(token).then(() => toast("Copied")) }, "Copy"),
      h("button", { class: "btn primary", onclick: () => { closeModal(); route(); } }, "Done")]);
  const clientCall = async (method, name, body) => {
    try { const r = await api(method, "/api/proxy/clients/" + encodeURIComponent(name), body); route(); return r; } catch (e) { toast(e.message, true); }
  };
  const rotate = async (name) => {
    try { const r = await api("POST", "/api/proxy/clients/" + encodeURIComponent(name) + "/rotate"); showToken(name, r.token); } catch (e) { toast(e.message, true); }
  };
  const removeClient = (name) => modal("Delete client " + name + "?", h("p", {}, "Requests using its token will be refused immediately."),
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn danger", onclick: async () => { closeModal(); await clientCall("DELETE", name); } }, "Delete")]);
  const createClient = () => {
    const name = h("input", { type: "text", placeholder: "dev-laptop" });
    modal("Create client", [h("label", { class: "field" }, h("span", {}, "Name"), name,
      h("div", { class: "help" }, "A token is generated for you."))],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn primary", onclick: async () => {
        try { const r = await api("POST", "/api/proxy/clients", { name: name.value.trim() }); showToken(r.name, r.token); }
        catch (e) { toast(e.message, true); }
      } }, "Create")]);
  };
  const editRules = () => {
    const obj = { allowed_hosts: p.allowed_hosts, allowed_ports: p.allowed_ports, allow_private: p.allow_private, private_allowlist: p.private_allowlist };
    const err = h("div", { class: "form-error hidden" });
    const fields = [
      { k: "allowed_hosts", label: "Allowed hosts", type: "list", help: "Exact host or *.domain, one per line." },
      { k: "allowed_ports", label: "Allowed ports", type: "list", help: "One per line." },
      { k: "allow_private", label: "Allow private / loopback / metadata destinations", type: "bool" },
      { k: "private_allowlist", label: "Allowed private CIDRs", type: "list" },
    ];
    modal("Edit destination rules", [err, ...fields.map((f) => fieldInput(f, obj, false))],
      [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
        h("button", { class: "btn primary", onclick: async () => {
          const body = { ...obj, allowed_ports: (obj.allowed_ports || []).map(Number) };
          try { await api("PUT", "/api/proxy", body); closeModal(); toast("Saved"); route(); }
          catch (e) { err.textContent = e.message; err.classList.remove("hidden"); }
        } }, "Save")]);
  };
  const el = page("Proxy & clients", {
    desc: "Outbound traffic (HTTP/CONNECT proxy and /forward) may only reach these destinations, using one of these client tokens.",
    actions: isEditor() ? [h("button", { class: "btn", onclick: editRules }, icon("edit"), "Edit rules"),
      h("button", { class: "btn primary", onclick: createClient }, icon("plus"), "Create client")] : null,
  }, viewerNote(),
    h("div", { class: "grid-2" },
      h("div", { class: "panel" }, h("h2", {}, "Allowed destinations"),
        props([["Hosts", p.allowed_hosts.length ? h("div", {}, p.allowed_hosts.map((x) => h("div", { class: "mono" }, x))) : h("span", { class: "muted" }, "none — all requests are denied")],
          ["Ports", p.allowed_ports.join(", ")],
          ["Private addresses", p.allow_private ? lozenge("allowed", "danger") : lozenge("blocked", "success")],
          ["Private allowlist", p.private_allowlist.join(", ") || null]])),
      h("div", { class: "panel" }, h("h2", {}, "How to connect"),
        h("pre", { class: "code" }, "export HTTPS_PROXY=http://any:<TOKEN>@<agent>:8080\n\n# or the JSON relay\ncurl -X POST http://<agent>:8080/forward \\\n  -H 'X-Agent-Key: <TOKEN>' \\\n  -d '{\"method\":\"GET\",\"url\":\"https://api.example.com/x\"}'"))),
    h("h2", {}, "Clients"),
    dataTable({ id: "clients", cols: clientCols, rows: p.clients, defaultSort: ["name:asc"], exportName: "proxy-clients",
      empty: ["No clients", "Create one to let applications use the proxy."] }).el);
  return { el };
}

// ---------------------------------------------------------------- access

async function pageAccess() {
  const a = await api("GET", "/api/access");
  const isDefault = (l) => l.length === a.default.length && l.every((x, i) => x === a.default[i]);
  const status = (l) => {
    if (!l.length) return lozenge("Nobody", "danger");
    if (l.includes("*")) return lozenge("Everyone", "danger");
    return isDefault(l) ? lozenge("Restricted default", "success") : lozenge("Restricted", "success");
  };
  const lists = [
    { k: "proxy", title: "Forward proxy & /forward", what: "Applications that send outbound traffic through this agent." },
    { k: "admin", title: "Console & API", what: "People and tools using this console, the API and /metrics." },
  ];
  const rules = (l) => h("div", { class: "rule-list" }, l.map((r) => h("code", { class: "rule" }, r)));
  const panel = (l) => h("div", { class: "panel" },
    h("div", { class: "panel-head" }, h("h2", {}, l.title), status(a[l.k])),
    h("p", { class: "help" }, l.what),
    a[l.k].length ? rules(a[l.k]) : h("p", { class: "muted" }, "Empty list: every client is refused."),
    l.k === "admin" ? h("p", { class: "help" }, icon("lock"), " File-only: change it in access.json in the dynamic config directory, so nobody can lock the console or open it to the internet from here.") : null);

  const routeCols = [
    { key: "source", label: "Type", sort: true, filter: [["webhook", "webhook"], ["ingest", "ingest"]], render: (r) => lozenge(r.source) },
    { key: "id", label: "Route", sort: true, filter: "text", render: (r) => h("span", { class: "key" }, r.id) },
    { key: "allowed_ips", label: "Who can call it", v: (r) => r.default ? "private only (default)" : r.allowed_ips.join(", "), sort: true, filter: "text",
      render: (r) => r.default ? h("span", {}, lozenge("private only", "warn"), " restricted default")
        : r.allowed_ips.includes("*") ? lozenge("anyone", r.auth ? "" : "danger") : rules(r.allowed_ips) },
    { key: "auth", label: "Auth", v: (r) => r.auth ? "required" : "none", sort: true, filter: [["required", "required"], ["none", "none"]], render: (r) => r.auth ? lozenge("required", "success") : lozenge("none") },
    { key: "status", label: "Status", v: (r) => r.disabled ? "disabled" : "active", sort: true, filter: [["active", "active"], ["disabled", "disabled"]], render: (r) => r.disabled ? lozenge("disabled") : lozenge("active", "success") },
    { key: "updated_at", label: "Updated", time: true, sort: true, filter: "time", render: (r) => when(r.updated_at) },
  ];
  const openRoute = (r) => navigate((r.source === "webhook" ? "/webhooks" : "/ingest") + "?id=" + encodeURIComponent(r.id));

  const notes = a.notes.filter((n) => n.level !== "info");
  const ipIn = h("input", { type: "text", placeholder: "e.g. 52.66.10.20", value: a.client_ip, "aria-label": "IP address to test" });
  const out = h("div", { class: "tester-out" });
  const verdict = (v, label) => h("div", { class: "tester-row" },
    v.allowed ? lozenge("allowed", "success") : lozenge("refused", "danger"), h("span", {}, label),
    v.rule ? h("span", { class: "muted" }, "matched ", h("code", {}, v.rule)) : null);
  const test = async () => {
    try {
      const r = await api("POST", "/api/access/test", { ip: ipIn.value.trim() });
      fill(out, lists.map((l) => verdict(r[l.k], l.title)),
        r.routes.map((v) => verdict(v, (v.source === "webhook" ? "webhook " : "ingest rule ") + v.id)));
    } catch (e) { fill(out, h("div", { class: "form-error" }, e.message)); }
  };
  ipIn.addEventListener("keydown", (e) => { if (e.key === "Enter") test(); });

  const edit = () => {
    const obj = { proxy: a.proxy };
    const err = h("div", { class: "form-error hidden" });
    modal("Edit proxy allowlist", [err,
      fieldInput({ k: "proxy", label: "Forward proxy & /forward", type: "list", help: IP_HELP + " Empty refuses everyone." }, obj, false),
      h("p", { class: "help" }, "The console & API list is file-only. Webhooks and ingest rules have their own Allowed IPs in their edit form.")],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn subtle", onclick: () => save({ proxy: [...a.default] }) }, "Reset to restricted default"),
      h("button", { class: "btn primary", onclick: () => save(obj, err) }, "Save")]);
  };
  const save = async (obj, err) => {
    try { await api("PUT", "/api/access", obj); closeModal(); toast("Saved"); route(); }
    catch (e) { if (err) { err.textContent = e.message; err.classList.remove("hidden"); } else toast(e.message, true); }
  };

  const el = page("Access", {
    desc: "Who may call each webhook and ingest rule (its Allowed IPs), and who may use the proxy and this console. Anything else gets 403 before authentication, and the refusal is logged.",
    actions: isEditor() ? [h("button", { class: "btn primary", onclick: edit }, icon("edit"), "Edit proxy allowlist")] : null,
  }, viewerNote(),
    notes.map((n) => banner(n.level === "error" ? "danger" : "warn", n.level === "error" ? "Error" : "Warning", n.message, true)),
    banner("info", "You are connecting from " + a.client_ip,
      a.trusted_proxies.length ? "Addresses are taken from X-Forwarded-For when the request comes through a trusted proxy (" + a.trusted_proxies.join(", ") + ")."
        : "No reverse proxy is trusted, so the connecting address is what is checked.", true),
    h("h2", {}, "Webhooks & ingest rules"),
    h("p", { class: "help" }, "Each route has its own Allowed IPs (allowed_ips, or source_cidrs, in its file). Without any, only loopback and private networks may call it."),
    dataTable({ id: "routes", cols: routeCols, rows: a.routes, onRow: openRoute, pageSize: 10, defaultSort: ["source:desc", "id:asc"], exportName: "route-access",
      empty: ["No webhooks or ingest rules", "Create one to set who can call it."] }).el,
    h("div", { class: "grid-2 mt" }, lists.map(panel)),
    h("div", { class: "grid-2 mt" },
      h("div", { class: "panel" }, h("h2", {}, "Test an address"),
        h("div", { class: "tester" }, ipIn, h("button", { class: "btn", onclick: test }, "Check")), out),
      h("div", { class: "panel" }, h("h2", {}, "Rule syntax"),
        props([["*", "anyone"], ["203.0.113.7", "one address (IPv4 or IPv6)"], ["10.0.0.0/8", "CIDR (IPv4 or IPv6)"],
          ["52.66.*.*  or  52.66.*", "IPv4 wildcard octets"], ["192.168.1-5.*", "IPv4 octet range"],
          ["(none)", "restricted default: " + a.default.join(", ")]]))));
  test();
  return { el };
}

// ---------------------------------------------------------------- system

async function pageSystem() {
  const st = await api("GET", "/api/status");
  const errs = Object.entries(st.config.errors || {});
  const up = st.uptime_sec;
  const el = page("Status", { desc: "Runtime state of this agent." },
    errs.length ? banner("danger", "Invalid config files (previous version still in use)", errs.map(([f, e]) => f + ": " + e).join("\n"))
      : banner("info", "All config files are valid", "Loaded " + new Date(st.config.loaded_at).toLocaleString() + " from " + st.config.dir),
    h("div", { class: "cards" },
      h("div", { class: "card" }, h("div", { class: "label" }, "Version"), h("div", { class: "value" }, st.version)),
      h("div", { class: "card" }, h("div", { class: "label" }, "Uptime"), h("div", { class: "value" },
        up > 86400 ? Math.floor(up / 86400) + "d" : up > 3600 ? Math.floor(up / 3600) + "h" : Math.floor(up / 60) + "m")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Database"), h("div", { class: "value" }, bytes(st.database.size_bytes)),
        h("div", { class: "sub" }, "retention " + st.database.retention_days + " days")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Dropped log rows"), h("div", { class: "value" }, String(st.logs_dropped)))),
    h("div", { class: "grid-2" },
      h("div", { class: "panel" }, h("h2", {}, "Configuration"), props([
        ["Webhooks", String(st.config.webhooks)], ["Ingest rules", String(st.config.ingest_rules)], ["Profiles", String(st.config.profiles)],
        ["Clients", String(st.config.clients)], ["Allowed hosts", String(st.config.allowed_hosts)], ["Directory", h("span", { class: "mono" }, st.config.dir)],
        ["Admin tokens", "admin.json (file only, not editable here)"]])),
      h("div", { class: "panel" }, h("h2", {}, "Runtime"), props([
        ["Proxy listener", h("span", { class: "mono" }, st.listeners.proxy)], ["Webhook listener", h("span", { class: "mono" }, st.listeners.webhook)],
        ["Admin listener", h("span", { class: "mono" }, st.listeners.admin)], ["Database", h("span", { class: "mono" }, st.database.path)],
        ["Tracing", st.tracing ? lozenge("OTLP on", "success") : lozenge("off")], ["Active tunnels", String(st.active_tunnels)],
        ["Deliveries", Object.entries(st.deliveries).map(([k, v]) => k + " " + v).join(" · ")],
        ["Signed in as", state.me.name + " (" + state.me.role + ")"]]))),
    h("div", { class: "panel notes" }, h("h2", {}, "Deployment notes"),
      h("p", { class: "help" }, "Checked at startup for this way of running the agent (" +
        (st.environment.container ? st.environment.container + " container" : st.environment.systemd ? "host, systemd" : "host") + ")."),
      st.deployment.length ? h("ul", { class: "timeline" }, ["error", "warn", "info"].flatMap((lv) =>
        st.deployment.filter((d) => d.level === lv).map((d) => h("li", {},
          lozenge(d.level, d.level === "info" ? "info" : "danger" === STATUS_KIND[d.level] ? "danger" : "warn"),
          h("div", {}, d.message), h("span", { class: "mono muted" }, d.topic))))) : h("div", { class: "muted" }, "Nothing to report.")),
    isEditor() ? h("div", { class: "toolbar" }, h("button", { class: "btn", onclick: async () => {
      try { const r = await api("POST", "/api/reload"); toast(Object.keys(r.errors || {}).length ? "Reloaded with errors" : "Reloaded"); route(); }
      catch (e) { toast(e.message, true); }
    } }, icon("refresh"), "Reload config files")) : null,
    h("p", { class: "help" }, "Prometheus metrics: GET /metrics with a viewer or editor token."));
  return { el };
}

function pageNotFound() {
  page("Not found", {}, emptyState("This page does not exist", "", link("/", "Go to overview")));
}

// ---------------------------------------------------------------- login & boot

function renderLogin(error) {
  const input = h("input", { type: "password", autocomplete: "current-password", placeholder: "Admin token", "aria-label": "Admin token" });
  const submit = async (e) => {
    e.preventDefault();
    state.token = input.value.trim();
    try {
      state.me = await api("GET", "/api/whoami");
      try { sessionStorage.setItem("fluid-token", state.token); } catch { /* ignore */ }
      boot();
    } catch (err) {
      state.token = null;
      renderLogin(err.status === 401 ? "That token was not accepted." : err.message);
    }
  };
  fill($app, h("div", { class: "login" }, h("form", { class: "login-card", onsubmit: submit },
    h("div", { class: "logo-mark" }, "F"), h("h1", {}, "fluid-agent console"),
    h("p", {}, "Sign in with a viewer or editor token from admin.json."),
    error ? h("div", { class: "form-error" }, error) : null,
    h("label", { class: "field" }, h("span", {}, "Token"), input),
    h("button", { class: "btn primary", type: "submit" }, "Sign in"))));
  input.focus();
}

async function boot() {
  if (!state.token) return renderLogin();
  if (!state.me) {
    try { state.me = await api("GET", "/api/whoami"); } catch { return; }
  }
  renderShell();
  try { state.live = localStorage.getItem("fluid-live") !== "0"; } catch { state.live = true; }
  if (state.live) startLive();
  route();
}

boot();
