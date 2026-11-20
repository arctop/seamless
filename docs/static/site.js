/* thereisnospoon.org -- theme toggle, OS switch, reading switch, copy
   buttons, the comparison matrix, scroll reveals. No dependencies, no
   network, no state beyond localStorage("theme"), localStorage("os") and
   sessionStorage's reading. */
(function () {
  "use strict";

  /* theme toggle: explicit choice wins; otherwise follow the OS */
  var root = document.documentElement;
  var toggle = document.querySelector(".theme-toggle");
  function effectiveTheme() {
    if (root.dataset.theme) return root.dataset.theme;
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }
  /* An explicit toggle overrides the OS preference, so the browser chrome
     color must follow the page, not the media query the metas were born with. */
  function syncThemeColor() {
    var paper = getComputedStyle(root).getPropertyValue("--paper").trim();
    if (!paper) return;
    document.querySelectorAll('meta[name="theme-color"]').forEach(function (m) {
      m.setAttribute("content", paper);
    });
  }
  if (toggle) {
    toggle.addEventListener("click", function () {
      var next = effectiveTheme() === "dark" ? "light" : "dark";
      root.dataset.theme = next;
      try { localStorage.setItem("theme", next); } catch (e) { /* private mode */ }
      syncThemeColor();
    });
  }

  /* os switch: the head script already set data-os from the UA; a pick
     overrides it everywhere on the page and persists. aria-pressed mirrors
     data-os for assistive tech; the visual state is pure CSS off <html>. */
  function syncOsButtons() {
    document.querySelectorAll("[data-os-pick]").forEach(function (btn) {
      btn.setAttribute("aria-pressed", btn.dataset.osPick === root.dataset.os ? "true" : "false");
    });
  }
  syncOsButtons();
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-os-pick]");
    if (!btn) return;
    root.dataset.os = btn.dataset.osPick;
    /* The docs picker shares this key with a finer vocabulary (macos, linux,
       windows, all). Store the UA-refined canonical value while this page
       keeps its coarse unix|windows display, so a pick made here survives
       into the docs instead of being re-UA-detected there. */
    var stored = btn.dataset.osPick === "unix"
      ? (/Mac/i.test(navigator.userAgent) ? "macos" : "linux") : "windows";
    try { localStorage.setItem("os", stored); } catch (e) { /* private mode */ }
    syncOsButtons();
  });

  /* mobile nav menu: the native details disclosure needs no JS to open;
     closing on pick or Escape is the whole enhancement */
  var navMenu = document.querySelector(".nav-menu");
  if (navMenu) {
    navMenu.addEventListener("click", function (ev) {
      if (ev.target.closest("a")) navMenu.open = false;
    });
    document.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape" && navMenu.open) navMenu.open = false;
    });
  }

  /* the reading switch: a ledger read in plain words or in technical terms.
     The head script restores the pick before paint (sessionStorage, so it
     holds across the site's pages for this visit only); without JS no
     data-reading is set and both readings show under their labels. */
  var READ_KEY = "seamless.reading";
  if (document.querySelector("[data-reading-pick]")) {
    var syncReading = function () {
      var mode = root.dataset.reading === "tech" ? "tech" : "plain";
      root.dataset.reading = mode;
      document.querySelectorAll("[data-reading-pick]").forEach(function (b) {
        b.setAttribute("aria-pressed", b.dataset.readingPick === mode ? "true" : "false");
      });
    };
    syncReading();
    document.addEventListener("click", function (ev) {
      var b = ev.target.closest("[data-reading-pick]");
      if (!b) return;
      root.dataset.reading = b.dataset.readingPick;
      try { sessionStorage.setItem(READ_KEY, b.dataset.readingPick); } catch (e) { /* private mode */ }
      syncReading();
      var live = document.getElementById("announce");
      if (live) {
        live.textContent = "";
        setTimeout(function () {
          live.textContent = b.dataset.readingPick === "tech" ? "Showing the technical reading" : "Showing the plain reading";
        }, 60);
      }
    });
  }

  /* the comparison matrix (the landing page and /compare/): picking a tool,
     from its column head or from the pills narrow screens show, lights that
     column and prints when to choose it over Seamless. The sentence, the
     tool's one-line description and its link ride on the column head as
     data attributes, so the markup stays the single copy of the words.
     Without JS the whole table shows and scrolls sideways. */
  document.querySelectorAll("[data-mtx]").forEach(function (box) {
    var out = box.querySelector("[data-mtx-out]");
    function pick(id) {
      var th = box.querySelector('thead th[data-col="' + id + '"]');
      if (!th) return;
      box.dataset.focus = id;
      box.querySelectorAll("[data-col]").forEach(function (c) {
        c.classList.toggle("is-focus", c.dataset.col === id);
      });
      box.querySelectorAll("[data-mtx-pick]").forEach(function (b) {
        b.setAttribute("aria-pressed", b.dataset.mtxPick === id ? "true" : "false");
      });
      if (!out) return;
      var name = th.querySelector(".mtx-name").firstChild.textContent;
      var why = th.dataset.pick || "";
      out.textContent = "";
      var lead = document.createElement("b");
      lead.textContent = "Choose " + name + " instead if";
      out.appendChild(lead);
      out.appendChild(document.createTextNode(" " + why.charAt(0).toLowerCase() + why.slice(1) + " "));
      if (th.dataset.href) {
        var a = document.createElement("a");
        a.href = th.dataset.href;
        a.textContent = "More on " + name;
        out.appendChild(a);
      }
    }
    box.addEventListener("click", function (ev) {
      var b = ev.target.closest("[data-mtx-pick]");
      if (b) pick(b.dataset.mtxPick);
    });
    pick(box.dataset.focus);
  });

  /* copy buttons: copy the nearest [data-copy] text */
  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest(".copy-btn");
    if (!btn) return;
    var scope = btn.closest(".install-pill, .code");
    var src = scope && scope.querySelector("[data-copy]");
    if (!src) return;
    var flash = function (label, cls) {
      btn.textContent = label;
      btn.classList.add(cls);
      setTimeout(function () {
        btn.textContent = "copy";
        btn.classList.remove(cls);
      }, 1600);
    };
    /* No clipboard API (plain-HTTP origins), or it refused: select the
       visible command -- the same text the attribute carries -- so one
       keystroke finishes the copy. */
    var fallback = function () {
      var range = document.createRange();
      range.selectNodeContents(src);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      flash("selected", "sel");
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(src.getAttribute("data-copy"))
        .then(function () { flash("copied", "ok"); }, fallback);
    } else {
      fallback();
    }
  });

  /* reveal on scroll */
  if ("IntersectionObserver" in window) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) {
          e.target.classList.add("in");
          io.unobserve(e.target);
        }
      });
    }, { rootMargin: "0px 0px -8% 0px" });
    document.querySelectorAll(".rv").forEach(function (el) { io.observe(el); });
  } else {
    document.querySelectorAll(".rv").forEach(function (el) { el.classList.add("in"); });
  }
})();
