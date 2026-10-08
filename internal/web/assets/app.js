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

  function openFromHash() {
    if (!location.hash || location.hash.indexOf("#f-") !== 0) return;
    var el = document.getElementById(location.hash.slice(1));
    if (!el) return;
    var d = el.querySelector("details");
    if (d) {
      d.open = true;
      openKeys[d.getAttribute("data-key")] = true;
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

  document.addEventListener("htmx:beforeSwap", function (e) {
    rememberOpen(document);
  });

  document.addEventListener("htmx:afterSwap", function (e) {
    restoreOpen(document);
    applyGridFilter();
  });

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
