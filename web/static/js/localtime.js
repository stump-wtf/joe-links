// Viewer-local timestamp rendering — progressive enhancement.
//
// Rewrites every <time datetime="..."> element (server-stamped UTC RFC 3339,
// never user-controlled) to the viewer's local timezone via the browser's
// Intl.DateTimeFormat (no library, no shipped timezone data), keeping the UTC
// form in the title attribute for hover. Re-runs after HTMX swaps
// (htmx:afterSwap) so fragments localize too. Makes no network requests; DOM
// writes go through textContent and setAttribute only — never innerHTML. With
// JavaScript unavailable the server-rendered UTC text remains.
//
// Governing: SPEC-0021 REQ "Viewer-Local Timestamps", ADR-0021
(function () {
  "use strict";

  var formatter = new Intl.DateTimeFormat(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short"
  });

  function localize(root) {
    if (!root || typeof root.querySelectorAll !== "function") {
      root = document;
    }
    var nodes = root.querySelectorAll("time[datetime]");
    for (var i = 0; i < nodes.length; i++) {
      var el = nodes[i];
      if (el.getAttribute("data-localized") === "true") {
        continue;
      }
      var utc = el.getAttribute("datetime");
      var parsed = new Date(utc);
      if (isNaN(parsed.getTime())) {
        continue;
      }
      el.setAttribute("title", utc); // preserve the UTC form on hover
      el.textContent = formatter.format(parsed);
      el.setAttribute("data-localized", "true");
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () {
      localize(document);
    });
  } else {
    localize(document);
  }

  // HTMX fragment swaps bring new <time> elements; localize the swapped subtree.
  document.addEventListener("htmx:afterSwap", function (evt) {
    localize(evt.target);
  });
})();
