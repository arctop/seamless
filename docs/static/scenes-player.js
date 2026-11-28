/* scenes-player.js -- animates the verbatim with/without terminal transcripts
   from scenes.js into the #scenes section as ONE self-running reel. No
   dependencies, no network, no state beyond the DOM.

   The four scenes share a single viewport slot: a scene-selector nav on top,
   one scene card below. On scroll into view the reel runs a tour and keeps
   looping until the visitor scrolls away:

     play `without` -> hold -> play `with` beside it -> hold -> next scene
     -> ... -> wrap back to the first scene

   The split scene (layout:"split") has no without/with; it plays once, holds,
   and advances. Scrolling the reel offscreen pauses the tour; returning resumes
   it where it stopped. Clicking a scene tab jumps the tour there (arrow keys,
   Home and End move between tabs); clicking the inner without|with toggle or
   replay steers the current scene. Once the visitor has steered, the tour
   stops changing scenes: their scene plays through and stays, since a
   transcript takes longer to read than the tour would hold it. A mouse over
   the reel holds it in the same way for as long as it is there. On wide
   screens both sides of a scene play at once, so
   neither terminal sits idle while the other runs. prefers-reduced-motion
   renders transcripts statically with no autoplay and no tour. Text is real,
   selectable DOM.

   Curation lives in the data (scenes.js), never here: this file renders whatever
   steps it is handed, verbatim. Two layouts: `with-without` (two terminals side
   by side, each outcome revealed when its side finishes; one terminal behind a
   without|with toggle on narrow screens) and `split` (two terminals on one
   beat-ordered timeline,
   so the tasks_claim race reads as a race). The files-as-truth epilogue (comment/
   cmd/files/fm steps) is folded into scene 1's with-side ending. */
