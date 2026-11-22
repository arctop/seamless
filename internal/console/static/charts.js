/* Seamless console -- the interaction layer for the server-rendered charts in
   charts.go and charts_marks.go: hover/focus readouts, and nothing else. Three
   parts, in this order: the trend readout of the value at a point in time on
   any .area[data-hover] chart (the injection trend and the coverage trend), the
   capture calendar's day readout, and the per-mark readouts on any
   .chart[data-hover] chart. Deliberately not a chart library: every number,
   label and unit it shows was formatted server-side and handed over in the
   data-hover payload, so this file only picks the point or mark under the
   pointer and places the crosshair and tooltip.

   The layer is rebuilt on demand from the payload rather than mounted once at
   load. The SSE live client morphs .main in place and strips any node absent
   from the freshly-fetched copy -- which is every node this file adds. Rebuilding
   on the next pointer/focus event survives that, and picks up the refreshed
   numbers for free. */
(function () {
  var NS = 'http://www.w3.org/2000/svg';
  var cache = new WeakMap(); // .area -> layer, valid while its payload is unchanged
  var active = null;         // the .area currently showing a readout

  function svgEl(name, attrs) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) e.setAttribute(k, String(attrs[k]));
    return e;
  }

  function div(cls, text) {
    var e = document.createElement('div');
    e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  function build(area, raw) {
    var data;
    try { data = JSON.parse(raw); } catch (e) { return null; }
    if (!data || !data.pts || !data.pts.length) return null;
    var svg = area.querySelector('svg.area-svg');
    if (!svg) return null;

    var rule = svgEl('line', { class: 'hov-rule', y1: data.top, y2: data.bot, 'vector-effect': 'non-scaling-stroke' });
    var dot = svgEl('circle', { class: 'hov-dot', r: 4.5, 'vector-effect': 'non-scaling-stroke' });
    var g = svgEl('g', { class: 'hov', 'aria-hidden': 'true' });
    g.appendChild(rule);
    g.appendChild(dot);
    // A transparent capture rect over the plot, added last so it sits on top: it
    // makes the gaps between points hoverable, and it masks the <title> fallbacks
    // on the coverage dots so the browser's own tooltip can't double up with ours.
    var hit = svgEl('rect', { class: 'hov-hit', x: 0, y: 0, width: data.w, height: data.h });
    svg.appendChild(g);
    svg.appendChild(hit);

    var tip = div('hov-tip');
    tip.hidden = true;
    area.appendChild(tip);

    var layer = { raw: raw, data: data, svg: svg, rule: rule, dot: dot, tip: tip, i: -1 };
    cache.set(area, layer);
    return layer;
  }

  function layerOf(area) {
    var raw = area.getAttribute('data-hover') || '';
    var l = cache.get(area);
    // isConnected catches the live morph having stripped the layer out from under us.
    if (l && l.raw === raw && l.tip.isConnected && l.svg.isConnected) return l;
    return build(area, raw);
  }

  // px maps viewBox units to page pixels. The svg carries a viewBox and is sized
  // width:100%/height:auto in CSS, so it scales uniformly -- one factor covers both axes.
  function px(l) {
    var r = l.svg.getBoundingClientRect();
    return { k: r.width / l.data.w || 1, left: r.left, top: r.top };
  }

  function nearest(l, clientX) {
    var s = px(l);
    var vx = (clientX - s.left) / s.k;
    var pts = l.data.pts, best = 0, bd = Infinity;
    for (var i = 0; i < pts.length; i++) {
      var d = Math.abs(pts[i].x - vx);
      if (d < bd) { bd = d; best = i; }
    }
    return best;
  }

  function place(area, l, p) {
    var s = px(l), ar = area.getBoundingClientRect();
    var cx = s.left - ar.left + p.x * s.k;
    var cy = s.top - ar.top + p.y * s.k;
    var w = l.tip.offsetWidth, h = l.tip.offsetHeight;
    // Centre on the point, then clamp inside the card; flip below when the point
    // sits too near the top for the tooltip to clear it.
    var left = Math.max(0, Math.min(cx - w / 2, area.clientWidth - w));
    var top = cy - h - 12;
    if (top < 0) top = cy + 14;
    l.tip.style.left = Math.round(left) + 'px';
    l.tip.style.top = Math.round(top) + 'px';
  }

  function show(area, l, i) {
    var p = l.data.pts[i];
    if (!p) return;
    if (l.i === i && !l.tip.hidden) return; // nothing moved -- don't re-measure
    l.rule.setAttribute('x1', p.x);
    l.rule.setAttribute('x2', p.x);
    l.dot.setAttribute('cx', p.x);
    l.dot.setAttribute('cy', p.y);
    l.tip.textContent = '';
    l.tip.appendChild(div('hov-t', p.label));
    l.tip.appendChild(div('hov-v', p.value));
    if (p.sub) l.tip.appendChild(div('hov-s', p.sub));
    l.tip.hidden = false;
    area.classList.add('hov-on');
    place(area, l, p); // after unhiding: the clamp needs the tooltip's real box
    l.i = i;
    active = area;
  }

  function hide(area) {
    var l = cache.get(area);
    if (l) { l.tip.hidden = true; l.i = -1; }
    area.classList.remove('hov-on');
    if (active === area) active = null;
  }

  function areaOf(t) { return t && t.closest ? t.closest('.area[data-hover]') : null; }

  // Delegated from the document: the charts are server-rendered and the live
  // client can replace them wholesale, so nothing may hold a node reference.
  function track(e) {
    var area = areaOf(e.target);
    if (!area) { if (active) hide(active); return; }
    if (active && active !== area) hide(active);
    var l = layerOf(area);
    if (l) show(area, l, nearest(l, e.clientX));
  }
  document.addEventListener('pointermove', track, { passive: true });
  document.addEventListener('pointerdown', track, { passive: true }); // touch: tap to read
  document.addEventListener('pointerleave', function () { if (active) hide(active); }, { passive: true });

  // Keyboard: the chart svg is focusable (tabindex is server-rendered), arrows
  // walk the series. Screen readers get the svg's aria-label summary instead --
  // the readout is a sighted-keyboard affordance, so it stays aria-hidden.
  document.addEventListener('focusin', function (e) {
    var area = areaOf(e.target);
    if (!area) { if (active) hide(active); return; }
    var l = layerOf(area);
    if (l) show(area, l, l.data.pts.length - 1); // land on the most recent bucket
  });
  document.addEventListener('focusout', function (e) {
    var area = areaOf(e.target);
    if (area) hide(area);
  });
  document.addEventListener('keydown', function (e) {
    var area = areaOf(e.target);
    if (!area) return;
    var l = layerOf(area);
    if (!l) return;
    var n = l.data.pts.length;
    var i = l.i < 0 ? n - 1 : l.i;
    var next;
    switch (e.key) {
      case 'ArrowLeft': next = Math.max(0, i - 1); break;
      case 'ArrowRight': next = Math.min(n - 1, i + 1); break;
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      case 'Escape': hide(area); return;
      default: return;
    }
    e.preventDefault();
    show(area, l, next);
  });
})();

