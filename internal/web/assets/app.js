// syncwatch: keeps fold-outs open across live updates, filters the grid and
// the sites, asks for confirmation on destructive forms, and counts on the
// age of the last structure scan. No inline scripts (CSP).
(function () {
  "use strict";
  var openKeys = {};

  function rememberOpen(root) {
    root.querySelectorAll("details[data-key]").forEach(function (d) {
      openKeys[d.getAttribute("data-key")] = d.open;
    });
  }

  function restoreOpen(root) {
    root.querySelectorAll("details[data-key]").forEach(function (d) {
      var k = d.getAttribute("data-key");
      if (Object.prototype.hasOwnProperty.call(openKeys, k)) d.open = openKeys[k];
    });
  }

  // Opens the finding (#f-<id>) or section (#snoozed) named in the URL,
  // and every fold-out it sits in.
  function openFromHash() {
    if (!location.hash) return;
    var el = document.getElementById(location.hash.slice(1));
    if (!el) return;
    var d = el.tagName === "DETAILS" ? el : el.querySelector("details") || el.closest("details");
    while (d) {
      d.open = true;
      if (d.hasAttribute("data-key")) openKeys[d.getAttribute("data-key")] = true;
      d = d.parentElement && d.parentElement.closest("details");
    }
  }

  function applyGridFilter() {
    var q = document.getElementById("grid-search");
    var onlyProblems = document.getElementById("grid-problems");
    if (!q || !onlyProblems) return;
    var text = q.value.trim().toLowerCase();
    var only = onlyProblems.checked;
    document.querySelectorAll(".frow").forEach(function (row) {
      var match = (!text || (row.getAttribute("data-search") || "").indexOf(text) >= 0) &&
        (!only || row.classList.contains("has-problem"));
      row.classList.toggle("hidden", !match);
    });
    document.querySelectorAll(".group").forEach(function (g) {
      var visible = g.querySelectorAll(".frow:not(.hidden)").length;
      g.classList.toggle("hidden", visible === 0);
      if ((text || only) && visible > 0) g.open = true;
    });
    try {
      sessionStorage.setItem("sw-grid", JSON.stringify({ q: q.value, p: only }));
    } catch (e) { /* storage may be unavailable */ }
  }

  // The site filter (W-057): the select "Site" of the Overview and the
  // Folders page, when 2 or more sites are set. It hides the elements of
  // other sites: data-site is the site of a tile, a grid column, a device
  // list or a finding row of one site; data-sites has the sites (1 on each
  // line) of a Connections row or a finding row between the servers of more
  // sites, which shows with each of them. An element without these (a
  // finding without a server) always shows. The browser remembers the site
  // for both pages (localStorage "sw-site").
  function siteHidden(el, site) {
    if (site === "") return false;
    if (el.hasAttribute("data-site")) return el.getAttribute("data-site") !== site;
    return el.getAttribute("data-sites").split("\n").indexOf(site) < 0;
  }

  function applySiteFilter() {
    var sel = document.getElementById("site-filter");
    var site = sel ? sel.value : "";
    document.querySelectorAll("[data-site], [data-sites]").forEach(function (el) {
      el.classList.toggle("site-hidden", siteHidden(el, site));
    });
    // A list with data-limit shows that many of its visible rows; the
    // paragraph .site-empty after it shows when the site has none.
    document.querySelectorAll("[data-limit]").forEach(function (list) {
      var limit = parseInt(list.getAttribute("data-limit"), 10) || 0;
      var shown = 0;
      Array.prototype.forEach.call(list.children, function (li) {
        var over = false;
        if (!li.classList.contains("site-hidden")) {
          shown++;
          over = shown > limit;
        }
        li.classList.toggle("over-limit", over);
      });
      var none = list.nextElementSibling;
      if (none && none.classList.contains("site-empty")) none.hidden = shown > 0;
    });
  }

  function initSiteFilter() {
    var sel = document.getElementById("site-filter");
    if (!sel) return;
    try {
      var saved = localStorage.getItem("sw-site") || "";
      for (var i = 0; i < sel.options.length; i++) {
        if (sel.options[i].value === saved) sel.value = saved;
      }
    } catch (e) { /* storage may be unavailable */ }
    sel.addEventListener("change", function () {
      try {
        localStorage.setItem("sw-site", sel.value);
      } catch (e) { /* storage may be unavailable */ }
      applySiteFilter();
    });
  }

  document.addEventListener("toggle", function (e) {
    var d = e.target;
    if (d && d.tagName === "DETAILS" && d.hasAttribute("data-key")) {
      openKeys[d.getAttribute("data-key")] = d.open;
    }
  }, true);

  // busyInput reports whether someone is working in a form inside root: a
  // form control has focus, or an input holds a change that was not
  // submitted yet.
  function busyInput(root) {
    var a = document.activeElement;
    if (a && root.contains(a) && /^(INPUT|TEXTAREA|SELECT|BUTTON)$/.test(a.tagName)) return true;
    var dirty = false;
    root.querySelectorAll("input, textarea, select").forEach(function (el) {
      if (el.tagName === "SELECT") {
        for (var i = 0; i < el.options.length; i++) {
          if (el.options[i].selected !== el.options[i].defaultSelected) dirty = true;
        }
      } else if (el.type === "checkbox" || el.type === "radio") {
        if (el.checked !== el.defaultChecked) dirty = true;
      } else if (el.type !== "hidden" && el.type !== "submit" && el.type !== "button") {
        if (el.value !== el.defaultValue) dirty = true;
      }
    });
    return dirty;
  }

  // Live updates replace whole page sections. A section with a half-typed
  // ignore reason or snooze date keeps its old content until the input is
  // left alone; the skipped update is fetched then.
  var postponed = [];

  function catchUp() {
    postponed = postponed.filter(function (t) {
      if (!document.body.contains(t)) return false;
      if (busyInput(t)) return true;
      t.classList.remove("updates-paused");
      htmx.trigger(t, "sse:update");
      return false;
    });
  }

  document.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.hasAttribute("hx-trigger") && busyInput(t)) {
      e.detail.shouldSwap = false;
      t.classList.add("updates-paused");
      if (postponed.indexOf(t) < 0) postponed.push(t);
      return;
    }
    rememberOpen(document);
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    restoreOpen(document);
    applyGridFilter();
    applySiteFilter();
    stampAgo(document);
  });

  // "last scan 21:52 (3 minutes ago)" (W-056): data-ago is the age in
  // seconds when syncwatch wrote the element. The age counts on while the
  // page is open, in the words of agoWords in scan.go: a live update comes
  // only when something changes.
  function plural(n, one, many) {
    return n === 1 ? "1 " + one : n + " " + many;
  }

  function agoWords(s) {
    var m = Math.floor(Math.max(s, 0) / 60);
    if (m < 1) return "less than 1 minute ago";
    if (m < 60) return plural(m, "minute", "minutes") + " ago";
    if (m < 24 * 60) return plural(Math.floor(m / 60), "hour", "hours") + " ago";
    return plural(Math.floor(m / (24 * 60)), "day", "days") + " ago";
  }

  // stampAgo notes when each new data-ago element arrived on the page.
  function stampAgo(root) {
    var now = Date.now();
    root.querySelectorAll("[data-ago]").forEach(function (el) {
      if (!el.swAgoFrom) el.swAgoFrom = now;
    });
  }

  setInterval(function () {
    stampAgo(document);
    var now = Date.now();
    document.querySelectorAll("[data-ago]").forEach(function (el) {
      var s = (parseInt(el.getAttribute("data-ago"), 10) || 0) + (now - el.swAgoFrom) / 1000;
      el.textContent = agoWords(s);
    });
  }, 30000);

  // Focus moves before focusout settles, so look again a moment later.
  document.addEventListener("focusout", function () { setTimeout(catchUp, 0); });
  document.addEventListener("change", function () { setTimeout(catchUp, 0); });

  document.addEventListener("submit", function (e) {
    var f = e.target;
    var msg = f.getAttribute && f.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  }, true);

  // The live stream. When it stops for more than 30 s, the page says so: a
  // banner with the time, a grey health line, no "Real-time view". When it
  // works again, or when the page shows again after more than a minute in
  // the background, every live section is fetched again (the guard above
  // still keeps a form that is in use).
  var conn = { source: null, errorAt: 0, offline: false, timer: null, lastEvent: Date.now(), hiddenAt: 0 };

  // zoned writes a time as an ISO text in the configured time zone of
  // syncwatch; clock gives its HH:MM.
  function zoned(ms) {
    var b = document.body;
    var off = (b && parseInt(b.getAttribute("data-tz-offset") || "0", 10)) || 0;
    return new Date(ms + off * 60000).toISOString();
  }

  function clock(ms) {
    return zoned(ms).slice(11, 16);
  }

  function showOffline(since) {
    if (conn.offline) return;
    conn.offline = true;
    document.body.classList.add("sw-offline");
    var b = document.getElementById("sw-conn");
    if (b) {
      var t = b.querySelector(".sw-conn-since");
      if (t) t.textContent = clock(since);
      b.hidden = false;
    }
  }

  // backOnline clears the "no connection" state. It reports whether the
  // page missed updates since the last time it was connected.
  function backOnline() {
    clearTimeout(conn.timer);
    conn.timer = null;
    var missed = conn.errorAt !== 0 || conn.offline;
    conn.errorAt = 0;
    if (conn.offline) {
      conn.offline = false;
      document.body.classList.remove("sw-offline");
      var b = document.getElementById("sw-conn");
      if (b) b.hidden = true;
    }
    return missed;
  }

  function refreshAll() {
    document.querySelectorAll("[hx-trigger~='sse:update']").forEach(function (el) {
      htmx.trigger(el, "sse:update");
    });
  }

  function watchSource(source) {
    if (!source || source === conn.source) return;
    conn.source = source;
    var seen = function () { conn.lastEvent = Date.now(); };
    source.addEventListener("ping", seen);
    source.addEventListener("update", seen);
  }

  document.addEventListener("htmx:sseOpen", function (e) {
    watchSource(e.detail && e.detail.source);
    conn.lastEvent = Date.now();
    if (backOnline()) refreshAll();
  });

  document.addEventListener("htmx:sseError", function (e) {
    watchSource(e.detail && e.detail.source);
    if (conn.errorAt === 0) conn.errorAt = Date.now();
    if (!conn.timer && !conn.offline) {
      conn.timer = setTimeout(function () {
        conn.timer = null;
        if (conn.errorAt !== 0) showOffline(conn.errorAt);
      }, 30000);
    }
  });

  // A connection can stop without an error (a VPN or a phone network that
  // stops sending). The server sends a "ping" event every 25 s: after 60 s
  // without an event, the page shows that its data is old and opens the
  // stream again through the sse extension (close, then its own error path).
  setInterval(function () {
    var s = conn.source;
    if (!s || document.hidden || Date.now() - conn.lastEvent < 60000) return;
    if (conn.errorAt === 0) conn.errorAt = conn.lastEvent;
    showOffline(conn.lastEvent);
    conn.lastEvent = Date.now(); // the next try in a minute
    if (s.readyState === EventSource.OPEN && typeof s.onerror === "function") {
      s.close();
      s.onerror(new Event("error"));
    }
  }, 10000);

  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      conn.hiddenAt = Date.now();
      return;
    }
    var away = conn.hiddenAt !== 0 && Date.now() - conn.hiddenAt > 60000;
    conn.hiddenAt = 0;
    conn.lastEvent = Date.now(); // a page in the background can get no events
    if (away) refreshAll();
  });

  // Times of today show without the date ("connected from 08:12"). A
  // section with such times has data-day (its day in the configured zone):
  // when the day changes, the page fetches it again one time, so that a time
  // of yesterday gets its date.
  var today = zoned(Date.now()).slice(0, 10);
  setInterval(function () {
    var d = zoned(Date.now()).slice(0, 10);
    if (d === today) return;
    today = d;
    document.querySelectorAll("[data-day]").forEach(function (el) {
      if (el.getAttribute("data-day") !== d) htmx.trigger(el, "sse:update");
    });
  }, 60000);

  document.addEventListener("DOMContentLoaded", function () {
    var q = document.getElementById("grid-search");
    var p = document.getElementById("grid-problems");
    if (q && p) {
      try {
        var saved = JSON.parse(sessionStorage.getItem("sw-grid") || "null");
        if (saved) { q.value = saved.q || ""; p.checked = !!saved.p; }
      } catch (e) { /* ignore */ }
      q.addEventListener("input", applyGridFilter);
      p.addEventListener("change", applyGridFilter);
      applyGridFilter();
    }
    initSiteFilter();
    applySiteFilter();
    stampAgo(document);
    rememberOpen(document);
    openFromHash();
    window.addEventListener("hashchange", openFromHash);
  });
})();
