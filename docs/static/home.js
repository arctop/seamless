/* home.js -- motion and the few interactive pieces of the landing page.
   No dependencies, no network. The page reads complete without it: every
   element here starts in its finished state unless this file arms it first.

   1. the sky        the folder section's canvas: sessions streak in as
                     meteors, write memories as stars or recall old ones;
                     point at a star to read it
   2. reveals        the two headline word-rises, staggered .rv siblings
   3. the header     the progress line, the current section in the nav
   4. the duel       the hero figure: Monday, then one Tuesday prompt down
                     two lanes, without Seamless and with it
   5. the loop       write -> brief -> recall -> curate: a tablist beside
                     the orbit that plays one lap by itself
   6. the board      two agents race for one plan step, once
   7. the keys       a planted write held, a locked project opened; the
                     layers line
   8. the folder     a star opens into a file; the gardener asks first
   9. the console    an annotated screen and a lightbox for the others
   10. the finale    the empty-set mark draws itself
   11. memory        the page remembers how far you read, in your browser only
   12. small things  the dock, copy confirmations, the copy meteor, the
                     phone menu

   site.js (shared with the other hand-written pages) still owns the theme
   toggle, the OS switch, copy buttons, the comparison matrix, the phone
   menu's close-on-pick, and the .rv -> .in reveal. */