(function () {
  "use strict";

  var SCENES = window.SEAMLESS_SCENES || [];
  var byId = {};
  SCENES.forEach(function (s) { byId[s.id] = s; });

  var reduced = window.matchMedia &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  /* how long to hold on a finished segment before the tour moves on */
  var HOLD_PANE = 2600;   // after `without`, before flipping to `with`
  var HOLD_SCENE = 3800;  // after the last segment, before the next scene

  var INJECT_LABEL = {
    "seam-briefing": "injected · session start",
    "seam-recall": "injected · prompt recall"
  };

  function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  /* both sides of a scene side by side, or one at a time behind a toggle */
  var wideMq = window.matchMedia ? window.matchMedia("(min-width: 901px)") : null;
  function wide() { return !wideMq || wideMq.matches; }

  /* scrolling the reel away pauses whatever is typing; coming back resumes it */
  async function gate(st) {
    while (st.paused) await wait(120);
  }

  /* arrow keys, Home and End move between the tabs of a tablist */
  function rovingKeys(tabs, activate) {
    tabs.forEach(function (t, i) {
      t.addEventListener("keydown", function (e) {
        var j = e.key === "ArrowRight" || e.key === "ArrowDown" ? (i + 1) % tabs.length
          : e.key === "ArrowLeft" || e.key === "ArrowUp" ? (i - 1 + tabs.length) % tabs.length
          : e.key === "Home" ? 0 : e.key === "End" ? tabs.length - 1 : -1;
        if (j < 0) return;
        e.preventDefault();
        tabs[j].focus();
        activate(j);
      });
    });
  }

  /* ---- markdown-lite for agent prose (trusted, committed data) ------------- */
  function esc(s) {
    return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }
  function inline(s) {
    return esc(s)
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/\*([^*]+)\*/g, "<em>$1</em>");
  }
  function markdown(text) {
    var out = "";
    var list = null;
    function closeList() { if (list) { out += "</" + list + ">"; list = null; } }
    text.split("\n").forEach(function (line) {
      if (/^\s*$/.test(line)) { closeList(); return; }
      var h = line.match(/^(#{1,4})\s+(.*)$/);
      if (h) { closeList(); out += "<h4>" + inline(h[2]) + "</h4>"; return; }
      if (/^>\s?/.test(line)) { closeList(); out += "<blockquote>" + inline(line.replace(/^>\s?/, "")) + "</blockquote>"; return; }
      var ol = line.match(/^\s*\d+\.\s+(.*)$/);
      if (ol) { if (list !== "ol") { closeList(); out += "<ol>"; list = "ol"; } out += "<li>" + inline(ol[1]) + "</li>"; return; }
      var ul = line.match(/^\s*[-*]\s+(.*)$/);
      if (ul) { if (list !== "ul") { closeList(); out += "<ul>"; list = "ul"; } out += "<li>" + inline(ul[1]) + "</li>"; return; }
      closeList();
      out += "<p>" + inline(line) + "</p>";
    });
    closeList();
    return out;
  }

  /* ---- render one transcript step to an element --------------------------- */
  function renderStep(step) {
    var el = document.createElement("div");
    if (step.role === "user") {
      el.className = "ln user";
      el.innerHTML = '<span class="p">&gt;</span> <span class="typed"></span>';
    } else if (step.role === "inject") {
      el.className = "ln inject";
      var body = esc(step.text);
      (step.focus || []).forEach(function (f) {
        var ef = esc(f);
        body = body.split(ef).join('<mark class="foc">' + ef + "</mark>");
      });
      var label = INJECT_LABEL[step.tag] || ("injected · " + step.tag);
      el.innerHTML = '<span class="inject-tag">' + esc(label) + "</span>" +
        '<span class="inject-body">' + body + "</span>";
    } else if (step.role === "agent") {
      el.className = "ln agent";
      el.innerHTML = markdown(step.text);
    } else if (step.role === "tool") {
      el.className = "ln tool" + (step.emphasis ? " tool-" + step.emphasis : "");
      var res = step.result ? ' <span class="tool-res">' + esc(step.result) + "</span>" : "";
      el.innerHTML = '<span class="tool-dot" aria-hidden="true">●</span> ' +
        '<span class="tool-label">' + esc(step.label) + "</span>" + res;
    } else if (step.role === "ffwd") {
      el.className = "ffwd";
      el.innerHTML = '<span class="ffwd-chip">4×</span> <span>' + esc(step.text) + "</span>";
    } else if (step.role === "comment") {
      el.className = "cl-ln cl-comment";
      el.innerHTML = '<span class="p">$</span> <span class="c">' + esc(step.text) + "</span>";
    } else if (step.role === "cmd") {
      el.className = "cl-ln cl-cmd";
      el.innerHTML = '<span class="p">$</span> ' + esc(step.text);
    } else if (step.role === "files") {
      el.className = "cl-ln cl-files";
      var grid = '<div class="cl-filegrid">';
      (step.files || []).forEach(function (f) {
        var isNew = !!f.tag;
        grid += '<span class="cl-file' + (isNew ? " new" : "") + '">' + esc(f.name) +
          (isNew ? ' <span class="cl-tag">· ' + esc(f.tag) + "</span>" : "") + "</span>";
      });
      el.innerHTML = grid + "</div>";
    } else if (step.role === "fm") {
      el.className = "cl-ln cl-fm";
      el.innerHTML = step.k
        ? '<span class="k">' + esc(step.k) + '</span> <span class="s">' + esc(step.v) + "</span>"
        : '<span class="dim">' + esc(step.v) + "</span>";
    } else {
      el.className = "ln";
      el.textContent = step.text || "";
    }
    return el;
  }

  function delayFor(step) {
    if (reduced) return 0;
    switch (step.role) {
      case "inject": return 1500;
      case "agent": return Math.max(600, Math.min(2600, (step.text || "").length * 12));
      case "tool": return 700;
      case "ffwd": return 950;
      case "user": return 450;
      case "comment": return 520;
      case "cmd": return 520;
      case "files": return 700;
      case "fm": return 240;
      default: return 300;
    }
  }

  function autoscroll(body, el, toFocus) {
    if (reduced) return;
    if (toFocus) {
      var foc = el.querySelector(".foc");
      body.scrollTop = (foc ? foc.offsetTop : el.offsetTop) - 40;
    } else {
      body.scrollTop = body.scrollHeight;
    }
  }

  async function typewriter(span, text, token, st) {
    if (reduced) { span.textContent = text; return; }
    span.parentNode.classList.add("typing");
    var per = Math.min(30, 2200 / Math.max(text.length, 1));
    for (var i = 1; i <= text.length; i++) {
      if (st.token !== token) { span.textContent = text; break; }
      await gate(st);
      span.textContent = text.slice(0, i);
      await wait(per);
    }
    span.parentNode.classList.remove("typing");
  }

  /* fill a pane's body with its full transcript at once (reduced motion, or
     re-showing a pane already seen) */
  function staticRender(pane, body) {
    body.innerHTML = "";
    pane.steps.forEach(function (step) {
      var el = renderStep(step);
      if (step.role === "user") el.querySelector(".typed").textContent = step.text;
      el.classList.add("show");
      body.appendChild(el);
    });
    body.scrollTop = 0;
  }

  /* animate a pane's body step by step; calls onDone() when finished, and
     nothing at all if superseded (a newer token started). */
  async function playPane(pane, body, st, onDone) {
    var token = ++st.token;
    body.innerHTML = "";
    for (var i = 0; i < pane.steps.length; i++) {
      if (st.token !== token) return;
      await gate(st);
      if (st.token !== token) return;
      var step = pane.steps[i];
      var el = renderStep(step);
      body.appendChild(el);
      if (step.role === "user") {
        await typewriter(el.querySelector(".typed"), step.text, token, st);
        el.classList.add("show");
      } else {
        void el.offsetWidth; // flush so the reveal transition runs
        el.classList.add("show");
        if (step.role === "inject") el.classList.add("pulse");
      }
      autoscroll(body, el, step.role === "inject");
      if (st.token !== token) return;
      await wait(delayFor(step));
    }
    if (st.token !== token) return;
    if (onDone) onDone();
  }

  var REPLAY_SVG =
    '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" ' +
    'stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
    '<path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v4h4"/></svg>';

  function sceneHead(scene) {
    var head = document.createElement("div");
    head.className = "ts-head";
    head.innerHTML =
      '<p class="ts-kicker">' + esc(scene.kicker) + "</p>" +
      '<h3 class="ts-title">' + esc(scene.title) + "</h3>" +
      '<p class="ts-ask"><span class="ts-ask-label">prompt</span>' +
      "<span>" + esc(scene.prompt) + "</span></p>";
    return head;
  }

  /* ---- controller: with/without scene -------------------------------------
     Both sides sit next to each other on wide screens and play in turn, each
     with its outcome underneath once it finishes; narrow screens show one side
     at a time behind a without|with toggle. Returns { segments, playSegment,
     showStatic, stop } for the reel to drive. `events` carries the reel's
     callbacks for user clicks. */
  function buildWithWithout(scene, events, mount) {
    var order = ["without", "with"].filter(function (k) {
      return scene.panes.some(function (p) { return p.key === k; });
    });
    var byKey = {};
    scene.panes.forEach(function (p) { byKey[p.key] = p; });

    mount.appendChild(sceneHead(scene));

    // without|with toggle (narrow screens; CSS hides it when both sides fit)
    var tabs = document.createElement("div");
    tabs.className = "term-tabs";
    tabs.setAttribute("role", "tablist");
    tabs.setAttribute("aria-label", scene.title + " -- with or without Seamless");
    var tabEls = {};
    order.forEach(function (key) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "term-tab tab-" + key;
      b.setAttribute("role", "tab");
      b.id = scene.id + "-tab-" + key;
      b.setAttribute("aria-controls", scene.id + "-side-" + key);
      b.dataset.pane = key;
      b.innerHTML = '<span class="tab-dot" aria-hidden="true"></span>' + esc(byKey[key].label);
      tabs.appendChild(b);
      tabEls[key] = b;
    });
    mount.appendChild(tabs);

    // one terminal per side, each with its outcome underneath
    var duo = document.createElement("div");
    duo.className = "ts-duo";
    var sides = {}, bodies = {}, outcomes = {}, played = {};
    order.forEach(function (key) {
      var side = document.createElement("div");
      side.className = "ts-side";
      side.id = scene.id + "-side-" + key;
      side.dataset.side = key;
      var label = document.createElement("p");
      label.className = "ts-side-head";
      label.textContent = byKey[key].label;
      side.appendChild(label);
      var term = document.createElement("div");
      term.className = "term term-scene-term";
      var bar = document.createElement("div");
      bar.className = "term-bar";
      bar.innerHTML = "<span>" + esc(byKey[key].label) + " · ~/code/myapp</span>";
      term.appendChild(bar);
      var body = document.createElement("div");
      body.className = "term-body ts-pane";
      body.dataset.pane = key;
      body.setAttribute("aria-label", byKey[key].label + " transcript");
      term.appendChild(body);
      side.appendChild(term);
      var out = document.createElement("p");
      out.className = "ts-outcome";
      out.textContent = byKey[key].outcome;
      side.appendChild(out);
      duo.appendChild(side);
      sides[key] = side;
      bodies[key] = body;
      outcomes[key] = out;
      played[key] = false;
    });
    mount.appendChild(duo);

    // footer: replay
    var foot = document.createElement("div");
    foot.className = "ts-foot";
    foot.innerHTML = '<button class="ts-replay" type="button">' + REPLAY_SVG + "replay</button>";
    mount.appendChild(foot);

    var state = {};
    order.forEach(function (key) { state[key] = { token: 0, paused: false }; });

    /* the sides are tab panels only while the toggle is what shows them */
    function syncRoles() {
      order.forEach(function (k) {
        if (wide()) {
          sides[k].removeAttribute("role");
          sides[k].removeAttribute("aria-labelledby");
        } else {
          sides[k].setAttribute("role", "tabpanel");
          sides[k].setAttribute("aria-labelledby", tabEls[k].id);
        }
      });
    }
    syncRoles();
    if (wideMq && wideMq.addEventListener) wideMq.addEventListener("change", syncRoles);

    function idle(key) {
      bodies[key].innerHTML =
        '<div class="ln idle"><span class="p">&gt;</span> <span class="caret"></span></div>';
    }

    function showPane(key) {
      order.forEach(function (k) {
        var on = k === key;
        sides[k].classList.toggle("is-active", on);
        sides[k].classList.toggle("is-shown", on);
        tabEls[k].setAttribute("aria-selected", on ? "true" : "false");
        tabEls[k].classList.toggle("active", on);
        tabEls[k].tabIndex = on ? 0 : -1;
      });
    }

    function stop() { order.forEach(function (k) { state[k].token++; state[k].paused = false; }); }
    function pause() { order.forEach(function (k) { state[k].paused = true; }); }
    function resume() { order.forEach(function (k) { state[k].paused = false; }); }

    /* wide screens: both sides at once, each outcome when its side ends */
    function playBoth(onDone) {
      stop();
      showPane(order[order.length - 1]);
      var left = order.length;
      order.forEach(function (k) {
        played[k] = false;
        sides[k].classList.add("is-active");
        sides[k].classList.remove("is-waiting");
        outcomes[k].classList.add("is-pending");
        playPane(byKey[k], bodies[k], state[k], function () {
          played[k] = true;
          outcomes[k].classList.remove("is-pending");
          left--;
          if (left === 0 && onDone) onDone();
        });
      });
    }

    function playSegment(name, onDone) {
      if (name === "both") { playBoth(onDone); return; }
      stop();
      showPane(name);
      if (name === order[0]) {
        // a fresh run: every later side goes back to waiting its turn
        order.forEach(function (k) {
          if (k === name) return;
          played[k] = false;
          idle(k);
          outcomes[k].classList.add("is-pending");
        });
      }
      order.forEach(function (k) {
        sides[k].classList.toggle("is-waiting", k !== name && !played[k]);
      });
      outcomes[name].classList.add("is-pending");
      playPane(byKey[name], bodies[name], state[name], function () {
        played[name] = true;
        outcomes[name].classList.remove("is-pending");
        if (onDone) onDone();
      });
    }

    function showStatic(name) {
      name = name || order[0];
      showPane(name);
      order.forEach(function (k) {
        staticRender(byKey[k], bodies[k]);
        played[k] = true;
        outcomes[k].classList.remove("is-pending");
        sides[k].classList.remove("is-waiting");
      });
    }

    // initial: idle carets, outcomes held back until each side has played
    showPane(order[0]);
    if (!reduced) {
      order.forEach(function (k) {
        idle(k);
        outcomes[k].classList.add("is-pending");
        if (k !== order[0]) sides[k].classList.add("is-waiting");
      });
    }

    order.forEach(function (key) {
      tabEls[key].addEventListener("click", function () {
        if (events.onSegment) events.onSegment(key);
      });
    });
    rovingKeys(order.map(function (k) { return tabEls[k]; }), function (j) {
      if (events.onSegment) events.onSegment(order[j]);
    });
    foot.querySelector(".ts-replay").addEventListener("click", function () {
      if (events.onReplay) events.onReplay();
    });

    return {
      get segments() { return wide() ? ["both"] : order; },
      playSegment: playSegment,
      showStatic: showStatic,
      stop: stop,
      pause: pause,
      resume: resume
    };
  }

  /* merge both panes' steps into one beat-ordered sequence, grouped so that
     steps sharing a beat fire together and the claim collision reads as a race:
     B's winning claim on its beat, A's bounce on the next. */
  function buildTimeline(panes) {
    var items = [];
    panes.forEach(function (p, pi) {
      p.steps.forEach(function (step, si) {
        items.push({
          pane: p.key, paneIndex: pi, step: step,
          beat: typeof step.beat === "number" ? step.beat : si
        });
      });
    });
    items.sort(function (a, b) {
      return a.beat !== b.beat ? a.beat - b.beat : a.paneIndex - b.paneIndex;
    });
    var groups = [];
    items.forEach(function (it) {
      var last = groups[groups.length - 1];
      if (last && last.beat === it.beat) last.items.push(it);
      else groups.push({ beat: it.beat, items: [it] });
    });
    return groups;
  }

  async function revealSplitStep(step, body, token, st) {
    var el = renderStep(step);
    body.appendChild(el);
    if (step.role === "user") {
      await typewriter(el.querySelector(".typed"), step.text, token, st);
      el.classList.add("show");
    } else {
      void el.offsetWidth; // flush so the reveal transition runs
      el.classList.add("show");
      if (step.role === "inject") el.classList.add("pulse");
    }
    autoscroll(body, el, step.role === "inject");
  }

  async function playSplit(groups, bodies, st, onDone) {
    var token = ++st.token;
    Object.keys(bodies).forEach(function (k) { bodies[k].innerHTML = ""; });
    for (var gi = 0; gi < groups.length; gi++) {
      if (st.token !== token) return;
      await gate(st);
      if (st.token !== token) return;
      var group = groups[gi];
      await Promise.all(group.items.map(function (it) {
        return revealSplitStep(it.step, bodies[it.pane], token, st);
      }));
      if (st.token !== token) return;
      var gap = 0;
      group.items.forEach(function (it) { gap = Math.max(gap, delayFor(it.step)); });
      await wait(gap);
    }
    if (st.token !== token) return;
    if (onDone) onDone();
  }

  /* ---- controller: split scene (two agents, one shared timeline) ----------- */
  function buildSplit(scene, events, mount) {
    var order = ["A", "B"]; // display: agent A left, agent B right
    var byKey = {};
    scene.panes.forEach(function (p) { byKey[p.key] = p; });

    mount.appendChild(sceneHead(scene));

    var split = document.createElement("div");
    split.className = "ts-split";
    var bodies = {};
    order.forEach(function (key) {
      var p = byKey[key];
      if (!p) return;
      var col = document.createElement("div");
      col.className = "ts-col";
      var term = document.createElement("div");
      term.className = "term term-scene-term ts-split-term";
      var bar = document.createElement("div");
      bar.className = "term-bar";
      bar.innerHTML = "<span>" + esc(p.label) + " · ~/code/myapp</span>";
      term.appendChild(bar);
      var body = document.createElement("div");
      body.className = "term-body ts-splitpane";
      body.dataset.pane = key;
      body.setAttribute("role", "group");
      body.setAttribute("aria-label", esc(p.label) + " transcript");
      term.appendChild(body);
      col.appendChild(term);
      var outcome = document.createElement("p");
      outcome.className = "ts-outcome ts-col-outcome";
      outcome.textContent = p.outcome;
      col.appendChild(outcome);
      split.appendChild(col);
      bodies[key] = body;
    });
    mount.appendChild(split);

    var foot = document.createElement("div");
    foot.className = "ts-foot ts-foot-split";
    foot.innerHTML = '<button class="ts-replay" type="button">' + REPLAY_SVG + "replay</button>";
    mount.appendChild(foot);

    var timeline = buildTimeline(scene.panes);
    var st = { token: 0, paused: false };

    function stop() { st.token++; st.paused = false; }

    function playSegment(name, onDone) {
      stop();
      playSplit(timeline, bodies, st, onDone);
    }

    function showStatic() {
      order.forEach(function (key) { if (byKey[key]) staticRender(byKey[key], bodies[key]); });
    }

    if (!reduced) {
      order.forEach(function (key) {
        if (byKey[key]) {
          bodies[key].innerHTML =
            '<div class="ln idle"><span class="p">&gt;</span> <span class="caret"></span></div>';
        }
      });
    }

    foot.querySelector(".ts-replay").addEventListener("click", function () {
      if (events.onReplay) events.onReplay();
    });

    return {
      segments: ["single"],
      playSegment: playSegment,
      showStatic: showStatic,
      stop: stop,
      pause: function () { st.paused = true; },
      resume: function () { st.paused = false; }
    };
  }

  /* ---- the reel: one slot, a scene nav, a self-running tour ---------------- */
  function buildReel(reel) {
    var mounts = Array.prototype.slice.call(reel.querySelectorAll(".term-scene[data-scene]"));
    var scenes = mounts.map(function (m) { return byId[m.dataset.scene]; });
    // keep only mounts whose scene exists, preserving order
    var pairs = [];
    mounts.forEach(function (m, i) { if (scenes[i]) pairs.push({ mount: m, scene: scenes[i] }); });
    if (!pairs.length) return;

    var nav = reel.querySelector(".scenes-nav");
    if (!nav) {
      nav = document.createElement("div");
      nav.className = "scenes-nav";
      nav.setAttribute("role", "tablist");
      reel.insertBefore(nav, reel.firstChild);
    }

    var controllers = new Array(pairs.length); // lazy

    // build the scene nav
    var navEls = pairs.map(function (pair, i) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "scene-tab";
      b.setAttribute("role", "tab");
      b.id = "scene-tab-" + pair.scene.id;
      b.setAttribute("aria-controls", "scene-" + pair.scene.id);
      pair.mount.id = "scene-" + pair.scene.id;
      pair.mount.setAttribute("role", "tabpanel");
      pair.mount.setAttribute("aria-labelledby", b.id);
      b.dataset.i = i;
      var num = ("0" + (i + 1)).slice(-2);
      b.innerHTML = '<span class="sr-num">' + num + "</span>" +
        '<span class="sr-label">' + esc(pair.scene.tab || pair.scene.kicker) + "</span>" +
        '<span class="sr-prog" aria-hidden="true"></span>';
      nav.appendChild(b);
      return b;
    });

    function controllerFor(i) {
      if (controllers[i]) return controllers[i];
      var scene = pairs[i].scene;
      var events = {
        onSegment: function (name) { userSegment(i, name); },
        onReplay: function () { userReplay(i); }
      };
      var c = scene.layout === "split"
        ? buildSplit(scene, events, pairs[i].mount)
        : buildWithWithout(scene, events, pairs[i].mount);
      controllers[i] = c;
      return c;
    }

    function showScene(i) {
      pairs.forEach(function (pair, j) {
        pair.mount.classList.toggle("on", j === i);
        navEls[j].classList.toggle("active", j === i);
        navEls[j].setAttribute("aria-selected", j === i ? "true" : "false");
        navEls[j].tabIndex = j === i ? 0 : -1;
        if (j !== i) navEls[j].classList.remove("playing");
      });
    }

    /* ---- tour state --------------------------------------------------------- */
    var cur = 0, seg = 0, holdTimer = null, inView = false, started = false, segDone = false;
    var took = false;      // the visitor picked a scene, a side or replay: no more scene changes
    var hovering = false;  // a mouse over the reel holds it where it is

    function setProgress(i, frac, ms) {
      var prog = navEls[i].querySelector(".sr-prog");
      if (!prog) return;
      prog.style.transition = "none";
      prog.style.transform = "scaleX(" + (frac ? 1 : 0) + ")";
      if (ms) {
        void prog.offsetWidth; // flush
        prog.style.transition = "transform " + ms + "ms linear";
        prog.style.transform = "scaleX(1)";
      }
    }
    function clearProgress() { navEls.forEach(function (b, i) { setProgress(i, 0, 0); }); }

    function clearHold() {
      if (holdTimer) { clearTimeout(holdTimer); holdTimer = null; }
      clearProgress();
    }

    function holdMs() {
      var segs = controllerFor(cur).segments;
      return seg < segs.length - 1 ? HOLD_PANE : HOLD_SCENE;
    }

    function playSeg() {
      clearHold();
      segDone = false;
      navEls[cur].classList.add("playing");
      var c = controllerFor(cur);
      var segs = c.segments;
      if (seg > segs.length - 1) seg = segs.length - 1; // the layout changed under us
      c.playSegment(segs[seg], onSegDone);
    }

    function onSegDone() {
      segDone = true;
      if (!inView || hovering) return;
      if (took && seg >= controllerFor(cur).segments.length - 1) return; // their scene stays
      var ms = holdMs();
      setProgress(cur, 0, ms); // telegraph the coming advance
      holdTimer = setTimeout(advance, ms);
    }

    function advance() {
      clearHold();
      var segs = controllerFor(cur).segments;
      if (seg < segs.length - 1) { seg++; playSeg(); return; }
      controllerFor(cur).stop();
      navEls[cur].classList.remove("playing");
      cur = (cur + 1) % pairs.length;
      seg = 0;
      controllerFor(cur); // build before showing to avoid an empty flash
      showScene(cur);
      playSeg();
    }

    /* ---- user steering ------------------------------------------------------ */
    function goScene(i) {
      clearHold();
      if (controllers[cur]) controllers[cur].stop();
      navEls[cur].classList.remove("playing");
      cur = i; seg = 0;
      controllerFor(cur); // build before showing to avoid an empty flash
      showScene(cur);
      playSeg();
    }
    function userSegment(i, name) {
      if (reduced) { pauseSwitch(i); controllerFor(i).showStatic(name); return; }
      took = true;
      clearHold();
      if (controllers[cur] && cur !== i) { controllers[cur].stop(); navEls[cur].classList.remove("playing"); }
      cur = i;
      var c = controllerFor(i);
      var idx = c.segments.indexOf(name);
      seg = idx < 0 ? 0 : idx;
      showScene(i);
      playSeg();
    }
    function userReplay(i) {
      if (reduced) { pauseSwitch(i); controllerFor(i).showStatic(); return; }
      took = true;
      clearHold();
      cur = i;
      showScene(i);
      playSeg();
    }
    function pauseSwitch(i) { cur = i; seg = 0; showScene(i); }

    navEls.forEach(function (b, i) {
      b.addEventListener("click", function () {
        if (reduced) { pauseSwitch(i); controllerFor(i).showStatic(); return; }
        took = true;
        goScene(i);
      });
    });
    rovingKeys(navEls, function (i) {
      if (reduced) { pauseSwitch(i); controllerFor(i).showStatic(); return; }
      took = true;
      goScene(i);
    });

    /* ---- reduced motion: static, no tour ------------------------------------ */
    if (reduced) {
      showScene(0);
      controllerFor(0).showStatic();
      return;
    }

    // build + show the first scene idle
    controllerFor(0);
    showScene(0);

    // a mouse over the reel holds the tour; leaving picks the hold back up
    reel.addEventListener("pointerenter", function (e) {
      if (e.pointerType !== "mouse") return;
      hovering = true;
      clearHold();
    });
    reel.addEventListener("pointerleave", function (e) {
      if (e.pointerType !== "mouse") return;
      hovering = false;
      if (segDone && inView) onSegDone();
    });

    // autoplay on scroll into view; leaving pauses, returning resumes
    if ("IntersectionObserver" in window) {
      var io = new IntersectionObserver(function (entries) {
        entries.forEach(function (e) {
          inView = e.isIntersecting;
          if (e.isIntersecting) {
            if (!started) { started = true; playSeg(); }
            else if (segDone) { onSegDone(); } // pick the hold back up
            else if (controllers[cur]) { controllers[cur].resume(); } // carry on mid-transcript
          } else {
            clearHold();
            if (controllers[cur]) controllers[cur].pause();
          }
        });
      }, { rootMargin: "0px 0px -12% 0px" });
      io.observe(reel);
    } else {
      inView = true; started = true;
      playSeg();
    }
  }

  function init() {
    document.querySelectorAll(".scenes-reel").forEach(buildReel);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
