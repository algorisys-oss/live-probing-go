// LiveProbe browser client. The server renders every fragment; this script only:
//  - subscribes to /ui/live (Server-Sent Events) and applies fragments in one
//    requestAnimationFrame batch (never a DOM write per event — LOOPS XXXIX),
//  - keeps the live trace list sorted/capped, flashes new rows, supports pause,
//  - handles zoom/pan/fit and diagram export on the flow graph,
//  - delegates clicks on [data-href], toggles the theme.
(function () {
  "use strict";

  var MAX_ROWS = 200;
  var $ = function (id) { return document.getElementById(id); };

  // ---- theme ----------------------------------------------------------------------------
  var THEME_KEY = "liveprobe-theme";
  function currentTheme() { return document.documentElement.getAttribute("data-theme") === "light" ? "light" : "dark"; }
  function syncThemeButton() {
    var b = $("lp-theme-toggle");
    if (b) b.textContent = currentTheme() === "dark" ? "☀" : "☾";
  }
  document.addEventListener("click", function (e) {
    if (!e.target.closest || !e.target.closest("#lp-theme-toggle")) return;
    var next = currentTheme() === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    try { localStorage.setItem(THEME_KEY, next); } catch (_) { /* private mode: session only */ }
    syncThemeButton();
  });

  // ---- navigation: [data-href] anywhere (rows, tiles, graph nodes, matrix cells) ---------
  var suppressClick = false; // set after a drag so releasing over a node doesn't navigate
  document.addEventListener("click", function (e) {
    if (suppressClick) { suppressClick = false; return; }
    var el = e.target.closest && e.target.closest("[data-href]");
    if (!el || !el.getAttribute("data-href") || el.disabled) return;
    var href = el.getAttribute("data-href");
    if (el.closest("#lp-trace") && window.location.hash) href += window.location.hash; // keep the selected span
    if (e.metaKey || e.ctrlKey) { window.open(href, "_blank"); return; }
    window.location.href = href;
  });

  // ---- rAF batching ---------------------------------------------------------------------
  var pendingRows = [];   // accumulated row fragments
  var pendingLive = null; // only the latest live fragment set matters
  var pendingReset = null;
  var scheduled = false;
  function schedule() {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(function () {
      scheduled = false;
      if (pendingReset) { applyReset(pendingReset); pendingReset = null; }
      if (pendingRows.length) { applyRows(pendingRows, true); pendingRows = []; }
      if (pendingLive) { applyLive(pendingLive); pendingLive = null; }
    });
  }

  // ---- trace list -----------------------------------------------------------------------
  var paused = false, held = [], heldNew = 0;
  function rowNode(html) {
    var t = document.createElement("template");
    t.innerHTML = html.trim();
    return t.content.firstElementChild;
  }
  function knownIds(list) {
    var ids = {};
    for (var i = 0; i < list.children.length; i++) ids[list.children[i].getAttribute("data-id")] = list.children[i];
    return ids;
  }
  function applyRows(rows, flash) {
    var list = $("lp-trace-list");
    if (!list) return;
    var ids = knownIds(list);
    if (paused) {
      for (var j = 0; j < rows.length; j++) if (!ids[rows[j].id]) heldNew++;
      held = held.concat(rows);
      updatePauseButton();
      return;
    }
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i], node = rowNode(r.html), old = ids[r.id];
      if (old) {
        old.replaceWith(node); // a trace grows as spans stream in
      } else {
        // insert newest-first by start time
        var before = null;
        for (var k = 0; k < list.children.length; k++) {
          if (Number(list.children[k].getAttribute("data-start")) < r.start) { before = list.children[k]; break; }
        }
        list.insertBefore(node, before);
        if (flash) flashRow(node);
      }
      ids[r.id] = node;
    }
    while (list.children.length > MAX_ROWS) list.removeChild(list.lastElementChild);
    var empty = $("lp-trace-empty");
    if (empty) empty.hidden = list.children.length > 0;
  }
  function flashRow(node) {
    node.classList.add("trace-row-new");
    setTimeout(function () { node.classList.remove("trace-row-new"); }, 1500);
  }
  function updatePauseButton() {
    var b = $("lp-pause");
    if (!b) return;
    b.className = paused ? "pause-btn pause-btn-active" : "pause-btn";
    b.textContent = paused ? "Paused · " + heldNew + " new" : "Pause";
    b.title = paused ? "Resume live feed" : "Pause live feed";
  }
  document.addEventListener("click", function (e) {
    if (!e.target.closest || !e.target.closest("#lp-pause")) return;
    paused = !paused;
    if (!paused) {
      var rows = held;
      held = []; heldNew = 0;
      applyRows(rows, false);
    }
    updatePauseButton();
  });

  // ---- live fragments -------------------------------------------------------------------
  function setText(id, v) { var el = $(id); if (el) el.textContent = String(v); }
  function applyHeader(f) {
    setText("lp-service-count", f.services);
    setText("lp-trace-count", f.traces);
    var badge = $("lp-alert-badge");
    if (badge) {
      badge.hidden = f.alerts === 0;
      badge.textContent = String(f.alerts);
      badge.className = "nav-badge nav-badge-" + f.alertSeverity;
    }
  }
  function applyLive(f) {
    applyHeader(f);
    if (!$("lp-live")) return;
    var red = $("lp-red");
    if (red) red.innerHTML = f.red || "";
    var sel = $("lp-filter");
    if (sel && document.activeElement !== sel && f.filterOptions) sel.innerHTML = f.filterOptions; // don't yank an open dropdown
    var matrix = $("lp-matrix");
    if (matrix && f.matrix !== undefined) matrix.innerHTML = f.matrix;
    if (f.flow !== undefined || f.flowEmpty) applyFlow(f);
  }
  function applyReset(msg) {
    var list = $("lp-trace-list");
    if (list && msg.rows) {
      list.innerHTML = "";
      applyRows(msg.rows, false);
    }
    applyLive(msg.live);
  }

  // ---- flow graph: zoom / pan / fit -----------------------------------------------------
  var view = null, fitBox = null;
  function parseBox(s) {
    var p = String(s || "").split(/\s+/).map(Number);
    return p.length === 4 && p.every(isFinite) ? { x: p[0], y: p[1], w: p[2], h: p[3] } : null;
  }
  function svgEl() { return $("lp-flow-svg"); }
  function setView(v) {
    view = v;
    var svg = svgEl();
    if (svg && v) svg.setAttribute("viewBox", v.x + " " + v.y + " " + v.w + " " + v.h);
  }
  function applyFlow(f) {
    var flow = $("lp-flow"), empty = $("lp-flow-empty"), content = $("lp-flow-content"), svg = svgEl();
    if (flow) flow.hidden = !!f.flowEmpty;
    if (empty) empty.hidden = !f.flowEmpty;
    if (content && f.flow !== undefined) content.innerHTML = f.flow;
    if (svg && f.mermaid !== undefined) svg.setAttribute("data-mermaid", f.mermaid);
    // Re-fit only when the node set (and so the fit box) changes, never on call-count deltas.
    if (f.flowFit && f.flowFit !== fitBox) {
      fitBox = f.flowFit;
      setView(parseBox(fitBox));
    }
  }
  function measure() {
    var svg = svgEl();
    if (!svg || !view) return null;
    var rect = svg.getBoundingClientRect();
    var scale = Math.min(rect.width / view.w, rect.height / view.h);
    return { rect: rect, scale: scale, offX: (rect.width - view.w * scale) / 2, offY: (rect.height - view.h * scale) / 2 };
  }
  function zoomAround(cx, cy, factor) {
    var m = measure();
    if (!m) return;
    var sx = view.x + (cx - m.rect.left - m.offX) / m.scale;
    var sy = view.y + (cy - m.rect.top - m.offY) / m.scale;
    setView({ x: sx - (sx - view.x) * factor, y: sy - (sy - view.y) * factor, w: view.w * factor, h: view.h * factor });
  }
  var drag = null, dragged = false;
  function initFlow() {
    var svg = svgEl();
    if (!svg) return;
    fitBox = svg.getAttribute("data-fit");
    view = parseBox(fitBox);
    svg.addEventListener("wheel", function (e) {
      e.preventDefault();
      zoomAround(e.clientX, e.clientY, e.deltaY > 0 ? 1.1 : 0.9);
    }, { passive: false });
    svg.addEventListener("mousedown", function (e) { drag = { x: e.clientX, y: e.clientY }; dragged = false; });
    svg.addEventListener("mousemove", function (e) {
      if (!drag) return;
      var m = measure();
      if (!m) return;
      var dx = (e.clientX - drag.x) / m.scale, dy = (e.clientY - drag.y) / m.scale;
      if (Math.abs(e.clientX - drag.x) + Math.abs(e.clientY - drag.y) > 2) { dragged = true; svg.classList.add("grabbing"); }
      drag = { x: e.clientX, y: e.clientY };
      setView({ x: view.x - dx, y: view.y - dy, w: view.w, h: view.h });
    });
    var end = function () {
      if (dragged) suppressClick = true;
      drag = null;
      svg.classList.remove("grabbing");
      setTimeout(function () { suppressClick = false; }, 0);
    };
    svg.addEventListener("mouseup", end);
    svg.addEventListener("mouseleave", function () { drag = null; svg.classList.remove("grabbing"); });
    document.addEventListener("click", function (e) {
      var b = e.target.closest && e.target.closest("[data-zoom],[data-fit],[data-export]");
      if (!b || !b.closest("#lp-flow")) return;
      var m = measure();
      if (b.hasAttribute("data-zoom") && m) zoomAround(m.rect.left + m.rect.width / 2, m.rect.top + m.rect.height / 2, Number(b.getAttribute("data-zoom")));
      if (b.hasAttribute("data-fit")) setView(parseBox(fitBox));
      var kind = b.getAttribute("data-export");
      if (kind === "mmd") copyText(svg.getAttribute("data-mermaid") || "");
      if (kind === "svg") downloadSvg(svg, "topology.svg");
      if (kind === "png") downloadPng(svg, "topology.png", 2);
    });
  }

  // ---- export (port of lib/export-diagram.ts) ------------------------------------------
  var STYLE_PROPS = ["fill", "stroke", "stroke-width", "stroke-dasharray", "stroke-linecap", "opacity", "font-family", "font-size", "font-weight", "text-anchor"];
  function inlineStyles(src, dst) {
    var cs = getComputedStyle(src), style = "";
    for (var i = 0; i < STYLE_PROPS.length; i++) {
      var v = cs.getPropertyValue(STYLE_PROPS[i]);
      if (v) style += STYLE_PROPS[i] + ":" + v + ";";
    }
    dst.setAttribute("style", style);
    for (var j = 0; j < src.children.length && j < dst.children.length; j++) inlineStyles(src.children[j], dst.children[j]);
  }
  function viewBoxDims(svg) {
    var b = parseBox(svg.getAttribute("viewBox"));
    if (b && b.w > 0 && b.h > 0) return { w: b.w, h: b.h };
    var r = svg.getBoundingClientRect();
    return { w: Math.max(1, r.width), h: Math.max(1, r.height) };
  }
  function standaloneSvg(svg) {
    var bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim() || "#0d1117";
    var clone = svg.cloneNode(true);
    inlineStyles(svg, clone);
    clone.setAttribute("xmlns", "http://www.w3.org/2000/svg");
    var d = viewBoxDims(svg), vb = parseBox(svg.getAttribute("viewBox")) || { x: 0, y: 0, w: d.w, h: d.h };
    clone.setAttribute("width", String(d.w));
    clone.setAttribute("height", String(d.h));
    var rect = document.createElementNS("http://www.w3.org/2000/svg", "rect");
    rect.setAttribute("x", String(vb.x)); rect.setAttribute("y", String(vb.y));
    rect.setAttribute("width", String(vb.w)); rect.setAttribute("height", String(vb.h));
    rect.setAttribute("fill", bg);
    clone.insertBefore(rect, clone.firstChild);
    return new XMLSerializer().serializeToString(clone);
  }
  function download(blob, filename) {
    var url = URL.createObjectURL(blob), a = document.createElement("a");
    a.href = url; a.download = filename; a.click();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  }
  function copyText(text) { if (navigator.clipboard) navigator.clipboard.writeText(text); }
  function downloadSvg(svg, filename) { download(new Blob([standaloneSvg(svg)], { type: "image/svg+xml" }), filename); }
  function downloadPng(svg, filename, scale) {
    var d = viewBoxDims(svg), img = new Image();
    img.onload = function () {
      var c = document.createElement("canvas");
      c.width = Math.round(d.w * scale); c.height = Math.round(d.h * scale);
      var ctx = c.getContext("2d");
      if (!ctx) return;
      ctx.drawImage(img, 0, 0, c.width, c.height);
      c.toBlob(function (blob) { if (blob) download(blob, filename); }, "image/png");
    };
    img.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(standaloneSvg(svg));
  }
  // Expose for other pages' diagrams (sequence, waterfall) to reuse.
  window.liveprobe = { downloadSvg: downloadSvg, downloadPng: downloadPng, copyText: copyText };

  // ---- trace page: span selection, subtree collapse, tabs, copy -------------------------
  // The selection is shared by the waterfall rows, sequence arrows and activation bars, and
  // mirrored in the URL #hash (a shareable deep link), as in the React trace page.
  function selectSpan(id, fetchPanel) {
    var trace = $("lp-trace");
    if (!trace || !id) return;
    var marks = document.querySelectorAll("[data-span]");
    for (var i = 0; i < marks.length; i++) {
      var el = marks[i], on = el.getAttribute("data-span") === id;
      if (el.classList.contains("wf-row")) el.classList.toggle("wf-row-selected", on);
      else if (el.classList.contains("seq-activation")) el.classList.toggle("seq-activation-selected", on);
      else if (el.classList.contains("seq-msg")) el.classList.toggle("seq-msg-selected", on);
    }
    if (window.location.hash.slice(1) !== id) history.replaceState(null, "", "#" + id);
    if (!fetchPanel) return;
    var url = "/ui/trace/" + encodeURIComponent(trace.getAttribute("data-trace")) + "/span/" + encodeURIComponent(id);
    fetch(url).then(function (r) { return r.ok ? r.text() : ""; }).then(function (html) {
      var panel = $("lp-span-detail");
      if (panel && html) panel.innerHTML = html;
    });
  }
  document.addEventListener("click", function (e) {
    var el = e.target.closest && e.target.closest("#lp-trace [data-span]");
    if (!el || el.getAttribute("data-href") || !el.getAttribute("data-span")) return;
    if (e.target.closest(".wf-toggle")) return;
    selectSpan(el.getAttribute("data-span"), true);
  });
  // Tabs keep the selected span.
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest("a[data-keep-hash]");
    if (!a || !window.location.hash) return;
    e.preventDefault();
    window.location.href = a.getAttribute("href") + window.location.hash;
  });
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copy]");
    if (b) copyText(b.getAttribute("data-copy"));
  });
  // Waterfall: collapse/expand a subtree. Rows are DFS-ordered, so a node's descendants are the
  // following rows deeper than it; a collapsed node hides its whole subtree.
  var collapsed = {};
  function relayoutWaterfall() {
    var rows = document.querySelectorAll(".wf-rows > .wf-row");
    var hideDeeperThan = Infinity;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i], depth = Number(r.getAttribute("data-depth"));
      if (depth > hideDeeperThan) { r.hidden = true; continue; }
      hideDeeperThan = Infinity;
      r.hidden = false;
      var id = r.getAttribute("data-span"), isCollapsed = !!collapsed[id];
      var toggle = r.querySelector("button.wf-toggle"), count = r.querySelector(".wf-hidden-count");
      if (toggle) {
        toggle.textContent = isCollapsed ? "▸" : "▾";
        toggle.title = isCollapsed ? "Expand subtree" : "Collapse subtree";
        if (isCollapsed) hideDeeperThan = depth;
      }
      if (count) count.hidden = !(isCollapsed && Number(r.getAttribute("data-hidden")) > 0);
    }
  }
  document.addEventListener("click", function (e) {
    var t = e.target.closest && e.target.closest(".wf-rows button.wf-toggle");
    if (!t) return;
    e.stopPropagation();
    var id = t.closest(".wf-row").getAttribute("data-span");
    collapsed[id] = !collapsed[id];
    relayoutWaterfall();
  });
  function initTrace() {
    var trace = $("lp-trace");
    if (!trace) return;
    var hash = decodeURIComponent(window.location.hash.slice(1));
    var serverDefault = trace.getAttribute("data-selected");
    var hasHash = hash && document.querySelector('[data-span="' + CSS.escape(hash) + '"]');
    // Honour a #spanId deep link; otherwise keep the server's default (failing span, else root).
    selectSpan(hasHash ? hash : serverDefault, hasHash && hash !== serverDefault);
  }

  // ---- filter select → navigate (view state lives in the URL) ---------------------------
  document.addEventListener("change", function (e) {
    if (e.target.id !== "lp-filter") return;
    var u = new URL(window.location.href);
    if (e.target.value) u.searchParams.set("service", e.target.value);
    else u.searchParams.delete("service");
    window.location.href = u.pathname + u.search;
  });

  // ---- stream ---------------------------------------------------------------------------
  function connect() {
    var app = document.querySelector(".app[data-stream]");
    if (!app || !window.EventSource) return;
    var es = new EventSource(app.getAttribute("data-stream"));
    var status = function (live) {
      var s = $("lp-status");
      if (s) s.className = live ? "status status-live" : "status status-offline";
      setText("lp-status-text", live ? "live" : "offline");
    };
    es.onopen = function () { status(true); };
    es.onerror = function () { status(false); }; // EventSource reconnects by itself
    es.addEventListener("reset", function (e) { pendingReset = JSON.parse(e.data); pendingRows = []; pendingLive = null; schedule(); });
    es.addEventListener("traces", function (e) {
      var m = JSON.parse(e.data);
      pendingRows = pendingRows.concat(m.rows);
      setText("lp-trace-count", m.count);
      schedule();
    });
    es.addEventListener("live", function (e) { pendingLive = JSON.parse(e.data); schedule(); });
  }

  function init() {
    syncThemeButton();
    initFlow();
    initTrace();
    connect();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
