// syncwatch: keeps fold-outs open across live updates, filters the grid and
// asks for confirmation on destructive forms. No inline scripts (CSP).
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
  });

  // Focus moves before focusout settles, so look again a moment later.
  document.addEventListener("focusout", function () { setTimeout(catchUp, 0); });
  document.addEventListener("change", function () { setTimeout(catchUp, 0); });

  document.addEventListener("submit", function (e) {
    var f = e.target;
    var msg = f.getAttribute && f.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  }, true);

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
    rememberOpen(document);
    openFromHash();
    window.addEventListener("hashchange", openFromHash);
  });
})();
