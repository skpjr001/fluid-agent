// fluid-agent console. Plain JS, no build step. All DOM is built with
// textContent / createElement, never innerHTML, so payload data cannot inject markup.
"use strict";

// ---------------------------------------------------------------- helpers

const $app = document.getElementById("app");
const MASK = "********";

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
function when(ts) {
  const t = new Date(ts);
  return h("span", { title: t.toLocaleString() }, ago(ts));
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
  const resp = await fetch(path, opts);
  if (resp.status === 401) { logout(); throw new ApiError(401, "Session expired"); }
  if (resp.status === 204) return null;
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!resp.ok) throw new ApiError(resp.status, (data && data.error) || resp.statusText);
  return data;
}

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
    const resp = await fetch("/api/stream?topics=logs,events,deliveries", {
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
  [/^\/system$/, () => pageSystem()],
];

function navigate(path, replace) {
  if (replace) history.replaceState(null, "", path);
  else history.pushState(null, "", path);
  route();
}
window.addEventListener("popstate", () => route());
document.addEventListener("click", (e) => {
  const a = e.target.closest("a[data-link]");
  if (!a || e.metaKey || e.ctrlKey || e.shiftKey) return;
  e.preventDefault();
  navigate(a.getAttribute("href"));
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    const ov = document.querySelector(".overlay");
    if (ov) ov.remove(); else closeDetail();
  }
});

