// Progressive enhancement only: every page works without this file.
(function () {
  "use strict";

  // Copy buttons, wired by delegation so htmx swaps keep working.
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest(".copy-btn");
    if (!btn) return;
    var text = btn.getAttribute("data-copy") || "";
    var done = function () {
      var was = btn.textContent;
      btn.textContent = "Copied";
      btn.classList.add("copied");
      setTimeout(function () {
        btn.textContent = was;
        btn.classList.remove("copied");
      }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, fallback);
    } else {
      fallback();
    }
    function fallback() {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); done(); } catch (e) { /* ignore */ }
      document.body.removeChild(ta);
    }
  });

  // Live indicator driven by the shell's single SSE connection.
  var dot = null;
  function setLive(on) {
    dot = dot || document.getElementById("live-dot");
    if (dot) dot.classList.toggle("live", !!on);
  }
  document.body.addEventListener("htmx:sseOpen", function () { setLive(true); });
  document.body.addEventListener("htmx:sseError", function () { setLive(false); });
  document.body.addEventListener("htmx:sseClose", function () { setLive(false); });
})();