/* Capture calendar (the Sessions page). The same contract as the trend readout
   above -- every string shown was formatted server-side -- but the cells are
   discrete rects, so the readout keys off each cell's own data-day and <title>
   rather than a nearest-point search. Three affordances:

   - Hover (or the keyboard cursor) shows the day's numbers in the shared
     .hov-tip readout, instantly, in place of the browser's sluggish native
     tooltip: enhance() lifts each <title> into a data-t attribute so the
     native one cannot double up. The morph restores <title>s from the fresh
     server copy; re-enhancing on seam:content-updated lifts them again.
   - Click (or Enter on the keyboard cursor) focuses the session map on that
     day via the shared no-reload path: it mints the ?day URL the server
     defined from the page's own URL, so every other filter (?project=
     among them) rides along; clicking the already-focused day clears it.
     Without script a cell is not a link: its <title> still reads the day.
   - Arrow keys walk days from today (the svg is server-rendered focusable):
     up/down one day, left/right one week, Home/End the span's edges. */
(function () {
  function grids() { return document.querySelectorAll('.mom-cal-grid'); }
  function cellsOf(grid) { return grid.querySelectorAll('.mom-cal-cell'); }

  function enhance() {
    grids().forEach(function (grid) {
      cellsOf(grid).forEach(function (cell) {
        var title = cell.querySelector('title');
        if (!title) return;
        cell.setAttribute('data-t', title.textContent);
        cell.removeChild(title);
      });
    });
  }
  enhance();
  document.addEventListener('seam:content-updated', enhance);

  function calOf(t) { return t && t.closest ? t.closest('.mom-cal') : null; }
  function cellOf(t) { return t && t.closest ? t.closest('.mom-cal-cell') : null; }

  function div(cls, text) {
    var e = document.createElement('div');
    e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  function tipOf(cal) {
    var tip = cal.querySelector(':scope > .hov-tip');
    if (!tip) {
      tip = div('hov-tip');
      tip.hidden = true;
      cal.appendChild(tip);
    }
    return tip;
  }

  function show(cell) {
    var cal = calOf(cell);
    if (!cal) return;
    var raw = cell.getAttribute('data-t') || '';
    var cut = raw.indexOf(' -- ');
    var tip = tipOf(cal);
    tip.textContent = '';
    tip.appendChild(div('hov-t', cut < 0 ? raw : raw.slice(0, cut)));
    tip.appendChild(div('hov-v', cut < 0 ? '' : raw.slice(cut + 4)));
    tip.appendChild(div('hov-s', cell.classList.contains('sel')
      ? 'Click to clear the day focus'
      : 'Click to focus the session map on this day'));
    tip.hidden = false;
    // Above the cell, centred and clamped inside the card; below when the top
    // row would clip it. Rect math, so the horizontal scroll is accounted for.
    var cr = cell.getBoundingClientRect(), ar = cal.getBoundingClientRect();
    var cx = cr.left - ar.left + cr.width / 2;
    var left = Math.max(0, Math.min(cx - tip.offsetWidth / 2, cal.clientWidth - tip.offsetWidth));
    var top = cr.top - ar.top - tip.offsetHeight - 10;
    if (top < 0) top = cr.bottom - ar.top + 10;
    tip.style.left = Math.round(left) + 'px';
    tip.style.top = Math.round(top) + 'px';
  }

  function hide(cal) {
    var tip = cal && cal.querySelector(':scope > .hov-tip');
    if (tip) tip.hidden = true;
    var kb = cal && cal.querySelector('.mom-cal-cell.kb');
    if (kb) kb.classList.remove('kb');
  }

  function open(cell) {
    var url = new URL(location.href);
    if (cell.classList.contains('sel')) url.searchParams.delete('day');
    else url.searchParams.set('day', cell.getAttribute('data-day') || '');
    if (window.SeamConsole && window.SeamConsole.load) {
      window.SeamConsole.load(url.href, { history: 'push', source: 'query' });
    } else {
      location.assign(url.href);
    }
  }

  document.addEventListener('pointerover', function (e) {
    var cell = cellOf(e.target);
    if (cell) { enhanceCell(cell); show(cell); }
    else hide(calOf(e.target));
  }, { passive: true });
  document.addEventListener('pointerout', function (e) {
    var cal = calOf(e.target);
    if (cal && !calOf(e.relatedTarget)) hide(cal);
  }, { passive: true });
  document.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var cell = cellOf(e.target);
    if (cell) open(cell);
  });

  // A cell hovered in the sliver between the morph restoring <title>s and the
  // enhance pass: lift lazily so the readout never shows stale emptiness.
  function enhanceCell(cell) {
    var title = cell.querySelector('title');
    if (!title) return;
    cell.setAttribute('data-t', title.textContent);
    cell.removeChild(title);
  }

  // Keyboard: the grid is one tab stop; arrows move a .kb cursor cell starting
  // from today (the last cell), Enter or Space opens it, Escape rests.
  function cursor(grid, cells, next) {
    var cur = grid.querySelector('.mom-cal-cell.kb');
    if (cur) cur.classList.remove('kb');
    var cell = cells[next];
    if (!cell) return;
    cell.classList.add('kb');
    enhanceCell(cell);
    show(cell);
  }
  document.addEventListener('focusin', function (e) {
    var t = e.target;
    if (t && t.classList && t.classList.contains('mom-cal-grid')) {
      var cells = cellsOf(t);
      cursor(t, cells, cells.length - 1);
    }
  });
  document.addEventListener('focusout', function (e) {
    var t = e.target;
    if (t && t.classList && t.classList.contains('mom-cal-grid')) hide(calOf(t));
  });
  document.addEventListener('keydown', function (e) {
    var t = e.target;
    if (!t || !t.classList || !t.classList.contains('mom-cal-grid')) return;
    var cells = cellsOf(t);
    var cur = t.querySelector('.mom-cal-cell.kb');
    var i = cur ? Array.prototype.indexOf.call(cells, cur) : cells.length - 1;
    var n = cells.length;
    var next;
    switch (e.key) {
      case 'ArrowUp': next = Math.max(0, i - 1); break;
      case 'ArrowDown': next = Math.min(n - 1, i + 1); break;
      case 'ArrowLeft': next = Math.max(0, i - 7); break;
      case 'ArrowRight': next = Math.min(n - 1, i + 7); break;
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      case 'Enter': case ' ':
        if (cur) { e.preventDefault(); open(cur); }
        return;
      case 'Escape': hide(calOf(t)); return;
      default: return;
    }
    e.preventDefault();
    cursor(t, cells, next);
  });
})();