(function () {
  "use strict";

  var root = document.documentElement;
  var reduced = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  function $(s, c) { return (c || document).querySelector(s); }
  function $$(s, c) { return Array.prototype.slice.call((c || document).querySelectorAll(s)); }
  function clamp(v, a, b) { return v < a ? a : v > b ? b : v; }
  function wait(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function cssVar(name) { return getComputedStyle(root).getPropertyValue(name).trim(); }
  function isNight() { return cssVar("--sky-mode") === "night"; }
  function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"); }

  /* run cb once, the first time el is substantially on screen */
  function onceVisible(el, cb, threshold) {
    if (!("IntersectionObserver" in window)) { cb(); return; }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (e.isIntersecting) { io.disconnect(); cb(); }
      });
    }, { threshold: threshold || 0.25 });
    io.observe(el);
  }
  /* cb(true|false) whenever el enters or leaves the screen */
  function watchVisible(el, cb, margin) {
    if (!("IntersectionObserver" in window)) { cb(true); return; }
    new IntersectionObserver(function (entries) {
      entries.forEach(function (e) { cb(e.isIntersecting); });
    }, { rootMargin: margin || "0px" }).observe(el);
  }

  /* one listener for theme flips, whichever way they happen */
  var themeHandlers = [];
  function onTheme(fn) { themeHandlers.push(fn); }
  function fireTheme() { themeHandlers.forEach(function (fn) { fn(); }); }
  new MutationObserver(fireTheme).observe(root, { attributes: true, attributeFilter: ["data-theme"] });
  if (window.matchMedia) {
    var mqDark = window.matchMedia("(prefers-color-scheme: dark)");
    if (mqDark.addEventListener) mqDark.addEventListener("change", fireTheme);
  }

  /* the polite live region in the page: copy confirmations, welcome-back */
  var announcer = $("#announce");
  function say(text) {
    if (!announcer) return;
    announcer.textContent = "";
    setTimeout(function () { announcer.textContent = text; }, 60);
  }

  /* ======================================================================
     1. THE SKY
     ====================================================================== */
  var WEDGES = ["myapp", "orbital", "orbital-web", "homelab", "dotfiles", "global"];
  /* [wedge, kind, name, description, ring 0 (fresh) .. 1 (old)]
     The myapp entries are the seeded demo instance's own memories, verbatim
     from internal/demokit/scenes.go; the rest are fictional neighbours. */
  var MEMS = [
    [0, "gotcha", "rate-limit-not-in-memory", "In-memory rate limit on the refresh endpoint resets per instance; use shared storage.", 0.16],
    [0, "constraint", "auth-cookies-samesite-lax", "Auth cookies must stay SameSite=Lax, not Strict: Strict logs out users arriving from an external link. Harden elsewhere (__Host-, TTL).", 0.3],
    [0, "constraint", "refresh-token-single-use", "A refresh token is single-use: rotate it on every /auth/refresh, and if an old one is replayed, revoke the whole family.", 0.42],
    [0, "constraint", "auth-cookies-httponly-secure", "Access and refresh tokens ride in HttpOnly, Secure, SameSite=Lax cookies -- never in localStorage or a JSON body (XSS reads both).", 0.55],
    [0, "gotcha", "edge-cache-gotcha", "CDN strips Vary on 304s; never cache HTML", 0.7],
    [0, "runbook", "deploy-runbook", "Deploy myapp: build the binary, run migrations, wait for /healthz, then flip the load balancer.", 0.36],
    [0, "gotcha", "chroma-boot-race", "Chroma isn't ready when the API boots; retry the first query with backoff instead of crashing the process.", 0.88],
    [0, "gotcha", "postgres-timeouts", "Postgres statement_timeout kills long analytics queries; run them on the read replica with a raised timeout.", 0.78],
    [0, "gotcha", "persist-refresh-tokens", "Persist refresh tokens to the database the safe way: one hard rule about what the token column may store.", 0.24],
    [1, "runbook", "dedupe-backfill-runbook", "Backfill the dedupe table in batches of 5k, with the consumer paused.", 0.2],
    [1, "decision", "dedupe-store-schema", "Dedupe keys live in their own table with a unique index, not on the events row.", 0.34],
    [1, "gotcha", "webhook-replay-storm", "Stripe retries for three days; a replayed webhook must be idempotent on the event id.", 0.5],
    [1, "reference", "stripe-test-clocks", "Use Stripe test clocks to simulate renewals instead of sleeping in tests.", 0.66],
    [1, "stage", "pgx-v5-migration", "Status: in_progress. pgx v5 lands behind a build tag until the pool tests pass.", 0.12],
    [2, "convention", "tokens-ts-consts-pattern", "Design tokens are exported as TS consts, never read back from CSS at runtime.", 0.26],
    [2, "gotcha", "edge-cache-vary-cookie", "The edge only keys on Vary: Cookie if the origin sends it on every response.", 0.44],
    [2, "decision", "design-tokens-refresh", "Colors move to OKLCH; hex survives only in the email templates.", 0.72],
    [3, "runbook", "restore-from-restic", "Restore a volume: restic restore latest --target, then chown to the container's uid.", 0.58],
    [3, "gotcha", "zfs-arc-ram", "The ZFS ARC eats the RAM the VMs need; cap zfs_arc_max at 8 GiB.", 0.4],
    [3, "convention", "caddy-per-service-files", "One Caddyfile snippet per service under sites/, imported by the root config.", 0.82],
    [4, "convention", "tmux-local-conf", "Per-host tmux bits live in ~/.tmux/local.conf, never in the shared config.", 0.48],
    [4, "gotcha", "zsh-equals-expansion", "zsh expands an unquoted word that starts with =; quote ==.", 0.3],
    [5, "constraint", "no-force-push-main", "Never force-push main; open a revert instead.", 0.22],
    [5, "convention", "commit-message-style", "Imperative subject under 72 characters; the body says why.", 0.6],
    [5, "reference", "ollama-embeddings", "nomic-embed-text through Ollama keeps embeddings on this machine.", 0.76]
  ];
  /* what the sessions streaking in will write */
  var INCOMING = [
    [0, "gotcha", "rate-limit-shared-storage", "Rate limiter must use shared storage, not a per-process map — each instance counts only its own traffic"],
    [1, "decision", "webhook-key-rotation", "Rotate the webhook signing key with a 24h overlap; accept both secrets meanwhile."],
    [3, "gotcha", "restic-prune-lock", "restic prune holds an exclusive lock; schedule it away from the nightly backup."],
    [2, "convention", "storybook-per-component", "Every component ships a story; the visual tests run against them."],
    [5, "convention", "secrets-never-in-argv", "Read secrets with read -s, never as command-line arguments."],
    [1, "gotcha", "outbox-retry-jitter", "Outbox retries need jitter or they stampede together after an outage."],
    [4, "runbook", "dotfiles-bootstrap", "New machine: clone dotfiles, run ./install, then restore ~/.config from the vault."],
    [0, "decision", "limiter-per-family-keys", "Rate-limit refresh by IP and by token family, so one stolen family can't burn the IP budget."]
  ];
  var KIND_ORDER = ["gotcha", "constraint", "convention", "reference", "decision", "runbook", "stage"];
  /* the tooltip is always a dark card, so its kind chips use the night palette */
  var NIGHT_KIND = { gotcha: "#ff8f73", constraint: "#9aa8ff", convention: "#ff7fc8", reference: "#5fdcbf", decision: "#c39bff", runbook: "#f3c46b", stage: "#6fc8ff" };
  /* the side-by-side layout needs this much room; below it the chart gets a
     band of its own under the copy, the host's bottom padding (home.css
     matches at 1179px) */
  var SKY_SIDE_MIN = 1180;

  function rng(seed) {
    return function () {
      seed |= 0; seed = seed + 0x6D2B79F5 | 0;
      var t = Math.imul(seed ^ seed >>> 15, 1 | seed);
      t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
      return ((t ^ t >>> 14) >>> 0) / 4294967296;
    };
  }

  /* Sky(host): host carries the .sky canvas and the .sky-tip, and marks the
     column the chart must stay clear of with data-sky-keep. data-sky-side
     says which half the chart takes on wide screens. */
  function Sky(hero) {
    var canvas = $(".sky", hero);
    var tip = $(".sky-tip", hero);
    var copy = $("[data-sky-keep]", hero);
    if (!canvas || !canvas.getContext || !tip || !copy) return null;
    var leftSide = hero.dataset.skySide === "left";
    var ctx = canvas.getContext("2d");
    var rand = rng(20261008);
    var W = 0, H = 0, dpr = 1, cx = 0, cy = 0, R = 0, dim = 1;
    var col = {}, sprites = {}, night = true;
    var dust = [], inner = [], stars = [], meteors = [], ripples = [], flashes = [], labels = [], links = [];
    var rot = 0, last = 0, clock = 0, nextAt = 1400, incomingIdx = 0;
    var running = false, raf = 0, onScreen = false, pendingVisit = null;
    var mx = 0, my = 0, pmx = 0, pmy = 0, hover = null, pinned = null;
    var keep = { l: 0, t: 0, r: 0, b: 0 }; /* the copy's box: the sky stays quiet there */
    var TAU = Math.PI * 2;

    function withAlpha(c, a) {
      /* hex (#rrggbb) or rgb()/rgba() -> rgba() with a new alpha */
      if (c.charAt(0) === "#") {
        var n = parseInt(c.slice(1), 16);
        return "rgba(" + (n >> 16 & 255) + "," + (n >> 8 & 255) + "," + (n & 255) + "," + a + ")";
      }
      var m = c.match(/[\d.]+/g);
      if (!m) return c;
      return "rgba(" + m[0] + "," + m[1] + "," + m[2] + "," + a + ")";
    }
    function readColors() {
      night = isNight();
      col = {
        line: cssVar("--sky-line"), line2: cssVar("--sky-line-2"), label: cssVar("--sky-label"),
        dust: cssVar("--sky-dust"), streak: cssVar("--sky-streak"), link: cssVar("--sky-link"),
        accent: cssVar("--accent"), bg: cssVar("--bg")
      };
      KIND_ORDER.forEach(function (k) { col[k] = cssVar("--k-" + k) || col.accent; });
      sprites = {};
      if (night) {
        KIND_ORDER.concat(["streak"]).forEach(function (k) {
          var c = document.createElement("canvas");
          c.width = c.height = 64;
          var g = c.getContext("2d");
          var grd = g.createRadialGradient(32, 32, 0, 32, 32, 32);
          var base = k === "streak" ? col.streak : col[k];
          grd.addColorStop(0, base);
          grd.addColorStop(0.12, base);
          grd.addColorStop(0.32, withAlpha(base, 0.28));
          grd.addColorStop(1, withAlpha(base, 0));
          g.fillStyle = grd;
          g.fillRect(0, 0, 64, 64);
          sprites[k] = c;
        });
      }
    }

    function layout() {
      var r = hero.getBoundingClientRect();
      W = Math.max(1, Math.round(r.width));
      H = Math.max(1, Math.round(r.height));
      dpr = Math.min(window.devicePixelRatio || 1, 2);
      canvas.width = Math.round(W * dpr);
      canvas.height = Math.round(H * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      var c = copy.getBoundingClientRect();
      keep = { l: c.left - r.left - 12, t: c.top - r.top - 12, r: c.right - r.left + 12, b: c.bottom - r.top + 12 };
      if (W >= SKY_SIDE_MIN) {
        /* the chart takes the half the copy leaves free */
        var free = leftSide ? c.left - r.left : W - (c.right - r.left);
        R = Math.min(H * 0.34, free * 0.4);
        cx = leftSide ? free * 0.5 : W - free * 0.5;
        cy = H * 0.5; dim = 1;
      } else {
        /* the band under the copy (the host's bottom padding) holds the chart */
        var pb = parseFloat(getComputedStyle(hero).paddingBottom) || W * 0.8;
        cx = W / 2; cy = H - pb * 0.54; R = Math.min(W * 0.4, pb * 0.3); dim = 0.95;
      }
      if (!dust.length) seed();
    }
    function inKeep(x, y) { return x > keep.l && x < keep.r && y > keep.t && y < keep.b; }

    function seed() {
      var i;
      for (i = 0; i < 260; i++) {
        dust.push({ x: rand(), y: rand(), s: rand() * rand() * 1.5 + 0.35, a: rand() * 0.55 + 0.15, f: rand() * 1.4 + 0.3, p: rand() * 6.28 });
      }
      for (i = 0; i < 90; i++) {
        inner.push({ w: Math.floor(rand() * 6), t: rand(), r: 0.1 + rand() * 0.9, s: rand() * 0.8 + 0.3, a: rand() * 0.4 + 0.1 });
      }
      MEMS.forEach(function (m, idx) {
        stars.push(makeStar(m, m[4], 0.08 + ((idx * 0.37) % 1) * 0.84));
      });
      stars.forEach(function (s) { s.born = 1; });
      stars.forEach(function (s) {
        var best = null, bd = 0.18;
        stars.forEach(function (o) {
          if (o === s || o.w !== s.w) return;
          var d = Math.abs(o.ring - s.ring) + Math.abs(o.t - s.t) * 0.6;
          if (d < bd) { bd = d; best = o; }
        });
        if (best && !links.some(function (l) { return l[0] === best && l[1] === s; })) links.push([s, best]);
      });
    }
    function makeStar(m, ring, t) {
      return {
        w: m[0], kind: m[1], name: m[2], desc: m[3],
        ring: ring, goal: ring, t: t === undefined ? 0.1 + rand() * 0.8 : t,
        size: 1.6 + rand() * 1.7, tw: rand() * 6.28, pulse: 0, born: 0, fade: 1, x: 0, y: 0
      };
    }

    function wedgeAngle(w, t) { return -Math.PI / 2 + rot + (w + 0.06 + t * 0.88) * (TAU / 6); }
    function polar(w, t, ring) {
      var a = wedgeAngle(w, t);
      var rr = R * (0.13 + ring * 0.85);
      return [cx + pmx + Math.cos(a) * rr, cy + pmy + Math.sin(a) * rr];
    }

    function spawn() {
      var write = rand() < 0.5 || stars.length < 12;
      var target;
      if (write) {
        var m = INCOMING[incomingIdx++ % INCOMING.length];
        target = makeStar(m, 0.04 + rand() * 0.1);
        target.incoming = true;
        stars.push(target);
        var fresh = stars.filter(function (s) { return s.incoming && s.born && !s.mine; });
        if (fresh.length > 5) fresh[0].dying = true;
      } else {
        var pool = stars.filter(function (s) { return s.born && !s.dying && !inKeep(s.x, s.y); });
        if (!pool.length) return;
        target = pool[Math.floor(rand() * pool.length)];
      }
      var ang = rand() * TAU;
      var dist = R * (1.5 + rand() * 0.7);
      var sx = cx + Math.cos(ang) * dist, sy = cy + Math.sin(ang) * dist * 0.8;
      meteors.push({ sx: sx, sy: sy, star: target, write: write, t: 0, dur: 1300 + rand() * 600, bend: (rand() - 0.5) * 0.5 });
    }

    function arrive(m) {
      var s = m.star;
      var p = polar(s.w, s.t, s.ring);
      if (m.write) {
        s.born = 0.001;
        ripples.push({ x: p[0], y: p[1], t: 0, c: col[s.kind] });
        labels.push({ star: s, t: 0, text: m.label || ("+ " + s.name + ".md"), hold: m.label ? 3200 : 2200 });
      } else {
        s.pulse = 1;
        s.goal = Math.max(0.03, s.ring * 0.35);
        ripples.push({ x: p[0], y: p[1], t: 0, c: col.accent, small: true });
        var near = stars.filter(function (o) { return o !== s && o.born && !o.dying && o.w === s.w; })
          .sort(function (a, b) { return Math.abs(a.ring - s.ring) + Math.abs(a.t - s.t) - (Math.abs(b.ring - s.ring) + Math.abs(b.t - s.t)); })
          .slice(0, 2);
        near.forEach(function (o) { flashes.push({ a: s, b: o, t: 0 }); });
      }
    }

    function bez(m, t) {
      var s = m.star;
      var p = polar(s.w, s.t, s.ring);
      var mxp = (m.sx + p[0]) / 2, myp = (m.sy + p[1]) / 2;
      var dx = p[0] - m.sx, dy = p[1] - m.sy;
      var c1 = mxp - dy * m.bend, c2 = myp + dx * m.bend;
      var u = 1 - t;
      return [u * u * m.sx + 2 * u * t * c1 + t * t * p[0], u * u * m.sy + 2 * u * t * c2 + t * t * p[1]];
    }

    var skip = false;
    function frame(now) {
      raf = 0;
      if (W < 600 && running && !reduced) {
        /* phones: half the frame rate; nobody can tell, the battery can */
        skip = !skip;
        if (skip) { raf = requestAnimationFrame(frame); return; }
      }
      var dt = last ? Math.min(64, now - last) : 16;
      last = now;
      clock += dt;
      if (!reduced) {
        rot += dt * 0.000012;
        nextAt -= dt;
        if (nextAt <= 0) { spawn(); nextAt = 2300 + rand() * 1900; }
      }
      /* ease the pointer parallax */
      var tx = reduced ? 0 : (mx - W / 2) * -0.012, ty = reduced ? 0 : (my - H / 2) * -0.012;
      pmx += (tx - pmx) * 0.05; pmy += (ty - pmy) * 0.05;
      draw(dt);
      if (running && !reduced) raf = requestAnimationFrame(frame);
    }

    function draw(dt) {
      ctx.clearRect(0, 0, W, H);
      var i, a, p;
      var ox = cx + pmx, oy = cy + pmy;

      /* a breath of light behind the chart */
      var halo = ctx.createRadialGradient(ox, oy, 0, ox, oy, R * 1.25);
      halo.addColorStop(0, withAlpha(col.accent, night ? 0.06 : 0.035));
      halo.addColorStop(1, withAlpha(col.accent, 0));
      ctx.fillStyle = halo;
      ctx.fillRect(0, 0, W, H);

      /* dust across the whole sky */
      ctx.fillStyle = col.dust;
      for (i = 0; i < dust.length; i++) {
        var d = dust[i];
        var tw = reduced ? 1 : 0.6 + 0.4 * Math.sin(clock * 0.001 * d.f + d.p);
        ctx.globalAlpha = d.a * tw * (night ? 1 : 0.55);
        ctx.beginPath();
        ctx.arc(d.x * W + pmx * 0.4, d.y * H + pmy * 0.4, d.s * (night ? 1 : 0.8), 0, TAU);
        ctx.fill();
      }
      ctx.globalAlpha = 1;

      /* the chart: rings, ticks, wedges */
      ctx.save();
      ctx.globalAlpha = dim;
      ctx.lineWidth = 1;
      ctx.strokeStyle = col.line;
      [0.13, 0.4, 0.66, 0.98].forEach(function (f, k) {
        ctx.beginPath();
        ctx.setLineDash(k === 1 || k === 2 ? [2, 5] : []);
        ctx.arc(ox, oy, R * f, 0, TAU);
        ctx.stroke();
      });
      ctx.setLineDash([]);
      ctx.strokeStyle = col.line2;
      ctx.beginPath();
      ctx.arc(ox, oy, R * 1.04, 0, TAU);
      ctx.stroke();
      for (i = 0; i < 120; i++) {
        a = rot * 0.6 + i * TAU / 120;
        var long = i % 10 === 0;
        var r1 = R * 1.04, r2 = R * (long ? 1.085 : 1.06);
        ctx.strokeStyle = long ? col.line2 : col.line;
        ctx.beginPath();
        ctx.moveTo(ox + Math.cos(a) * r1, oy + Math.sin(a) * r1);
        ctx.lineTo(ox + Math.cos(a) * r2, oy + Math.sin(a) * r2);
        ctx.stroke();
      }
      ctx.strokeStyle = col.line;
      for (i = 0; i < 6; i++) {
        a = -Math.PI / 2 + rot + i * TAU / 6;
        ctx.beginPath();
        ctx.moveTo(ox + Math.cos(a) * R * 0.13, oy + Math.sin(a) * R * 0.13);
        ctx.lineTo(ox + Math.cos(a) * R * 0.98, oy + Math.sin(a) * R * 0.98);
        ctx.stroke();
      }
      /* wedge names around the rim, ring names along the top; any label that
         would print behind the copy is left out */
      ctx.fillStyle = col.label;
      ctx.font = "500 " + (W < 600 ? 9 : 10.5) + "px 'IBM Plex Mono', ui-monospace, monospace";
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      for (i = 0; i < 6; i++) {
        a = -Math.PI / 2 + rot + (i + 0.5) * TAU / 6;
        var lr = R * 1.15;
        var lx = ox + Math.cos(a) * lr, ly = oy + Math.sin(a) * lr;
        if (inKeep(lx, ly)) continue;
        ctx.save();
        ctx.translate(lx, ly);
        var up = Math.sin(a) > 0 ? a - Math.PI / 2 : a + Math.PI / 2;
        ctx.rotate(up);
        ctx.fillText(WEDGES[i].toUpperCase().split("").join(String.fromCharCode(8202)), 0, 0);
        ctx.restore();
      }
      ctx.textAlign = "left";
      ["last 24h", "7 days", "45 days"].forEach(function (t, k) {
        var rr = R * [0.13, 0.4, 0.66][k];
        if (inKeep(ox + 6, oy - rr - 7) || inKeep(ox + 60, oy - rr - 7)) return;
        ctx.fillText(t, ox + 6, oy - rr - 7);
      });
      ctx.restore();

      /* faint inner dust */
      ctx.fillStyle = col.dust;
      for (i = 0; i < inner.length; i++) {
        var q = inner[i];
        p = polar(q.w, q.t, q.r);
        ctx.globalAlpha = q.a * dim * (night ? 0.7 : 0.45) * (inKeep(p[0], p[1]) ? 0.3 : 1);
        ctx.beginPath();
        ctx.arc(p[0], p[1], q.s, 0, TAU);
        ctx.fill();
      }
      ctx.globalAlpha = 1;

      /* stars: drift outward as they age, jump inward when recalled */
      for (i = stars.length - 1; i >= 0; i--) {
        var s = stars[i];
        if (!reduced) {
          if (s.goal < s.ring - 0.001) s.ring += (s.goal - s.ring) * 0.04;
          else { s.ring = Math.min(0.98, s.ring + dt * 0.0000045); s.goal = s.ring; }
          if (s.born && s.born < 1) s.born = Math.min(1, s.born + dt / 900);
          if (s.pulse > 0) s.pulse = Math.max(0, s.pulse - dt / 1600);
          if (s.dying) { s.fade -= dt / 1400; if (s.fade <= 0) { stars.splice(i, 1); continue; } }
        }
        p = polar(s.w, s.t, s.ring);
        s.x = p[0]; s.y = p[1];
      }
      /* standing constellations: related memories, faintly joined */
      ctx.lineWidth = 1;
      ctx.strokeStyle = col.link;
      for (i = 0; i < links.length; i++) {
        var la0 = links[i][0], lb0 = links[i][1];
        if (!la0.born || !lb0.born || la0.dying || lb0.dying) continue;
        ctx.globalAlpha = (night ? 0.32 : 0.4) * dim * (inKeep(la0.x, la0.y) || inKeep(lb0.x, lb0.y) ? 0.3 : 1);
        ctx.beginPath();
        ctx.moveTo(la0.x, la0.y);
        ctx.lineTo(lb0.x, lb0.y);
        ctx.stroke();
      }
      ctx.globalAlpha = 1;
      /* constellation flashes: a recall lights the neighbours it pulled in */
      for (i = flashes.length - 1; i >= 0; i--) {
        var f = flashes[i];
        f.t += dt;
        var fa = 1 - f.t / 2600;
        if (fa <= 0 || !f.b.born) { flashes.splice(i, 1); continue; }
        ctx.strokeStyle = withAlpha(col.accent, 0.55 * fa * dim);
        ctx.beginPath();
        ctx.moveTo(f.a.x, f.a.y);
        ctx.lineTo(f.b.x, f.b.y);
        ctx.stroke();
      }
      for (i = 0; i < stars.length; i++) {
        var st = stars[i];
        if (!st.born) continue;
        var grow = st.born < 1 ? easeOutBack(st.born) : 1;
        var twk = reduced ? 1 : 0.82 + 0.18 * Math.sin(clock * 0.0016 + st.tw);
        var focus = st === hover || st === pinned;
        var sz = st.size * grow * (1 + st.pulse * 0.9) * (focus ? 1.5 : 1) * (st.mine ? 1.35 : 1);
        var alpha = clamp(st.fade, 0, 1) * (W < 600 ? 0.85 : 1) * (inKeep(st.x, st.y) && !focus ? 0.3 : 1);
        if (night) {
          var spr = sprites[st.kind] || sprites.gotcha;
          var gs = sz * 12 * twk;
          ctx.globalAlpha = alpha * (0.75 + st.pulse * 0.25);
          ctx.drawImage(spr, st.x - gs / 2, st.y - gs / 2, gs, gs);
          ctx.globalAlpha = alpha;
          ctx.fillStyle = "#fffaf0";
          ctx.beginPath();
          ctx.arc(st.x, st.y, Math.max(0.7, sz * 0.5), 0, TAU);
          ctx.fill();
        } else {
          ctx.globalAlpha = alpha;
          ctx.fillStyle = col[st.kind];
          ctx.beginPath();
          ctx.arc(st.x, st.y, sz * 1.05, 0, TAU);
          ctx.fill();
          ctx.strokeStyle = withAlpha(col[st.kind], 0.35);
          ctx.beginPath();
          ctx.arc(st.x, st.y, sz * 1.05 + 3, 0, TAU);
          ctx.stroke();
        }
        if (focus || st.mine) {
          ctx.globalAlpha = focus ? 1 : 0.55;
          ctx.strokeStyle = col.accent;
          ctx.lineWidth = 1.2;
          ctx.beginPath();
          ctx.arc(st.x, st.y, focus ? 11 : 9, 0, TAU);
          ctx.stroke();
          ctx.lineWidth = 1;
        }
      }
      ctx.globalAlpha = 1;
      /* the open tooltip follows its star as the sky turns */
      var shown = hover || pinned;
      if (shown && !tip.hidden) placeTip(shown);

      /* ripples where a memory was written or recalled */
      for (i = ripples.length - 1; i >= 0; i--) {
        var rp = ripples[i];
        rp.t += dt;
        var k2 = rp.t / 1500;
        if (k2 >= 1) { ripples.splice(i, 1); continue; }
        if (inKeep(rp.x, rp.y)) continue;
        ctx.strokeStyle = withAlpha(rp.c, (1 - k2) * 0.8);
        ctx.lineWidth = 1.2;
        ctx.beginPath();
        ctx.arc(rp.x, rp.y, (rp.small ? 6 : 8) + easeOut(k2) * (rp.small ? 18 : 34), 0, TAU);
        ctx.stroke();
      }
      ctx.lineWidth = 1;

      /* meteors: each session streaks in and is gone */
      for (i = meteors.length - 1; i >= 0; i--) {
        var mt = meteors[i];
        mt.t += dt / mt.dur;
        if (mt.t >= 1) {
          if (!mt.done) { mt.done = true; arrive(mt); }
          mt.fade = (mt.fade || 1) - dt / 380;
          if (mt.fade <= 0) { meteors.splice(i, 1); continue; }
        }
        var head = Math.min(1, easeOut(Math.min(1, mt.t)));
        var tail = Math.max(0, head - 0.3);
        var seg = 16, prev = bez(mt, tail);
        for (var j = 1; j <= seg; j++) {
          var u = tail + (head - tail) * (j / seg);
          var pt = bez(mt, u);
          var w = j / seg;
          ctx.strokeStyle = withAlpha(col.streak, w * w * (night ? 0.9 : 0.7) * (mt.fade || 1));
          ctx.lineWidth = 0.4 + w * (night ? 1.6 : 1.2);
          ctx.beginPath();
          ctx.moveTo(prev[0], prev[1]);
          ctx.lineTo(pt[0], pt[1]);
          ctx.stroke();
          prev = pt;
        }
        if (night && sprites.streak) {
          ctx.globalAlpha = mt.fade || 1;
          ctx.drawImage(sprites.streak, prev[0] - 9, prev[1] - 9, 18, 18);
          ctx.globalAlpha = 1;
        }
      }
      ctx.lineWidth = 1;

      /* "+ name.md" where a session just left something behind */
      ctx.font = "500 11px 'IBM Plex Mono', ui-monospace, monospace";
      ctx.textAlign = "left";
      ctx.textBaseline = "middle";
      for (i = labels.length - 1; i >= 0; i--) {
        var lb = labels[i];
        lb.t += dt;
        var hold = lb.hold || 2200;
        var la = lb.t < 300 ? lb.t / 300 : 1 - (lb.t - hold) / 900;
        if (lb.t > hold + 900 || !lb.star.born) { labels.splice(i, 1); continue; }
        var lw = ctx.measureText(lb.text).width;
        var lx2 = lb.star.x + 12 + lw > W - 8 ? lb.star.x - 12 - lw : lb.star.x + 12;
        lx2 = Math.max(8, lx2);
        var ly2 = lb.star.y - 12 - lb.t * 0.004;
        if (inKeep(lx2, ly2) || inKeep(lx2 + lw, ly2)) continue;
        ctx.globalAlpha = clamp(la, 0, 1) * dim;
        ctx.fillStyle = col.accent;
        ctx.fillText(lb.text, lx2, ly2);
      }
      ctx.globalAlpha = 1;
    }
    function easeOut(t) { return 1 - Math.pow(1 - t, 3); }
    function easeOutBack(t) { var c = 1.7; return 1 + (c + 1) * Math.pow(t - 1, 3) + c * Math.pow(t - 1, 2); }

    function start() {
      if (running) return;
      running = true;
      last = 0;
      if (!raf) raf = requestAnimationFrame(frame);
    }
    function stop() {
      running = false;
      if (raf) { cancelAnimationFrame(raf); raf = 0; }
    }
    function redrawOnce() { if (!running) { last = 0; raf = requestAnimationFrame(frame); } }

    /* pointing at a star reads its file */
    function pick(x, y, radius) {
      var best = null, bd = radius * radius;
      stars.forEach(function (s) {
        if (!s.born || s.dying) return;
        var dx = s.x - x, dy = s.y - y, dd = dx * dx + dy * dy;
        if (dd < bd) { bd = dd; best = s; }
      });
      return best;
    }
    function placeTip(s) {
      /* beside the star, flipped and clamped so it never leaves the hero */
      var w = tip.offsetWidth, h = tip.offsetHeight;
      var x = s.x + 16;
      if (x + w > W - 10) x = s.x - 16 - w;
      x = clamp(x, 10, Math.max(10, W - w - 10));
      var y = clamp(s.y - h / 2, 10, Math.max(10, H - h - 10));
      tip.style.left = x.toFixed(1) + "px";
      tip.style.top = y.toFixed(1) + "px";
    }
    function showTip(s) {
      if (!s) { tip.classList.remove("is-on"); tip.hidden = true; return; }
      tip.hidden = false;
      tip.innerHTML = "";
      var path = document.createElement("p");
      path.className = "tp-path";
      path.textContent = "memory/" + WEDGES[s.w] + "/" + s.name + ".md";
      var kind = document.createElement("span");
      kind.className = "tp-kind";
      kind.textContent = s.kind;
      kind.style.background = NIGHT_KIND[s.kind] || "#f3c46b";
      var desc = document.createElement("p");
      desc.className = "tp-desc";
      desc.textContent = s.desc;
      tip.appendChild(path); tip.appendChild(kind); tip.appendChild(desc);
      placeTip(s);
      tip.classList.add("is-on");
    }

    function overCopy(clientX, clientY) {
      var r = copy.getBoundingClientRect();
      return clientX >= r.left && clientX <= r.right && clientY >= r.top && clientY <= r.bottom;
    }
    hero.addEventListener("pointermove", function (e) {
      var r = hero.getBoundingClientRect();
      mx = e.clientX - r.left; my = e.clientY - r.top;
      if (e.pointerType !== "mouse") return;
      var s = overCopy(e.clientX, e.clientY) ? null : pick(mx, my, 18);
      if (s !== hover) { hover = s; showTip(s || pinned); hero.style.cursor = s ? "pointer" : ""; redrawOnce(); }
    }, { passive: true });
    hero.addEventListener("pointerleave", function () {
      hover = null; mx = W / 2; my = H / 2; showTip(pinned); hero.style.cursor = "";
    });
    hero.addEventListener("click", function (e) {
      if (e.target.closest("a, button, [data-sky-keep]")) return;
      var r = hero.getBoundingClientRect();
      var s = pick(e.clientX - r.left, e.clientY - r.top, 26);
      pinned = s && s !== pinned ? s : null;
      showTip(pinned || hover);
      redrawOnce();
    });

    readColors();
    layout();
    mx = W / 2; my = H / 2;
    onTheme(function () { readColors(); redrawOnce(); });
    var rz = 0;
    function relayout() {
      clearTimeout(rz);
      rz = setTimeout(function () { layout(); redrawOnce(); }, 120);
    }
    window.addEventListener("resize", relayout);
    /* the section grows as its own fonts and figures settle */
    if (window.ResizeObserver) new ResizeObserver(relayout).observe(hero);
    /* fonts settling can move the copy box */
    if (document.fonts && document.fonts.ready) document.fonts.ready.then(function () { layout(); redrawOnce(); });

    var api = {
      /* the visitor copied the install command: their own session streaks in
         and leaves a star behind. From the button if the sky is on screen;
         otherwise it arrives the next time the sky is. */
      visit: function (clientX, clientY) {
        if (api.visited || reduced) return;
        api.visited = true;
        var star = makeStar([5, "reference", "you-were-here", "You copied the install command. This star is yours; the next ones will be your agents'."], 0.03, 0.5);
        star.incoming = true;
        star.mine = true;
        function send(x, y) {
          stars.push(star);
          meteors.push({ sx: x, sy: y, star: star, write: true, t: 0, dur: 1700, bend: 0.28, label: "+ you-were-here.md" });
          start();
        }
        if (onScreen) {
          var r = hero.getBoundingClientRect();
          send(clientX - r.left, clientY - r.top);
        } else {
          pendingVisit = function () { send(W * 0.5, -30); };
        }
      }
    };

    if (reduced) {
      /* one still frame: the sky without the weather */
      requestAnimationFrame(frame);
      return api;
    }
    watchVisible(hero, function (v) {
      onScreen = v;
      if (v && !document.hidden) start(); else stop();
      if (v && pendingVisit) { var go = pendingVisit; pendingVisit = null; setTimeout(go, 700); }
    });
    document.addEventListener("visibilitychange", function () {
      if (document.hidden) stop(); else if (onScreen) start();
    });
    /* the first session arrives soon after the sky does */
    nextAt = 900;
    return api;
  }

  /* ======================================================================
     2. REVEALS
     ====================================================================== */
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
        } else if (n.nodeType === 1 && !n.classList.contains("br")) {
          walk(n);
        }
      });
    })(el);
  }
  function reveals() {
    /* stagger siblings that reveal together */
    var groups = new Map();
    $$(".rv").forEach(function (el) {
      var p = el.parentNode;
      var n = groups.get(p) || 0;
      el.style.setProperty("--d", Math.min(n, 6) * 90 + "ms");
      groups.set(p, n + 1);
    });
    if (reduced) return;
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

  /* ======================================================================
     3. THE HEADER, THE PROGRESS LINE, THE CURRENT SECTION
     ====================================================================== */
  var head = $("[data-head]");
  var progress = head && $(".head-progress i", head);
  var chapters = $$("[data-chapter]");
  var horizons = $$(".chapter");
  var navLinks = $$(".head-nav a[href^='#']");
  var starfield = $(".starfield");
  var furthest = -1;
  var scrollHooks = [];

  function onScroll() {
    var y = window.scrollY || window.pageYOffset;
    var vh = window.innerHeight;
    var docH = root.scrollHeight;
    if (head) head.classList.toggle("is-solid", y > 24);
    if (progress) progress.style.setProperty("--p", clamp(y / Math.max(1, docH - vh), 0, 1).toFixed(4));

    var line = vh * 0.42, cur = -1;
    for (var i = 0; i < chapters.length; i++) {
      if (chapters[i].getBoundingClientRect().top <= line) cur = i;
    }
    if (cur > furthest) { furthest = cur; memory.reached(chapters[cur].id); }
    for (var c = 0; c < horizons.length; c++) horizons[c].classList.toggle("is-reached", horizons[c].getBoundingClientRect().top <= line);
    var curId = cur >= 0 ? chapters[cur].id : "";
    navLinks.forEach(function (a) {
      var on = a.getAttribute("href") === "#" + curId;
      a.classList.toggle("is-current", on);
      if (on) a.setAttribute("aria-current", "true"); else a.removeAttribute("aria-current");
    });
    if (starfield && !reduced) starfield.style.transform = "translate3d(0," + (-y * 0.02).toFixed(1) + "px,0)";
    scrollHooks.forEach(function (fn) { fn(y, vh); });
  }
  var ticking = false;
  function requestScroll() {
    if (ticking) return;
    ticking = true;
    requestAnimationFrame(function () { ticking = false; onScroll(); });
  }

  /* arrow keys, Home and End move between the tabs of a tablist */
  function rove(tabs, pick) {
    tabs.forEach(function (t, i) {
      t.addEventListener("keydown", function (ev) {
        var k = ev.key;
        var j = k === "ArrowDown" || k === "ArrowRight" ? i + 1
          : k === "ArrowUp" || k === "ArrowLeft" ? i - 1
          : k === "Home" ? 0 : k === "End" ? tabs.length - 1 : null;
        if (j === null) return;
        ev.preventDefault();
        pick((j + tabs.length) % tabs.length, true);
      });
    });
  }

  /* ======================================================================
     4. THE DUEL -- Monday's session, then one Tuesday prompt down two lanes
     Each [data-beat] arrives in turn, the same beat on both lanes at once.
     Every line holds its place from the start, so nothing shifts as it plays.
     ====================================================================== */
  function Duel(fig) {
    if (reduced) return;
    var beats = $$("[data-beat]", fig);
    var replay = $(".duel-replay", fig);
    var brief = $(".dl-brief", fig);
    /* the mark around the figure: its star lands when the file is saved
       (beat 2) and answers when the briefing reads it (beat 4) */
    var mark = $(".mk", fig);
    var last = 0;
    beats.forEach(function (el) { last = Math.max(last, +el.dataset.beat); });
    /* how long each beat holds before the next one arrives */
    var HOLD = [700, 950, 1050, 700, 1000, 800, 850, 800, 0];
    fig.classList.add("is-armed");
    var token = 0;
    async function run() {
      var my = ++token;
      if (replay) replay.hidden = true;
      beats.forEach(function (el) { el.classList.remove("is-on", "is-landed"); });
      if (mark) mark.classList.remove("is-lit", "is-read");
      await wait(300); if (my !== token) return;
      for (var b = 0; b <= last; b++) {
        for (var k = 0; k < beats.length; k++) if (+beats[k].dataset.beat === b) beats[k].classList.add("is-on");
        if (b === 2 && mark) mark.classList.add("is-lit");
        if (b === 4 && brief) brief.classList.add("is-landed");
        if (b === 4 && mark) mark.classList.add("is-read");
        await wait(HOLD[b] === undefined ? 750 : HOLD[b]); if (my !== token) return;
      }
      if (replay) replay.hidden = false;
    }
    if (replay) replay.addEventListener("click", run);
    /* the headline rises first */
    onceVisible(fig, function () { setTimeout(run, 450); }, 0.3);
  }

  /* ======================================================================
     5. THE LOOP -- write, brief, recall, curate on one stage
     A tablist beside the orbit. Once on screen it plays one lap by itself,
     waits while the pointer or the keyboard is on it, and stops for good the
     moment the visitor picks a step. The comet only ever travels forward.
     ====================================================================== */
  function Loop(box) {
    var tabs = $$('[role="tab"]', box);
    var panels = tabs.map(function (t) { return document.getElementById(t.getAttribute("aria-controls")); });
    var nodes = $$(".o-node", box);
    var labels = $$(".o-label", box);
    var arm = $(".o-arm", box);
    var trail = $(".o-trail", box);
    var ticks = $(".o-ticks", box);
    var n = tabs.length, cur = 0, turns = 0;
    var DWELL = 6500;
    if (!n) return;
    if (ticks) {
      var NS = "http://www.w3.org/2000/svg";
      for (var i = 0; i < 72; i++) {
        var a = i * Math.PI / 36, long = i % 6 === 0;
        var ln = document.createElementNS(NS, "line");
        ln.setAttribute("x1", (220 + Math.cos(a) * 196).toFixed(2));
        ln.setAttribute("y1", (220 + Math.sin(a) * 196).toFixed(2));
        ln.setAttribute("x2", (220 + Math.cos(a) * (long ? 184 : 190)).toFixed(2));
        ln.setAttribute("y2", (220 + Math.sin(a) * (long ? 184 : 190)).toFixed(2));
        if (long) ln.setAttribute("class", "long");
        ticks.appendChild(ln);
      }
    }
    var trailT = 0;
    function show(i, focus, lap) {
      /* closing the loop: the comet goes on round to the first station */
      if (lap && i === 0) turns++;
      cur = i;
      tabs.forEach(function (t, k) {
        var on = k === i;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.classList.toggle("active", on);
        t.tabIndex = on ? 0 : -1;
        if (panels[k]) panels[k].classList.toggle("is-on", on);
      });
      nodes.forEach(function (nd, k) { nd.classList.toggle("is-on", k === i); nd.classList.toggle("is-past", k < i); });
      labels.forEach(function (lb, k) { lb.classList.toggle("is-on", k === i); });
      if (arm) arm.style.setProperty("--turn", ((turns * n + i) * (360 / n)) + "deg");
      if (trail) {
        clearTimeout(trailT);
        if (lap && i === 0) {
          trail.style.strokeDasharray = "100 100";
          trailT = setTimeout(function () {
            trail.style.transition = "none";
            trail.style.strokeDasharray = "0 100";
            void trail.getBoundingClientRect();
            trail.style.transition = "";
          }, 1200);
        } else {
          trail.style.strokeDasharray = (i / n * 100).toFixed(2) + " 100";
        }
      }
      if (focus) tabs[i].focus();
    }

    /* narrow screens show one step at its own height (home.css, 960px), so a
       step changing by itself would move the page under the reader */
    var narrow = window.matchMedia && window.matchMedia("(max-width: 960px)").matches;
    var auto = false, done = reduced || narrow, onScreen = false, held = false, timer = 0;
    function sync() {
      var run = !done && onScreen && !held;
      if (run === auto) return;
      auto = run;
      box.classList.toggle("is-auto", auto);
      clearTimeout(timer);
      if (auto) next();
    }
    function next() {
      timer = setTimeout(function () {
        if (!auto) return;
        var to = (cur + 1) % n;
        show(to, false, true);
        if (to === 0) { done = true; sync(); return; }
        next();
      }, DWELL);
    }
    function picked(i, focus) { done = true; sync(); show(i, focus, false); }

    box.style.setProperty("--dwell", DWELL + "ms");
    tabs.forEach(function (t, i) { t.addEventListener("click", function () { picked(i, false); }); });
    rove(tabs, picked);
    nodes.forEach(function (nd, i) { nd.addEventListener("click", function () { picked(i, false); }); });
    box.addEventListener("pointerenter", function (e) { if (e.pointerType === "mouse") { held = true; sync(); } });
    box.addEventListener("pointerleave", function () { held = false; sync(); });
    box.addEventListener("focusin", function () { held = true; sync(); });
    box.addEventListener("focusout", function () { held = false; sync(); });
    show(0, false, false);
    if (!("IntersectionObserver" in window)) return;
    new IntersectionObserver(function (entries) {
      entries.forEach(function (e) { onScreen = e.isIntersecting; sync(); });
    }, { threshold: 0.45 }).observe(box);
  }

  /* ======================================================================
     6. THE BOARD -- the race plays once; replay is a click away
     ====================================================================== */
  function Board(fig) {
    if (reduced) return;
    var s5 = $('[data-bstep="5"]', fig), s6 = $('[data-bstep="6"]', fig);
    var logs = $$(".board-log p", fig);
    var count = $("[data-board-count]", fig);
    var agA = $('[data-agent="a"]', fig), agB = $('[data-agent="b"]', fig);
    var replay = $(".board-replay", fig);
    if (!s5 || !s6) return;
    fig.classList.add("is-armed");
    var LOCK = '<svg aria-hidden="true"><use href="#i-lock"/></svg>';
    var CHECK = '<svg aria-hidden="true"><use href="#i-check"/></svg>';
    function status(step, cls, html) {
      step.classList.remove("is-ready", "is-held-a", "is-held-b", "is-done", "is-bounce");
      step.classList.add(cls);
      $("[data-bs]", step).innerHTML = html;
    }
    function reset() {
      status(s5, "is-ready", "ready");
      status(s6, "is-ready", "ready");
      logs.forEach(function (l) { l.classList.remove("is-on"); });
      count.textContent = "4/6 done";
      count.classList.remove("is-done");
      agA.classList.remove("is-busy");
      agB.classList.remove("is-busy");
    }
    var token = 0;
    async function run() {
      var my = ++token;
      if (replay) replay.hidden = true;
      reset();
      await wait(900); if (my !== token) return;
      status(s5, "is-held-b", LOCK + "B · lease 15m");
      agB.classList.add("is-busy");
      logs[0].classList.add("is-on");
      await wait(1400); if (my !== token) return;
      void s5.offsetWidth;
      s5.classList.add("is-bounce");
      logs[1].classList.add("is-on");
      await wait(1500); if (my !== token) return;
      status(s6, "is-held-a", LOCK + "A · lease 15m");
      agA.classList.add("is-busy");
      logs[2].classList.add("is-on");
      await wait(2600); if (my !== token) return;
      status(s5, "is-done", CHECK + "done");
      agB.classList.remove("is-busy");
      count.textContent = "5/6 done";
      await wait(1500); if (my !== token) return;
      status(s6, "is-done", CHECK + "done");
      agA.classList.remove("is-busy");
      count.textContent = "6/6 done";
      count.classList.add("is-done");
      if (replay) replay.hidden = false;
    }
    if (replay) replay.addEventListener("click", run);
    onceVisible(fig, run, 0.4);
  }

  /* ======================================================================
     7. THE KEYS: a planted write held, a locked project opened by a touch.
     Both terminals play line by line, side by side; the approval card waits
     for its scan before the agent goes on. Without JS (or with reduced
     motion) every line simply shows.
     ====================================================================== */
  function Keys(box) {
    if (reduced) return;
    var terms = $$(".vig .term-body", box);
    if (terms.length !== 2) return;
    var replay = $("[data-keys-replay]");
    var a = Array.prototype.slice.call(terms[0].children);
    var b = Array.prototype.slice.call(terms[1].children);
    var card = $("[data-approve]", box);
    box.classList.add("is-armed");
    function reset() {
      a.concat(b).forEach(function (el) { el.classList.remove("is-on"); });
      box.classList.remove("is-scanning", "is-approved");
    }
    var token = 0;
    async function run() {
      var my = ++token;
      if (replay) replay.hidden = true;
      reset();
      await wait(500); if (my !== token) return;
      var ia = 0, ib = 0;
      // the two sessions interleave, the left a beat ahead
      while (ia < a.length || ib < b.length) {
        if (ia < a.length) {
          a[ia].classList.add("is-on");
          ia++;
          await wait(a[ia - 1].classList.contains("t-held") ? 1000 : 450); if (my !== token) return;
        }
        if (ib < b.length) {
          b[ib].classList.add("is-on");
          if (b[ib] === card) {
            box.classList.add("is-scanning");
            await wait(1800); if (my !== token) return;
            box.classList.remove("is-scanning");
            box.classList.add("is-approved");
          }
          ib++;
          await wait(450); if (my !== token) return;
        }
      }
      if (replay) replay.hidden = false;
    }
    if (replay) replay.addEventListener("click", run);
    onceVisible(box, run, 0.35);
  }

  /* ======================================================================
     THE LAYERS: six stops on one line, one sentence on show. A tablist; a
     pointer resting on a stop picks it too.
     ====================================================================== */
  function Layers(box) {
    var tabs = $$('[role="tab"]', box);
    function pick(i, focus) {
      tabs.forEach(function (t, k) {
        var on = k === i;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.classList.toggle("active", on);
        t.tabIndex = on ? 0 : -1;
        var p = document.getElementById(t.getAttribute("aria-controls"));
        if (p) p.classList.toggle("is-on", on);
      });
      if (focus) tabs[i].focus();
    }
    tabs.forEach(function (t, i) {
      t.addEventListener("click", function () { pick(i, false); });
      t.addEventListener("pointerenter", function (e) { if (e.pointerType === "mouse") pick(i, false); });
    });
    rove(tabs, pick);
  }

  /* ======================================================================
     8. THE FOLDER + THE GARDENER
     ====================================================================== */
  function Folder(cell) {
    var file = $(".file", cell) || cell;
    var keys = $$(".file-notes li", file).map(function (li) { return li.dataset.for; });
    var tour = null, k = 0;
    function hot(key) {
      $$(".is-hot", file).forEach(function (el) { el.classList.remove("is-hot"); });
      if (!key) return;
      $$('[data-note="' + key + '"], .file-notes li[data-for="' + key + '"]', file).forEach(function (el) { el.classList.add("is-hot"); });
    }
    function stopTour() { if (tour) { clearInterval(tour); tour = null; } }
    $$(".file-notes li, .fl[data-note]", file).forEach(function (el) {
      var key = el.dataset.for || el.dataset.note;
      el.addEventListener("mouseenter", function () { stopTour(); hot(key); });
      el.addEventListener("mouseleave", function () { hot(null); });
    });
    if (reduced) return;
    cell.classList.add("is-armed");
    onceVisible(cell, function () {
      cell.classList.add("in");
      setTimeout(function () {
        hot(keys[0]);
        /* one pass through the notes, then rest */
        tour = setInterval(function () {
          k++;
          if (k >= keys.length) { stopTour(); hot(null); return; }
          hot(keys[k]);
        }, 2600);
      }, 1700);
    }, 0.3);
  }
  function Garden(card) {
    var out = $("[data-garden-out]", card);
    var btns = $$("[data-garden-act]", card);
    var initial = out.innerHTML;
    function reset() {
      card.classList.remove("is-merged", "is-dismissed");
      btns.forEach(function (b) { b.disabled = false; });
      out.innerHTML = initial;
    }
    card.addEventListener("click", function (e) {
      if (e.target.closest("[data-undo]")) { reset(); return; }
      var b = e.target.closest("[data-garden-act]");
      if (!b || b.disabled) return;
      btns.forEach(function (x) { x.disabled = true; });
      if (b.dataset.gardenAct === "merge") {
        card.classList.add("is-merged");
        out.innerHTML = "Merged. <code>deploy-steps</code> is superseded by <code>deploy-runbook</code> and stays readable, with a pointer to its replacement. <button type=\"button\" data-undo>Undo</button>";
      } else {
        card.classList.add("is-dismissed");
        out.innerHTML = "Dismissed. The gardener won't raise it again unless new evidence turns up. <button type=\"button\" data-undo>Undo</button>";
      }
    });
  }

  /* ======================================================================
     9. THE CONSOLE -- hotspots on one screen, a lightbox for the others
     ====================================================================== */
  function Hotshot(fig) {
    var marks = $$(".hs", fig), notes = $$(".hs-notes li", fig);
    var touched = false;
    function hot(n) {
      marks.forEach(function (m) { m.classList.toggle("is-hot", m.dataset.hs === n); });
      notes.forEach(function (li) { li.classList.toggle("is-hot", li.dataset.hs === n); });
    }
    marks.concat(notes).forEach(function (el) {
      el.addEventListener("mouseenter", function () { touched = true; hot(el.dataset.hs); });
      el.addEventListener("mouseleave", function () { hot(null); });
    });
    if (reduced) return;
    /* one guided pass, then hands off */
    onceVisible(fig, async function () {
      await wait(700);
      for (var i = 1; i <= marks.length; i++) {
        if (touched) return;
        hot(String(i));
        await wait(1700);
      }
      if (!touched) hot(null);
    }, 0.45);
  }
  function Lightbox() {
    var links = $$(".thumb a");
    if (!links.length || typeof HTMLDialogElement === "undefined") return;
    var dlg = document.createElement("dialog");
    dlg.className = "lightbox";
    dlg.innerHTML = '<img alt=""><p></p>';
    document.body.appendChild(dlg);
    var img = $("img", dlg), cap = $("p", dlg);
    links.forEach(function (a) {
      a.addEventListener("click", function (e) {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
        e.preventDefault();
        var im = $("img", a);
        img.src = a.getAttribute("href");
        img.alt = im ? im.alt : "";
        var p = a.parentNode.querySelector("p");
        cap.textContent = p ? p.textContent : "";
        dlg.showModal();
      });
    });
    dlg.addEventListener("click", function () { dlg.close(); });
  }

  /* ======================================================================
     10. THE FINALE
     ====================================================================== */
  function Finale(sec) {
    if (reduced) return;
    sec.classList.add("is-armed");
    onceVisible(sec, function () { sec.classList.add("is-in"); }, 0.35);
  }

  /* ======================================================================
     11. THE PAGE REMEMBERS YOU -- locally, legibly, and you can delete it
     ====================================================================== */
  var memory = (function () {
    var KEY = "seamless.visit";
    var now = Date.now();
    var forgotten = false;
    function read() { try { return JSON.parse(localStorage.getItem(KEY) || "null"); } catch (e) { return null; } }
    function write(v) { if (forgotten) return; try { localStorage.setItem(KEY, JSON.stringify(v)); } catch (e) { /* private mode */ } }
    var prev = read();
    if (prev && typeof prev !== "object") prev = null;
    var state = {
      v: 1,
      first: prev && prev.first || now,
      last: now,
      visits: prev ? (prev.visits || 1) + (now - (prev.last || 0) > 30 * 60 * 1000 ? 1 : 0) : 1,
      furthest: prev && prev.furthest || null,
      copied: prev && prev.copied || null
    };
    var order = chapters.map(function (c) { return c.id; });
    var saveT = 0;
    function save() { clearTimeout(saveT); saveT = setTimeout(function () { write(state); }, 400); }
    save();

    function reached(id) {
      if (!id) return;
      if (!state.furthest || order.indexOf(id) > order.indexOf(state.furthest)) { state.furthest = id; save(); }
    }
    function copiedInstall() { state.copied = root.dataset.os || "unix"; save(); }
    function forget() {
      forgotten = true;
      clearTimeout(saveT);
      try { localStorage.removeItem(KEY); } catch (e) { /* nothing to remove */ }
    }
    $$("[data-forget]").forEach(function (b) {
      b.addEventListener("click", function () {
        forget();
        b.textContent = "forgotten";
        b.disabled = true;
        say("Forgotten. This visit will not be remembered.");
      });
    });

    function ago(ms) {
      var m = Math.round(ms / 60000);
      if (m < 60) return m + " minute" + (m === 1 ? "" : "s") + " ago";
      var h = Math.round(m / 60);
      if (h < 24) return h + " hour" + (h === 1 ? "" : "s") + " ago";
      var d = Math.round(h / 24);
      if (d < 14) return d === 1 ? "yesterday" : d + " days ago";
      if (d < 60) return Math.round(d / 7) + " weeks ago";
      if (d < 365) return Math.round(d / 30) + " months ago";
      var y = Math.round(d / 365);
      return y === 1 ? "about a year ago" : y + " years ago";
    }
    function title(id) {
      var sec = document.getElementById(id);
      /* an id an older visit stored may no longer name a section */
      return sec && sec.dataset.title || "";
    }
    function briefing() {
      if (!prev || !prev.last || now - prev.last < 20 * 60 * 1000) return;
      if (!prev.furthest && !prev.copied) return;
      /* early in the DOM, so the keyboard reaches it before the hero */
      var card = document.createElement("aside");
      card.className = "visit";
      card.setAttribute("aria-label", "Welcome back");
      var where = prev.furthest ? title(prev.furthest) : "";
      var lines = [];
      lines.push('<p class="v-tag">&lt;seam-briefing&gt;</p>');
      lines.push('<p class="v-line">Welcome back. ' + (where ? 'You read as far as <b>' + esc(where) + "</b>." : "") + "</p>");
      lines.push('<p class="v-line v-detail">Your last visit was <b>' + ago(now - prev.last) + "</b>.</p>");
      if (prev.copied) lines.push('<p class="v-line v-detail">You copied the install command. Next: <b>open your agent in any repo</b> and look for the briefing.</p>');
      lines.push('<div class="visit-btns"><button type="button" class="v-go">' + (prev.copied ? "Show me step two" : "Pick up where you left off") + '</button><button type="button" class="v-forget">Forget me</button></div>');
      lines.push('<p class="v-tag">&lt;/seam-briefing&gt;</p>');
      lines.push('<p class="v-fine">Kept in this browser only. Nothing leaves it.</p>');
      lines.push('<button type="button" class="visit-x" aria-label="Dismiss"><svg aria-hidden="true"><use href="#i-x"/></svg></button>');
      card.innerHTML = lines.join("");
      var skipLink = $(".skip-link");
      if (skipLink && skipLink.parentNode) skipLink.parentNode.insertBefore(card, skipLink.nextSibling);
      else document.body.appendChild(card);
      function close() {
        card.classList.remove("is-on");
        setTimeout(function () { card.remove(); }, 700);
      }
      $(".v-go", card).addEventListener("click", function () {
        var target = document.getElementById(prev.copied ? "quickstart" : prev.furthest);
        if (target) target.scrollIntoView({ behavior: reduced ? "auto" : "smooth" });
        close();
      });
      $(".v-forget", card).addEventListener("click", function () {
        forget();
        card.innerHTML = '<p class="v-line">Forgotten. This visit won\'t be remembered either.</p>';
        say("Forgotten. This visit will not be remembered.");
        setTimeout(close, 2200);
      });
      $(".visit-x", card).addEventListener("click", close);
      setTimeout(function () {
        card.classList.add("is-on");
        say("Welcome back." + (where ? " You read as far as " + where + "." : ""));
      }, reduced ? 0 : 1600);
    }
    return { reached: reached, briefing: briefing, copiedInstall: copiedInstall };
  })();

  /* ======================================================================
     12. SMALL THINGS
     ====================================================================== */
  var sky = null;
  /* THE DOCK -- the install command, within reach once the hero's own has
     scrolled away. It steps aside whenever another install command, the
     welcome-back card, or the footer is on screen, and the close button
     keeps it away for the rest of the visit (sessionStorage).
     */
  function Dock(el) {
    var KEY = "seamless.dock";
    try { if (sessionStorage.getItem(KEY) === "off") return; } catch (e) { /* private mode */ }
    if (!("IntersectionObserver" in window)) return;
    var others = $$(".install-pill, .qs-step:first-child .code, .site-foot").filter(function (n) { return !el.contains(n); });
    var inView = [], past = false, closed = false;
    el.hidden = false;
    function sync() {
      el.classList.toggle("is-on", past && !closed && !inView.length && !$(".visit"));
    }
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        var k = inView.indexOf(e.target);
        if (e.isIntersecting && k < 0) inView.push(e.target);
        else if (!e.isIntersecting && k >= 0) inView.splice(k, 1);
      });
      sync();
    });
    others.forEach(function (n) { io.observe(n); });
    scrollHooks.push(function (y) { past = y > 260; sync(); });
    var x = $("[data-dock-x]", el);
    if (x) x.addEventListener("click", function () {
      closed = true;
      try { sessionStorage.setItem(KEY, "off"); } catch (e) { /* private mode */ }
      sync();
    });
  }

  function copies() {
    document.addEventListener("click", function (e) {
      var btn = e.target.closest(".copy-btn");
      if (!btn) return;
      var cmd = btn.parentNode && $("[data-copy]", btn.parentNode);
      var isInstall = cmd && /thereisnospoon\.org\/install/.test(cmd.getAttribute("data-copy"));
      if (isInstall) memory.copiedInstall();
      /* tell assistive tech what site.js did: copied, or selected for you */
      var mo = new MutationObserver(function () {
        if (btn.classList.contains("ok")) { say("Copied to clipboard"); mo.disconnect(); }
        else if (btn.classList.contains("sel")) { say("Selected. Press Command C or Control C to copy it."); mo.disconnect(); }
      });
      mo.observe(btn, { attributes: true, attributeFilter: ["class"] });
      setTimeout(function () { mo.disconnect(); }, 2500);
      if (reduced) return;
      var r = btn.getBoundingClientRect();
      /* an install command copied anywhere sends a session into the sky */
      if (isInstall && sky) sky.visit(r.left + r.width / 2, r.top + r.height / 2);
      /* everywhere: a small burst of stars */
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
  }
  function phoneMenu() {
    var nav = $(".nav-menu");
    if (!nav) return;
    document.addEventListener("click", function (e) {
      if (nav.open && !nav.contains(e.target)) nav.open = false;
    });
    window.addEventListener("scroll", function () { if (nav.open) nav.open = false; }, { passive: true });
  }

  /* ======================================================================
     BOOT
     ====================================================================== */
  function boot() {
    reveals();
    copies();
    phoneMenu();
    if (starfield) starfield.classList.add("is-on");
    var duel = $("[data-duel]");
    if (duel) Duel(duel);
    var loop = $("[data-loop]");
    if (loop) Loop(loop);
    var board = $("[data-board]");
    if (board) Board(board);
    var skyHost = $("[data-sky-host]");
    if (skyHost) sky = Sky(skyHost);
    var file = $("[data-file]");
    if (file) Folder(file);
    var keysBox = $("[data-keys]");
    if (keysBox) Keys(keysBox);
    var layers = $("[data-layers]");
    if (layers) Layers(layers);
    var garden = $("[data-garden]");
    if (garden) Garden(garden);
    var shot = $("[data-hotshot]");
    if (shot) Hotshot(shot);
    Lightbox();
    var fin = $(".finale");
    if (fin) Finale(fin);
    var dock = $("[data-dock]");
    if (dock) Dock(dock);
    window.addEventListener("scroll", requestScroll, { passive: true });
    window.addEventListener("resize", requestScroll);
    onScroll();
    memory.briefing();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();
