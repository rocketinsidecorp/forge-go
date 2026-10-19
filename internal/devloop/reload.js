// Forge development reload client. Served by `forge dev` only, never by the
// application, and only with FORGE_ENV=development.
(function () {
  "use strict";
  if (window.__forgeReload) { return; }
  window.__forgeReload = true;

  var boot = null;
  var banner = null;

  function hide() {
    if (banner && banner.parentNode) { banner.parentNode.removeChild(banner); }
    banner = null;
  }

  function show(message) {
    hide();
    banner = document.createElement("pre");
    // Set through the CSSOM: a style attribute would be blocked by a strict CSP.
    banner.style.cssText = "position:fixed;left:0;right:0;bottom:0;max-height:50vh;overflow:auto;" +
      "margin:0;padding:12px 16px;background:#7f1d1d;color:#fff;font:13px/1.4 monospace;" +
      "white-space:pre-wrap;z-index:2147483647";
    banner.textContent = "forge dev: build failed. The previous build is still running.\n\n" + message;
    document.body.appendChild(banner);
  }

  var source = new EventSource("/__forge/events");
  source.addEventListener("hello", function (event) {
    // A new boot id means forge dev itself restarted.
    if (boot !== null && boot !== event.data) { window.location.reload(); }
    boot = event.data;
  });
  source.addEventListener("reload", function () { window.location.reload(); });
  source.addEventListener("build-error", function (event) {
    var detail = {};
    try { detail = JSON.parse(event.data); } catch (e) { /* show the generic text */ }
    show(detail.message || "see the terminal running forge dev.");
  });
})();