function link(path, ...children) { return h("a", { href: path, "data-link": true }, ...children); }

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
  const path = location.pathname;
  for (const a of document.querySelectorAll(".nav a")) {
    const p = a.getAttribute("href");
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

async function pageEvents() {
  const defaults = { q: "", source: "", status: "", route: "", since: "" };
  const f = queryState(defaults);
  let items = [], next = "";
  const listEl = h("div", {});
  const load = async (more) => {
    pushQuery(f, defaults);
    const qs = new URLSearchParams({ limit: "50" });
    for (const k of ["q", "source", "status", "route", "since"]) if (f[k]) qs.set(k, f[k]);
    if (more) qs.set("before_id", next);
    const r = await api("GET", "/api/events?" + qs);
    items = more ? items.concat(r.items) : r.items;
    next = r.next_before_id;
    draw();
    if (!more && items.length === 1 && f.q === items[0].id) showEvent(items[0].id);
  };
  const cols = [
    { label: "Key", render: (e) => h("span", { class: "key" }, eventKey(e)) },
    { label: "Source", render: (e) => lozenge(e.source, e.source === "ingest" ? "purple" : "info") },
    { label: "Route", render: (e) => h("span", { class: "cell-flex" }, avatar(e.route, true), h("span", {}, e.route)) },
    { label: "Request", render: (e) => h("span", {}, h("strong", {}, e.method + " "), e.path) },
    { label: "Vendor / type", render: (e) => e.vendor || e.event_type ? [e.vendor || "", e.event_type ? " · " + e.event_type : ""] : h("span", { class: "muted" }, "—") },
    { label: "Status", render: (e) => statusLozenge(e.status) },
    { label: "Size", num: true, render: (e) => bytes(e.body_size) },
    { label: "From", render: (e) => h("span", { class: "mono" }, e.source_ip || "—") },
    { label: "Received", render: (e) => when(e.ts) },
  ];
  const draw = () => fill(listEl, 
    table(cols, items, (e) => showEvent(e.id)) || emptyState("No events", "Calls to /hooks/<id> and ingest paths appear here."),
    next ? h("div", { class: "more" }, h("button", { class: "btn", onclick: () => load(true) }, "Load more")) : null);

  const routes = await routeOptions();
  page("Events", {
    desc: "Every inbound call (webhook or ingest), stored before it is acknowledged. Open one to see its body, headers and deliveries.",
    actions: h("button", { class: "btn", onclick: () => load() }, icon("refresh"), "Refresh"),
  },
    h("div", { class: "toolbar" },
      h("input", { type: "search", placeholder: "Search path, vendor, type…", value: f.q, "aria-label": "Search",
        onchange: (e) => { f.q = e.target.value; load(); } }),
      seg([["", "All"], ["webhook", "Webhooks"], ["ingest", "Ingest"]], f.source, (v) => { f.source = v; pushQuery(f, defaults); route(); }),
      select([["", "Any status"], ["pending", "Pending"], ["delivered", "Delivered"], ["partial", "Partial"], ["failed", "Failed"]], f.status,
        (v) => { f.status = v; load(); }, "Status"),
      select([["", "Any route"], ...routes.map((r) => [r, r])], f.route, (v) => { f.route = v; load(); }, "Route"),
      select(SINCE, f.since, (v) => { f.since = v; load(); }, "Time range")),
    listEl);
  await load();
  return {
    onLive(topic, ev) {
      if (topic !== "events") return;
      if ((f.source && ev.source !== f.source) || (f.route && ev.route !== f.route) || f.q || f.status) return;
      items.unshift(ev);
      draw();
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
  const defaults = { view: "list", status: "", route: "" };
  const f = queryState(defaults);
  let items = [], next = 0;
  const body = h("div", {});
  const load = async (more) => {
    pushQuery(f, defaults);
    const qs = new URLSearchParams({ limit: f.view === "board" ? "300" : "50" });
    if (f.route) qs.set("route", f.route);
    if (f.status && f.view === "list") qs.set("status", f.status);
    if (more) qs.set("before_id", String(next));
    const r = await api("GET", "/api/deliveries?" + qs);
    items = more ? items.concat(r.items) : r.items;
    next = r.next_before_id;
    draw();
  };
  const open = (d) => showEvent(d.event_id);
  const cols = [
    { label: "Key", render: (d) => h("span", { class: "key" }, "DL-" + d.id) },
    { label: "Status", render: (d) => statusLozenge(d.status) },
    { label: "Route", render: (d) => h("span", { class: "cell-flex" }, avatar(d.route, true), h("span", {}, d.route)) },
    { label: "Target", render: (d) => h("span", { class: "mono", title: d.target }, d.target) },
    { label: "Mode", render: (d) => lozenge(d.mode, d.mode === "sync" ? "" : "purple") },
    { label: "Attempts", num: true, render: (d) => `${d.attempts}/${d.max_attempts}` },
    { label: "Last", render: (d) => httpLozenge(d.last_status) },
    { label: "Updated", render: (d) => when(d.updated_at) },
    { label: "", render: (d) => retryButton(d, () => load()) },
  ];
  const COLS = [["pending", "Pending"], ["inflight", "In flight"], ["delivered", "Delivered"], ["failed", "Failed (sync)"], ["dead", "Dead letter"]];
  const draw = () => {
    if (f.view === "board") {
      fill(body, h("div", { class: "board" }, COLS.map(([s, title]) => {
        const col = items.filter((d) => d.status === s);
        return h("section", { class: "column", "aria-label": title }, h("h3", {}, title, h("span", { class: "badge" }, String(col.length))),
          col.slice(0, 60).map((d) => {
            const c = h("div", { class: "bcard", tabindex: "0" },
              h("div", { class: "t", title: d.target }, d.target.replace(/^https?:\/\//, "")),
              h("div", { class: "m" }, avatar(d.route, true), h("span", {}, d.route), d.last_status ? httpLozenge(d.last_status) : null,
                h("span", { class: "key" }, "DL-" + d.id)),
              d.last_error && s !== "delivered" ? h("div", { class: "muted" }, d.last_error) : null);
            c.addEventListener("click", () => open(d));
            c.addEventListener("keydown", (e) => { if (e.key === "Enter") open(d); });
            return c;
          }));
      })));
    } else {
      fill(body, table(cols, items, open) || emptyState("No deliveries", "Deliveries are created for every event target."),
        next ? h("div", { class: "more" }, h("button", { class: "btn", onclick: () => load(true) }, "Load more")) : null);
    }
  };
  const routes = await routeOptions();
  page("Deliveries", {
    desc: "Each event is sent to its targets with retries and exponential backoff; deliveries that give up land in the dead-letter column.",
    actions: [
      isEditor() ? h("button", { class: "btn", onclick: async () => {
        try { const r = await api("POST", "/api/deliveries/retry-dead" + (f.route ? "?route=" + encodeURIComponent(f.route) : "")); toast(r.queued + " deliveries queued"); load(); }
        catch (e) { toast(e.message, true); }
      } }, icon("retry"), "Retry all dead") : null,
      h("button", { class: "btn", onclick: () => load() }, icon("refresh"), "Refresh"),
    ],
  },
    h("div", { class: "toolbar" },
      seg([["list", "List"], ["board", "Board"]], f.view, (v) => { f.view = v; pushQuery(f, defaults); route(); }),
      f.view === "list" ? select([["", "Any status"], ...COLS], f.status, (v) => { f.status = v; load(); }, "Status") : null,
      select([["", "Any route"], ...routes.map((r) => [r, r])], f.route, (v) => { f.route = v; load(); }, "Route")),
    body);
  await load();
  return {
    onLive(topic, d) {
      if (topic !== "deliveries" || (f.route && d.route !== f.route)) return;
      const i = items.findIndex((x) => x.id === d.id);
      if (i >= 0) items[i] = d; else items.unshift(d);
      draw();
    },
  };
}

// ---------------------------------------------------------------- logs

const KINDS = ["proxy_http", "proxy_connect", "proxy_denied", "forward", "webhook", "ingest", "delivery", "auth", "admin", "config", "system", "app"];

async function pageLogs() {
  const defaults = { view: "list", direction: "", kind: "", level: "", status: "", since: "1h", q: "", route: "", client: "", host: "", by: "kind" };
  const f = queryState(defaults);
  let items = [], next = 0;
  const body = h("div", {});
  const qs = () => {
    const q = new URLSearchParams();
    for (const k of ["direction", "kind", "level", "status", "since", "q", "route", "client", "host"]) if (f[k]) q.set(k, f[k]);
    return q;
  };
  const load = async (more) => {
    pushQuery(f, defaults);
    if (f.view === "summary") {
      const q = qs();
      q.set("by", f.by);
      const r = await api("GET", "/api/logs/summary?" + q);
      drawSummary(r.items);
      return;
    }
    const q = qs();
    q.set("limit", "100");
    if (more) q.set("before_id", String(next));
    const r = await api("GET", "/api/logs?" + q);
    items = more ? items.concat(r.items) : r.items;
    next = r.next_before_id;
    draw();
  };
  const cols = [
    { label: "Time", render: (l) => h("span", { class: "mono", title: new Date(l.ts).toLocaleString() }, new Date(l.ts).toLocaleTimeString()) },
    { label: "Level", render: (l) => lozenge(l.level, STATUS_KIND[l.level]) },
    { label: "Dir", render: (l) => dirTag(l.direction) },
    { label: "Kind", render: (l) => h("span", { class: "mono" }, l.kind) },
    { label: "Client / route", render: (l) => (l.client || l.route) ? h("span", { class: "cell-flex" }, avatar(l.client || l.route, true), h("span", {}, l.client || l.route)) : h("span", { class: "muted" }, "—") },
    { label: "Method", render: (l) => l.method || "" },
    { label: "Status", render: (l) => httpLozenge(l.status) },
    { label: "ms", num: true, render: (l) => l.duration_ms ? String(l.duration_ms) : "" },
    { label: "URL / message", render: (l) => h("span", { title: l.url || l.msg }, l.url || l.msg) },
  ];
  const showLog = (l) => openDetail("#" + l.id, l.msg,
    h("div", { class: "toolbar" }, lozenge(l.level, STATUS_KIND[l.level]), dirTag(l.direction), h("span", { class: "mono" }, l.kind)),
    props([["Time", new Date(l.ts).toLocaleString()], ["Client", l.client || null], ["Route", l.route || null], ["Method", l.method || null],
      ["Host", l.host || null], ["URL", l.url ? h("span", { class: "mono" }, l.url) : null], ["Status", l.status ? httpLozenge(l.status) : null],
      ["Duration", l.duration_ms ? l.duration_ms + " ms" : null], ["Bytes in / out", (l.bytes_in || l.bytes_out) ? bytes(l.bytes_in) + " / " + bytes(l.bytes_out) : null],
      ["Source IP", l.source_ip || null], ["Trace", l.trace_id ? h("span", { class: "mono" }, l.trace_id) : null]]),
    l.attrs && l.attrs.event_id ? h("p", {}, link("/events?q=" + encodeURIComponent(l.attrs.event_id), "Open event " + l.attrs.event_id)) : null,
    l.attrs ? [h("h3", {}, "Attributes"), h("pre", { class: "code" }, JSON.stringify(l.attrs, null, 2))] : null);
  const draw = () => fill(body, 
    table(cols, items, showLog) || emptyState("No log entries", "Try a wider time range or fewer filters."),
    next ? h("div", { class: "more" }, h("button", { class: "btn", onclick: () => load(true) }, "Load more")) : null);
  const drawSummary = (rows) => {
    const max = rows.length ? rows[0].count : 1;
    fill(body, rows.length ? h("div", { class: "table-wrap" }, h("table", { class: "issues" },
      h("thead", {}, h("tr", {}, ["", "Count", "Errors", "Avg ms", "Bytes out", "Last seen"].map((c, i) => h("th", { class: i ? "num" : "" }, c || f.by)))),
      h("tbody", {}, rows.map((r) => {
        const fill = h("i", {});
        fill.style.width = Math.max(2, (r.count / max) * 100) + "%";
        const tr = h("tr", {},
          h("td", {}, h("div", {}, r.key || "—"), h("div", { class: "bar" }, fill)),
          h("td", { class: "num" }, String(r.count)), h("td", { class: "num" }, r.errors ? lozenge(String(r.errors), "danger") : "0"),
          h("td", { class: "num" }, r.avg_ms.toFixed(0)), h("td", { class: "num" }, bytes(r.bytes_out)), h("td", { class: "num" }, when(r.last_seen)));
        tr.addEventListener("click", () => { if (["kind", "direction", "level", "status", "route", "client", "host"].includes(f.by)) { f[f.by] = r.key; f.view = "list"; pushQuery(f, defaults); route(); } });
        return tr;
      })))) : emptyState("Nothing to summarize", "No log entries match these filters."));
  };
  const text = (k, ph) => h("input", { type: "search", placeholder: ph, value: f[k], "aria-label": ph, onchange: (e) => { f[k] = e.target.value; load(); } });

  page("Logs", {
    desc: "Every proxy call, forward, webhook, ingest and delivery, stored in SQLite. Incoming calls are marked IN, outgoing calls OUT.",
    actions: h("button", { class: "btn", onclick: () => load() }, icon("refresh"), "Refresh"),
  },
    h("div", { class: "toolbar" },
      seg([["list", "List"], ["summary", "Summary"]], f.view, (v) => { f.view = v; pushQuery(f, defaults); route(); }),
      seg([["", "All"], ["inbound", "↓ Incoming"], ["outbound", "↑ Outgoing"]], f.direction, (v) => { f.direction = v; pushQuery(f, defaults); route(); }),
      select([["", "Any kind"], ...KINDS.map((k) => [k, k])], f.kind, (v) => { f.kind = v; load(); }, "Kind"),
      select([["", "Any level"], ["info", "Info+"], ["warn", "Warn+"], ["error", "Error"]], f.level, (v) => { f.level = v; load(); }, "Level"),
      select([["", "Any status"], ["2xx", "2xx"], ["3xx", "3xx"], ["4xx", "4xx"], ["5xx", "5xx"], ["error", "≥ 400"]], f.status, (v) => { f.status = v; load(); }, "Status"),
      select(SINCE, f.since, (v) => { f.since = v; load(); }, "Time range"),
      f.view === "summary" ? select([["kind", "Group by kind"], ["direction", "Group by direction"], ["route", "Group by route"],
        ["client", "Group by client"], ["host", "Group by host"], ["status", "Group by status"], ["source_ip", "Group by source IP"],
        ["level", "Group by level"], ["method", "Group by method"]], f.by, (v) => { f.by = v; load(); }, "Group by") : null),
    h("div", { class: "toolbar" }, text("q", "Search text…"), text("route", "Route"), text("client", "Client"), text("host", "Host")),
    body);
  await load();
  return {
    onLive(topic, l) {
      if (topic !== "logs" || f.view !== "list") return;
      if ((f.direction && l.direction !== f.direction) || (f.kind && l.kind !== f.kind) || f.q || f.status ||
        (f.route && l.route !== f.route) || (f.client && l.client !== f.client) || (f.host && l.host !== f.host)) return;
      if (f.level && ["debug", "info", "warn", "error"].indexOf(l.level) < ["debug", "info", "warn", "error"].indexOf(f.level)) return;
      items.unshift(l);
      if (items.length > 500) items.length = 500;
      draw();
    },
  };
}

// ---------------------------------------------------------------- config resources (webhooks, ingest, profiles)

const ROUTE_FIELDS = [
  { legend: "Inbound checks", fields: [
    { k: "methods", label: "Allowed methods", type: "list", help: "One per line. Default POST; * allows any." },
    { k: "source_cidrs", label: "Allowed sender IPs / CIDRs", type: "list", help: "One per line. Empty allows any sender." },
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
      { label: "ID", render: (w) => h("span", { class: "cell-flex" }, avatar(w.id, true), h("span", { class: "key" }, w.id)) },
      { label: "Status", render: (w) => w.disabled ? lozenge("disabled") : lozenge("active", "success") },
      { label: "Mode", render: (w) => lozenge(w.async ? "async" : "sync", w.async ? "purple" : "") },
      { label: "Target", render: (w) => h("span", { class: "mono", title: w.target }, w.target) },
      { label: "Mirrors", num: true, render: (w) => String((w.mirrors || []).length) },
      { label: "Auth", render: (w) => authSummary(w) },
      { label: "Updated", render: (w) => when(w.updated_at) },
    ],
    detail: (w) => [
      props([["Public path", h("span", { class: "mono" }, "/hooks/" + w.id)], ["Target", h("span", { class: "mono" }, w.target)],
        ["Mirrors", (w.mirrors || []).join("\n") || null], ["Mode", w.async ? "async (202, durable)" : "sync (passthrough)"],
        ["Methods", (w.methods || []).join(", ")], ["Senders", (w.source_cidrs || []).join(", ") || "any"],
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
      { label: "ID", render: (r) => h("span", { class: "cell-flex" }, avatar(r.id, true), h("span", { class: "key" }, r.id)) },
      { label: "Status", render: (r) => r.disabled ? lozenge("disabled") : lozenge("active", "success") },
      { label: "Priority", num: true, render: (r) => String(r.priority || 0) },
      { label: "Match", render: (r) => h("span", {}, lozenge(r.match.type), " ", h("span", { class: "mono" }, r.match.value)) },
      { label: "Vendor / type", render: (r) => [r.vendor || "", r.event_type ? " · " + r.event_type : ""] },
      { label: "Targets", num: true, render: (r) => String(r.targets.length) },
      { label: "Auth", render: (r) => authSummary(r) },
    ],
    detail: (r) => [props([["Match", r.match.type + " " + r.match.value], ["Priority", String(r.priority || 0)], ["Vendor", r.vendor || null],
      ["Event type", r.event_type || null], ["Targets", r.targets.join("\n")], ["Ack", `${r.response.status} ${r.response.content_type}`],
      ["Ack body", h("span", { class: "mono" }, r.response.body)], ["Senders", (r.source_cidrs || []).join(", ") || "any"],
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
      { label: "Name", render: (p) => h("span", { class: "cell-flex" }, avatar(p.name, true), h("span", { class: "key" }, p.name)) },
      { label: "Base URL", render: (p) => h("span", { class: "mono" }, p.base_url) },
      { label: "Injected headers", render: (p) => Object.keys(p.headers || {}).join(", ") || h("span", { class: "muted" }, "none") },
      { label: "Clients", render: (p) => (p.clients || []).join(", ") || "all" },
      { label: "Methods", render: (p) => (p.allowed_methods || []).join(", ") || "any" },
    ],
    detail: (p) => [props([["Base URL", h("span", { class: "mono" }, p.base_url)], ["Headers", Object.keys(p.headers || {}).join(", ") || "none"],
      ["Clients", (p.clients || []).join(", ") || "all"], ["Methods", (p.allowed_methods || []).join(", ") || "any"],
      ["Timeout", p.timeout_sec ? p.timeout_sec + " s" : "default"], ["Description", p.description || null]]),
      h("h3", {}, "Usage"), h("pre", { class: "code" }, `curl -X POST http://<agent>:8080/forward \\\n  -H "X-Agent-Key: <client token>" \\\n  -d '{"profile":"${p.name}","method":"GET","path":"/..."}'`)],
    related: (p) => "/logs?kind=forward&route=" + encodeURIComponent(p.name),
  },
};

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
    table(res.cols, items, (it) => showResource(res, it)) ||
      emptyState("No " + res.title.toLowerCase() + " yet", "They are stored as JSON files in the dynamic directory.",
        isEditor() ? h("button", { class: "btn primary", onclick: () => editResource(res, null) }, "Create " + res.singular) : null));
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
        "Full object as stored in the file. Validated against ", link("/api/schema/" + (res === RES.ingest ? "ingest" : res === RES.profiles ? "profile" : "webhook") + ".schema.json", "the JSON Schema"), " on save."));
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
    { label: "Client", render: (c) => h("span", { class: "cell-flex" }, avatar(c.name, true), h("span", {}, c.name)) },
    { label: "Status", render: (c) => c.disabled ? lozenge("disabled") : lozenge("active", "success") },
    { label: "Token", render: (c) => h("span", { class: "mono" }, c.token) },
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
    table(clientCols, p.clients) || emptyState("No clients", "Create one to let applications use the proxy."));
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