/* Per-mark readouts (charts_marks.go): the ring, the day rhythm, ranked bars,
   the segmented bar, the month grid and the presence strip. The contract is
   the trend readout's -- every string was formatted server-side, and goes in
   with textContent -- but each mark is its own hit target rather than the
   point nearest the pointer. A chart root (.chart[data-hover]) carries one
   readout per mark as JSON. Every element of a mark names it with data-i (the
   mark and its larger, transparent hit area); a legend row, which is already
   its mark's readout, names the mark it lights with data-lit.

   - Hover, or a tap, shows the readout and lifts the mark: the mark's elements
     take .chart-on and the root .chart-lit, and the stylesheet quiets the rest.
   - Keyboard: a .chart-walk element (the ring and rhythm svgs, the segmented
     bar, the presence track) is one tab stop; the arrows walk its marks, Home
     and End jump, Escape rests. A mark that is a link (a ranked bar's name, a
     month cell) answers its own focus. The month grid is one tab stop too:
     this file gives its days a roving tabindex, and the arrows move focus a
     day or a week. Without this file every day is a plain Tab stop.
   - The live morph strips what this file adds (the tooltip, the classes, the
     roving tabindex). Each comes back on the next event, and the roving
     tabindex on seam:content-updated. */
(function () {
  var state = new WeakMap(); // chart root -> { raw, data, i }
  var current = null;        // the root showing a readout
  var TONE = /^(series-[1-5]|series-other|mix-[1-4])$/;

  function div(cls, text) {
    var e = document.createElement('div');
    e.className = cls;
    if (text) e.textContent = text;
    return e;
  }

  function rootOf(t) { return t && t.closest ? t.closest('.chart[data-hover]') : null; }

  // stateOf parses the root's payload once per version of it: the morph
  // rewrites data-hover in place when the numbers change.
  function stateOf(root) {
    var raw = root.getAttribute('data-hover') || '';
    var s = state.get(root);
    if (s && s.raw === raw) return s;
    var data = null;
    try { data = JSON.parse(raw); } catch (e) { data = null; }
    if (!data || !data.marks || !data.marks.length) data = null;
    s = { raw: raw, data: data, i: -1 };
    state.set(root, s);
    return s;
  }

  function tipOf(root) {
    var tip = root.querySelector(':scope > .hov-tip');
    if (!tip) {
      tip = div('hov-tip');
      tip.hidden = true;
      tip.setAttribute('aria-hidden', 'true');
      root.appendChild(tip);
    }
    return tip;
  }

  function light(root, i) {
    root.querySelectorAll('.chart-on').forEach(function (el) { el.classList.remove('chart-on'); });
    if (i < 0) { root.classList.remove('chart-lit'); return; }
    root.querySelectorAll('[data-i="' + i + '"], [data-lit="' + i + '"]').forEach(function (el) { el.classList.add('chart-on'); });
    root.classList.add('chart-lit');
  }

  function fill(tip, m) {
    tip.textContent = '';
    tip.appendChild(div('hov-t', m.label));
    tip.appendChild(div('hov-v', m.value));
    if (m.sub) tip.appendChild(div('hov-s', m.sub));
    if (m.rows && m.rows.length) {
      var rows = div('hov-rows');
      m.rows.forEach(function (r) {
        rows.appendChild(div('hov-key' + (TONE.test(r.tone) ? ' ' + r.tone : '')));
        rows.appendChild(div('hov-row-v', r.value));
        rows.appendChild(div('hov-row-n', r.name));
      });
      tip.appendChild(rows);
    }
  }

  // place sets the tooltip above the mark: over its At point on an svg chart
  // (viewBox units, scaled by the svg's rendered box), else over the mark's
  // own box. Centered and clamped inside the chart; below the mark only when
  // above would leave the window.
  function place(root, tip, data, m, el) {
    var rr = root.getBoundingClientRect(), cx, top, bottom;
    var svg = root.querySelector('svg.chart-svg');
    if (m.at && svg && data.w && data.h) {
      var sr = svg.getBoundingClientRect();
      cx = sr.left - rr.left + m.at[0] * (sr.width / data.w);
      top = bottom = sr.top - rr.top + m.at[1] * (sr.height / data.h);
    } else if (el) {
      var er = el.getBoundingClientRect();
      cx = er.left - rr.left + er.width / 2;
      top = er.top - rr.top;
      bottom = er.bottom - rr.top;
    } else {
      return;
    }
    var w = tip.offsetWidth, h = tip.offsetHeight;
    var left = Math.max(0, Math.min(cx - w / 2, root.clientWidth - w));
    var y = top - h - 10;
    if (rr.top + y < 0) y = bottom + 10;
    tip.style.left = Math.round(left) + 'px';
    tip.style.top = Math.round(y) + 'px';
  }

  function hide(root) {
    var tip = root.querySelector(':scope > .hov-tip');
    if (tip) tip.hidden = true;
    light(root, -1);
    var s = state.get(root);
    if (s) s.i = -1;
    if (current === root) current = null;
  }

  function show(root, i, el) {
    var s = stateOf(root);
    var m = s.data && s.data.marks[i];
    if (!m) return;
    if (current && current !== root) hide(current);
    current = root;
    var tip = tipOf(root);
    if (s.i !== i || tip.hidden) {
      fill(tip, m);
      tip.hidden = false; // before placing: the clamp needs the tooltip's real box
      light(root, i);
      s.i = i;
    }
    // From the keyboard there is no element under the pointer: anchor on the
    // mark's first element, or its legend row for a part the bar cannot draw.
    place(root, tip, s.data, m, el || root.querySelector('[data-i="' + i + '"], [data-lit="' + i + '"]'));
  }

  function markOf(t, root) {
    var el = t && t.closest ? t.closest('[data-i], [data-lit]') : null;
    return el && root.contains(el) ? el : null;
  }

  // Delegated from the document: the charts are server-rendered and the live
  // morph can replace them wholesale, so nothing may hold a node reference.
  function point(e) {
    var root = rootOf(e.target);
    if (!root) { if (current) hide(current); return; }
    var el = markOf(e.target, root);
    if (!el) { hide(root); return; }
    if (el.hasAttribute('data-lit')) {
      if (current && current !== root) hide(current);
      current = root;
      var tip = root.querySelector(':scope > .hov-tip');
      if (tip) tip.hidden = true;
      stateOf(root).i = -1;
      light(root, +el.getAttribute('data-lit'));
      return;
    }
    show(root, +el.getAttribute('data-i'), el);
  }
  document.addEventListener('pointerover', point, { passive: true });
  document.addEventListener('pointerdown', point, { passive: true }); // touch: tap to read
  document.addEventListener('pointerout', function (e) {
    var root = rootOf(e.target);
    if (root && !(e.relatedTarget && root.contains(e.relatedTarget))) hide(root);
  }, { passive: true });

  function walker(t) { return !!(t && t.classList && t.classList.contains('chart-walk')); }

  document.addEventListener('focusin', function (e) {
    var root = rootOf(e.target);
    if (!root) { if (current) hide(current); return; }
    var s = stateOf(root);
    if (!s.data) return;
    if (walker(e.target)) { show(root, s.i >= 0 ? s.i : (s.data.start || 0)); return; }
    var el = markOf(e.target, root);
    if (el && el.hasAttribute('data-i')) show(root, +el.getAttribute('data-i'), el);
  });
  document.addEventListener('focusout', function (e) {
    var root = rootOf(e.target);
    if (root && !(e.relatedTarget && root.contains(e.relatedTarget))) hide(root);
  });

  function walk(e, root, s) {
    var n = s.data.marks.length;
    var i = s.i >= 0 ? s.i : (s.data.start || 0);
    var next;
    switch (e.key) {
      case 'ArrowLeft': next = Math.max(0, i - 1); break;
      case 'ArrowRight': next = Math.min(n - 1, i + 1); break;
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      case 'Escape': hide(root); return;
      default: return;
    }
    e.preventDefault();
    show(root, next);
  }

  // The month grid's roving tabindex: one day is the tab stop (the selected
  // day, else today, else the last day that links), and the arrows move it.
  function days(root) { return root.querySelectorAll('a.chart-month-cell'); }
  function rest(root) {
    var links = days(root);
    if (!links.length) return;
    var home = root.querySelector('a.chart-month-cell.sel') || root.querySelector('a.chart-month-cell.today') || links[links.length - 1];
    links.forEach(function (a) { a.setAttribute('tabindex', a === home ? '0' : '-1'); });
  }
  function restAll() { document.querySelectorAll('.chart-month').forEach(rest); }
  restAll();
  document.addEventListener('seam:content-updated', restAll);

  function rove(e, root, s) {
    var links = days(root);
    var i = +e.target.getAttribute('data-i');
    var n = s.data.marks.length;
    var day = function (j) { return root.querySelector('a.chart-month-cell[data-i="' + j + '"]'); };
    var target = null;
    switch (e.key) {
      case 'ArrowLeft':
      case 'ArrowRight':
        // A day that does not link is skipped, to the next one that does.
        var step = e.key === 'ArrowLeft' ? -1 : 1;
        for (var j = i + step; j >= 0 && j < n && !target; j += step) target = day(j);
        break;
      case 'ArrowUp': target = day(i - 7); break;
      case 'ArrowDown': target = day(i + 7); break;
      case 'Home': target = links[0]; break;
      case 'End': target = links[links.length - 1]; break;
      default: return;
    }
    e.preventDefault();
    if (!target) return;
    links.forEach(function (a) { a.setAttribute('tabindex', a === target ? '0' : '-1'); });
    target.focus();
  }

  document.addEventListener('keydown', function (e) {
    var root = rootOf(e.target);
    if (!root) return;
    var s = stateOf(root);
    if (!s.data) return;
    if (walker(e.target)) walk(e, root, s);
    else if (e.target.matches && e.target.matches('.chart-month a.chart-month-cell')) rove(e, root, s);
  });
})();
