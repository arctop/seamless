/* thereisnospoon.org/example/ -- the explorer over the copied project.

   The page ships every section and every record as plain stacked HTML, so it
   reads complete without this file. Here the explorer goes live: one section
   and one record on show at a time, a list beside the reader, and the address
   bar kept on the record that is open, so a link to it opens it again.

   Anything on the page that carries data-open="<id>" opens that record,
   wherever it sits; a tab (data-sec) opens its section at the record last
   read there. No dependencies. */
(function () {
  "use strict";
  var ex = document.querySelector("[data-ex]");
  if (!ex || !ex.querySelector(".ex-sec")) return;

  function $$(s, c) { return Array.prototype.slice.call((c || document).querySelectorAll(s)); }
  var secs = $$(".ex-sec", ex);
  var tabs = $$(".ex-tab", ex);
  var announcer = document.getElementById("announce");
  var wide = window.matchMedia("(min-width: 1000px)");
  var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var last = {};   /* section id -> the record last open there */

  function say(text) {
    if (!announcer) return;
    announcer.textContent = "";
    setTimeout(function () { announcer.textContent = text; }, 60);
  }
  function mark(el, on) {
    el.classList.toggle("is-on", on);
    if (on) el.setAttribute("aria-current", "true"); else el.removeAttribute("aria-current");
  }
  /* bring a row into its list's view without moving the page */
  function reveal(list, row) {
    var lr = list.getBoundingClientRect(), rr = row.getBoundingClientRect();
    if (rr.top < lr.top + 8) list.scrollTop -= lr.top + 8 - rr.top;
    else if (rr.bottom > lr.bottom - 8) list.scrollTop += rr.bottom - lr.bottom + 8;
  }

  /* open(id): id names a section or a record inside one */
  function open(id, opts) {
    opts = opts || {};
    var el = document.getElementById(id);
    if (!el || !ex.contains(el)) return false;
    var isSec = el.classList.contains("ex-sec");
    var sec = isSec ? el : el.closest(".ex-sec");
    if (!sec) return false;
    var records = $$(".ex-item", sec);
    var item = isSec ? (document.getElementById(last[sec.id]) || records[0]) : el.closest(".ex-item");
    if (!item) return false;

    secs.forEach(function (s) { s.hidden = s !== sec; });
    tabs.forEach(function (t) { mark(t, t.getAttribute("data-sec") === sec.id); });
    records.forEach(function (r) { r.hidden = r !== item; });
    last[sec.id] = item.id;

    var list = sec.querySelector(".ex-list");
    var row = null;
    $$(".ex-row[data-open]", sec).forEach(function (r) {
      var on = r.getAttribute("data-open") === item.id;
      mark(r, on);
      if (on) row = r;
    });
    var read = sec.querySelector(".ex-read");
    if (read) read.scrollTop = 0;
    if (list && row) reveal(list, row);
    var tab = ex.querySelector(".ex-tab.is-on");
    if (tab && tab.scrollIntoView && opts.tab) tab.scrollIntoView({ block: "nearest", inline: "nearest" });

    if (opts.hash !== false) {
      try { history.replaceState(null, "", "#" + (isSec ? sec.id : item.id)); } catch (e) { /* file:// */ }
    }
    if (opts.scroll) {
      /* on a wide screen the whole explorer comes into view; stacked, the
         reader does, because the list sits above it */
      var target = wide.matches || isSec || !read ? ex : read;
      var top = target.getBoundingClientRect().top + window.pageYOffset - 82;
      window.scrollTo({ top: Math.max(0, top), behavior: reduced ? "auto" : "smooth" });
    }
    if (opts.focus) { try { item.focus({ preventScroll: true }); } catch (e) { /* older engines */ } }
    if (opts.say) {
      var name = item.querySelector("h3");
      say("Showing " + (name ? name.textContent : sec.id));
    }
    return true;
  }

  ex.classList.add("is-live");

  document.addEventListener("click", function (e) {
    if (e.defaultPrevented || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest ? e.target.closest("a[data-open], a[data-sec]") : null;
    if (!a) return;
    var id = a.getAttribute("data-open") || a.getAttribute("data-sec");
    var inside = ex.contains(a);
    var fromList = !!a.closest(".ex-list");
    var r = ex.getBoundingClientRect();
    var away = r.top < 60 || r.top > window.innerHeight * 0.45;
    if (open(id, {
      /* a link from outside always brings the explorer up; inside, only when
         the reader is not where the eye is */
      scroll: !inside || (fromList ? !wide.matches : away && !a.classList.contains("ex-tab")),
      focus: !fromList, say: true, tab: true
    })) e.preventDefault();
  });

  /* up and down walk a list, opening as they go */
  ex.addEventListener("keydown", function (e) {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    var list = e.target.closest ? e.target.closest(".ex-list") : null;
    if (!list) return;
    var rows = $$("a.ex-row", list);
    var i = rows.indexOf(document.activeElement);
    if (i < 0) return;
    var next = rows[i + (e.key === "ArrowDown" ? 1 : -1)];
    if (!next) return;
    e.preventDefault();
    next.focus({ preventScroll: true });
    open(next.getAttribute("data-open"), { say: true });
  });

  function fromHash(first) {
    var id = decodeURIComponent((location.hash || "").slice(1));
    if (id && open(id, { hash: false, scroll: true, tab: true })) return;
    if (first) open(secs[0].id, { hash: false });
  }
  window.addEventListener("hashchange", function () { fromHash(false); });
  fromHash(true);
})();
