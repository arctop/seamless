/* thereisnospoon.org -- the shared motion and interactive pieces of the
   hand-written pages beyond the landing (/claude-app/, /security/, /compare/).
   home.js drives the landing's own sections (its sky, the week, the orbit,
   the welcome-back briefing) and is not loaded with this; the behaviors here
   follow it: the header and its progress line, the chapter rail, the
   headline rise, staggered reveals, tablists, the matter chart, the copy
   burst. site.js still owns the theme, the OS switch, the copy buttons, the
   reading switch and the .rv reveal. Without JS every page reads complete. */
(function () {
  "use strict";
  var root = document.documentElement;
  var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  function $(s, c) { return (c || document).querySelector(s); }
  function $$(s, c) { return Array.prototype.slice.call((c || document).querySelectorAll(s)); }
  function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }
  function onceVisible(el, cb, threshold) {
    if (!("IntersectionObserver" in window)) { cb(); return; }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) { if (e.isIntersecting) { io.disconnect(); cb(); } });
    }, { threshold: threshold || 0.25 });
    io.observe(el);
  }
  var announcer = $("#announce");
  function say(text) {
    if (!announcer) return;
    announcer.textContent = "";
    setTimeout(function () { announcer.textContent = text; }, 60);
  }

  /* the headline rise and staggered reveals, as on the landing */
  function splitWords(el) {
    var i = 0;
    (function walk(node) {
      Array.prototype.slice.call(node.childNodes).forEach(function (n) {
        if (n.nodeType === 3) {
          var frag = document.createDocumentFragment();
          n.textContent.split(/(\s+)/).forEach(function (part) {
            if (!part) return;
            if (/^\s+$/.test(part)) { frag.appendChild(document.createTextNode(part)); return; }
            var w = document.createElement("span");
            w.className = "w";
            var wi = document.createElement("span");
            wi.className = "wi";
            wi.textContent = part;
            wi.style.setProperty("--i", i++);
            w.appendChild(wi);
            frag.appendChild(w);
          });
          n.parentNode.replaceChild(frag, n);
        } else if (n.nodeType === 1) {
          walk(n);
        }
      });
    })(el);
  }
  var groups = new Map();
  $$(".rv").forEach(function (el) {
    var n = groups.get(el.parentNode) || 0;
    el.style.setProperty("--d", Math.min(n, 6) * 90 + "ms");
    groups.set(el.parentNode, n + 1);
  });
  if (!reduced) {
    $$("[data-split]").forEach(function (el) {
      splitWords(el);
      if (el.classList.contains("h1")) {
        var go = function () { setTimeout(function () { el.classList.add("is-in"); }, 120); };
        if (document.fonts && document.fonts.ready) document.fonts.ready.then(go); else go();
        setTimeout(function () { el.classList.add("is-in"); }, 1500);
      } else {
        onceVisible(el, function () { el.classList.add("is-in"); }, 0.4);
      }
    });
  }

  /* the header, the progress line, the rail, the horizons, the starfield */
  var head = $("[data-head]");
  var progress = head && $(".head-progress i", head);
  var rail = $(".rail");
  var railLinks = rail ? $$("a[data-rail]", rail) : [];
  var railFill = rail && $(".rail-line i", rail);
  var chapters = $$("[data-chapter]");
  var horizons = $$(".chapter");
  var navLinks = $$(".head-nav a[href^='#']");
  var starfield = $(".starfield");
  var heroEl = $("[data-hero]");
  var finaleEl = $(".finale");
  function onScroll() {
    var y = window.scrollY || window.pageYOffset;
    var vh = window.innerHeight;
    if (head) head.classList.toggle("is-solid", y > 24);
    if (progress) progress.style.setProperty("--p", clamp(y / Math.max(1, root.scrollHeight - vh), 0, 1).toFixed(4));
    var line = vh * 0.42, cur = -1, frac = 0;
    for (var i = 0; i < chapters.length; i++) {
      var r = chapters[i].getBoundingClientRect();
      if (r.top <= line) { cur = i; frac = clamp((line - r.top) / Math.max(1, r.height), 0, 1); }
    }
    for (var c = 0; c < horizons.length; c++) horizons[c].classList.toggle("is-reached", horizons[c].getBoundingClientRect().top <= line);
    var curId = cur >= 0 ? chapters[cur].id : "";
    railLinks.forEach(function (a, k) {
      a.classList.toggle("is-current", k === cur);
      a.classList.toggle("is-past", k < cur);
      if (k === cur) a.setAttribute("aria-current", "true"); else a.removeAttribute("aria-current");
    });
    if (railFill) railFill.style.setProperty("--fill", clamp((cur + frac) / Math.max(1, chapters.length - 1), 0, 1).toFixed(4));
    var pastHero = heroEl ? heroEl.getBoundingClientRect().bottom < vh * 0.5 : true;
    var atEnd = finaleEl ? finaleEl.getBoundingClientRect().top < vh * 0.55 : false;
    if (rail) rail.classList.toggle("is-on", pastHero && !atEnd && cur >= 0);
    navLinks.forEach(function (a) { a.classList.toggle("is-current", a.getAttribute("href") === "#" + curId); });
    if (starfield) {
      starfield.classList.toggle("is-on", pastHero);
      if (!reduced) starfield.style.transform = "translate3d(0," + (-y * 0.02).toFixed(1) + "px,0)";
    }
  }
  var ticking = false;
  function requestScroll() {
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(function () { ticking = false; onScroll(); });
  }
  window.addEventListener("scroll", requestScroll, { passive: true });
  window.addEventListener("resize", requestScroll);
  onScroll();

  /* tablists (the desk switcher, the first-chats index): WAI-ARIA tabs, each
     panel shown until this hides all but the first */
  $$("[data-tabs], [data-desks], [data-asks]").forEach(function (box) {
    var tabs = $$('[role="tab"]', box);
    if (!tabs.length) return;
    var pick = function (tab, focus) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.classList.toggle("active", on);
        t.tabIndex = on ? 0 : -1;
        var panel = document.getElementById(t.getAttribute("aria-controls"));
        panel.hidden = !on;
        panel.classList.toggle("is-shown", on);
      });
      if (focus) tab.focus();
    };
    tabs.forEach(function (t, i) {
      t.addEventListener("click", function () { pick(t, false); });
      t.addEventListener("keydown", function (ev) {
        var k = ev.key;
        var j = k === "ArrowRight" || k === "ArrowDown" ? i + 1
          : k === "ArrowLeft" || k === "ArrowUp" ? i - 1
          : k === "Home" ? 0 : k === "End" ? tabs.length - 1 : null;
        if (j === null) return;
        ev.preventDefault();
        pick(tabs[(j + tabs.length) % tabs.length], true);
      });
    });
    tabs.forEach(function (t) { document.getElementById(t.getAttribute("aria-controls")).hidden = t !== tabs[0]; });
  });

  /* the matter chart: point at a star to read its entry */
  var chart = $("[data-chart]");
  if (chart) {
    var tip = $(".sky-tip", chart);
    var project = chart.getAttribute("data-chart") || "";
    var hot = null;
    var show = function (st) {
      if (hot) hot.classList.remove("is-hot");
      hot = st;
      st.classList.add("is-hot");
      tip.textContent = "";
      var path = document.createElement("p");
      path.className = "tp-path";
      path.textContent = "~/.seamless/" + st.getAttribute("data-f") + "/" + project + "/" + st.getAttribute("data-n") + ".md";
      var kind = document.createElement("span");
      kind.className = "tp-kind";
      kind.textContent = st.classList.contains("is-was") ? "decision · replaced" : st.getAttribute("data-k");
      kind.style.background = getComputedStyle(st).color;
      var desc = document.createElement("p");
      desc.className = "tp-desc";
      desc.textContent = st.getAttribute("data-d");
      tip.appendChild(path); tip.appendChild(kind); tip.appendChild(desc);
      var cr = chart.getBoundingClientRect(), sr = st.getBoundingClientRect();
      var x = sr.left + sr.width / 2 - cr.left, y = sr.top + sr.height / 2 - cr.top;
      var w = tip.offsetWidth, h = tip.offsetHeight;
      var left = clamp(x + 16, 0, cr.width - w);
      var top = y - h - 14 < 0 ? y + 16 : y - h - 14;
      tip.style.transform = "translate(" + left.toFixed(0) + "px," + top.toFixed(0) + "px)";
      tip.classList.add("is-on");
    };
    var hide = function () {
      if (hot) hot.classList.remove("is-hot");
      hot = null;
      tip.classList.remove("is-on");
    };
    $$(".c-st", chart).forEach(function (st) {
      st.addEventListener("pointerenter", function () { show(st); });
      st.addEventListener("pointerleave", hide);
      st.addEventListener("click", function (ev) { ev.stopPropagation(); if (hot === st) hide(); else show(st); });
    });
    document.addEventListener("click", hide);
  }

  /* the file: point at a note to light its line */
  $$(".file-notes li[data-for]").forEach(function (li) {
    var line = $('.fl[data-note="' + li.getAttribute("data-for") + '"]');
    if (!line) return;
    var on = function () { li.classList.add("is-hot"); line.classList.add("is-hot"); };
    var off = function () { li.classList.remove("is-hot"); line.classList.remove("is-hot"); };
    li.addEventListener("pointerenter", on);
    li.addEventListener("pointerleave", off);
  });

  /* copy confirmations for assistive tech, and the landing's small burst */
  document.addEventListener("click", function (e) {
    var btn = e.target.closest(".copy-btn");
    if (!btn) return;
    var mo = new MutationObserver(function () {
      if (btn.classList.contains("ok")) { say("Copied to clipboard"); mo.disconnect(); }
      else if (btn.classList.contains("sel")) { say("Text selected. Press Command C or Control C to copy it."); mo.disconnect(); }
    });
    mo.observe(btn, { attributes: true, attributeFilter: ["class"] });
    setTimeout(function () { mo.disconnect(); }, 2500);
    if (reduced) return;
    var r = btn.getBoundingClientRect();
    var b = document.createElement("span");
    b.className = "burst";
    b.style.left = (r.left + r.width / 2) + "px";
    b.style.top = (r.top + r.height / 2) + "px";
    for (var i = 0; i < 9; i++) {
      var dot = document.createElement("i");
      dot.style.setProperty("--a", (i * 40 + Math.random() * 18) + "deg");
      dot.style.setProperty("--r", (22 + Math.random() * 18) + "px");
      b.appendChild(dot);
    }
    document.body.appendChild(b);
    setTimeout(function () { b.remove(); }, 900);
  });

  /* the phone menu closes on a tap outside it or on scroll */
  var nav = $(".nav-menu");
  if (nav) {
    document.addEventListener("click", function (e) { if (nav.open && !nav.contains(e.target)) nav.open = false; });
    window.addEventListener("scroll", function () { if (nav.open) nav.open = false; }, { passive: true });
  }

  /* the finale's mark draws itself */
  if (finaleEl && !reduced) {
    finaleEl.classList.add("is-armed");
    onceVisible(finaleEl, function () { finaleEl.classList.add("is-in"); }, 0.35);
  }
})();
