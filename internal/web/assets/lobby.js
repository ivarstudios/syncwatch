// syncwatch lobby page: when the server refuses a live update because the
// login ended (the session expired, or the viewer password changed), open
// the login page with this lobby page as the next page. After the login the
// screen shows the same lobby again, with its site and theme. Without this,
// htmx opens the login page alone and the login goes to the Overview.
// No inline scripts (CSP).
(function () {
  "use strict";
  document.addEventListener("htmx:beforeOnLoad", function (e) {
    var xhr = e.detail && e.detail.xhr;
    if (!xhr || xhr.status !== 401 || !xhr.getResponseHeader("HX-Redirect")) return;
    e.preventDefault();
    window.location.href = "/login?next=" + encodeURIComponent(window.location.pathname + window.location.search);
  });
})();
