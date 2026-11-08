/* Seamless console -- the System screen (templates/system.html): Orrery, a
   dependency-free WebGL2 engine; the scene that draws Seamless on it; and the
   page controller that feeds the scene the server's parts and live events.
   Three sections, one file, no build step. */

/* Seamless console -- Orrery, the small WebGL2 engine behind the System
   screen's 3D view. Dependency-free on purpose (the console has no build
   step and no vendored libraries): column-major matrices, a handful of
   procedural meshes (lathe, rounded box, torus, crystal, ribbon), four
   shaders (lit solids, the engraved plate, flowing traces, billboard light)
   and an orbit camera with picking. It knows nothing about Seamless; the
   scene (system.js) builds on it. */
(function (root) {
  'use strict';

  /* ---- vectors ------------------------------------------------------- */

  function add(a, b) { return [a[0] + b[0], a[1] + b[1], a[2] + b[2]]; }
  function sub(a, b) { return [a[0] - b[0], a[1] - b[1], a[2] - b[2]]; }
  function mulS(a, s) { return [a[0] * s, a[1] * s, a[2] * s]; }
  function dot(a, b) { return a[0] * b[0] + a[1] * b[1] + a[2] * b[2]; }
  function cross(a, b) { return [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]]; }
  function len(a) { return Math.sqrt(dot(a, a)); }
  function norm(a) { var l = len(a) || 1; return [a[0] / l, a[1] / l, a[2] / l]; }
  function mix(a, b, t) { return a + (b - a) * t; }
  function mix3(a, b, t) { return [mix(a[0], b[0], t), mix(a[1], b[1], t), mix(a[2], b[2], t)]; }
  function clamp(x, a, b) { return x < a ? a : x > b ? b : x; }
  function smooth(t) { t = clamp(t, 0, 1); return t * t * (3 - 2 * t); }
  function easeInOut(t) { t = clamp(t, 0, 1); return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; }

  /* ---- matrices (column-major, WebGL order) -------------------------- */

  function ident() { return [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1]; }

  function mul(a, b) {
    var o = new Array(16);
    for (var c = 0; c < 4; c++) {
      for (var r = 0; r < 4; r++) {
        o[c * 4 + r] = a[r] * b[c * 4] + a[4 + r] * b[c * 4 + 1] + a[8 + r] * b[c * 4 + 2] + a[12 + r] * b[c * 4 + 3];
      }
    }
    return o;
  }

  function perspective(fovy, aspect, near, far) {
    var f = 1 / Math.tan(fovy / 2), nf = 1 / (near - far);
    return [f / aspect, 0, 0, 0, 0, f, 0, 0, 0, 0, (far + near) * nf, -1, 0, 0, 2 * far * near * nf, 0];
  }

  function lookAt(eye, at, up) {
    var z = norm(sub(eye, at)), x = norm(cross(up, z)), y = cross(z, x);
    return [x[0], y[0], z[0], 0, x[1], y[1], z[1], 0, x[2], y[2], z[2], 0, -dot(x, eye), -dot(y, eye), -dot(z, eye), 1];
  }

  // trs composes translate * rotY * rotX * rotZ * scale. s is a number or [x,y,z].
  function trs(t, ry, s, rx, rz) {
    var sx, sy, sz;
    if (typeof s === 'number') { sx = sy = sz = s; } else if (s) { sx = s[0]; sy = s[1]; sz = s[2]; } else { sx = sy = sz = 1; }
    var cy = Math.cos(ry || 0), syn = Math.sin(ry || 0);
    var cx = Math.cos(rx || 0), sxn = Math.sin(rx || 0);
    var cz = Math.cos(rz || 0), szn = Math.sin(rz || 0);
    // R = Ry * Rx * Rz
    var m00 = cy * cz + syn * sxn * szn, m01 = -cy * szn + syn * sxn * cz, m02 = syn * cx;
    var m10 = cx * szn, m11 = cx * cz, m12 = -sxn;
    var m20 = -syn * cz + cy * sxn * szn, m21 = syn * szn + cy * sxn * cz, m22 = cy * cx;
    return [
      m00 * sx, m10 * sx, m20 * sx, 0,
      m01 * sy, m11 * sy, m21 * sy, 0,
      m02 * sz, m12 * sz, m22 * sz, 0,
      t ? t[0] : 0, t ? t[1] : 0, t ? t[2] : 0, 1
    ];
  }

  function invert(m) {
    var a00 = m[0], a01 = m[1], a02 = m[2], a03 = m[3], a10 = m[4], a11 = m[5], a12 = m[6], a13 = m[7];
    var a20 = m[8], a21 = m[9], a22 = m[10], a23 = m[11], a30 = m[12], a31 = m[13], a32 = m[14], a33 = m[15];
    var b00 = a00 * a11 - a01 * a10, b01 = a00 * a12 - a02 * a10, b02 = a00 * a13 - a03 * a10;
    var b03 = a01 * a12 - a02 * a11, b04 = a01 * a13 - a03 * a11, b05 = a02 * a13 - a03 * a12;
    var b06 = a20 * a31 - a21 * a30, b07 = a20 * a32 - a22 * a30, b08 = a20 * a33 - a23 * a30;
    var b09 = a21 * a32 - a22 * a31, b10 = a21 * a33 - a23 * a31, b11 = a22 * a33 - a23 * a32;
    var det = b00 * b11 - b01 * b10 + b02 * b09 + b03 * b08 - b04 * b07 + b05 * b06;
    if (!det) return ident();
    det = 1 / det;
    return [
      (a11 * b11 - a12 * b10 + a13 * b09) * det, (a02 * b10 - a01 * b11 - a03 * b09) * det,
      (a31 * b05 - a32 * b04 + a33 * b03) * det, (a22 * b04 - a21 * b05 - a23 * b03) * det,
      (a12 * b08 - a10 * b11 - a13 * b07) * det, (a00 * b11 - a02 * b08 + a03 * b07) * det,
      (a32 * b02 - a30 * b05 - a33 * b01) * det, (a20 * b05 - a22 * b02 + a23 * b01) * det,
      (a10 * b10 - a11 * b08 + a13 * b06) * det, (a01 * b08 - a00 * b10 - a03 * b06) * det,
      (a30 * b04 - a31 * b02 + a33 * b00) * det, (a21 * b02 - a20 * b04 - a23 * b00) * det,
      (a11 * b07 - a10 * b09 - a12 * b06) * det, (a00 * b09 - a01 * b07 + a02 * b06) * det,
      (a31 * b01 - a30 * b03 - a32 * b00) * det, (a20 * b03 - a21 * b01 + a22 * b00) * det
    ];
  }

  // normalMat is the inverse-transpose of m's upper 3x3, as a 9-element array.
  function normalMat(m) {
    var a = m[0], b = m[1], c = m[2], d = m[4], e = m[5], f = m[6], g = m[8], h = m[9], i = m[10];
    var A = e * i - f * h, B = -(d * i - f * g), C = d * h - e * g;
    var det = a * A + b * B + c * C || 1;
    var inv = 1 / det;
    return [
      A * inv, B * inv, C * inv,
      -(b * i - c * h) * inv, (a * i - c * g) * inv, -(a * h - b * g) * inv,
      (b * f - c * e) * inv, -(a * f - c * d) * inv, (a * e - b * d) * inv
    ];
  }

  function xform(m, p) {
    var x = p[0], y = p[1], z = p[2];
    var w = m[3] * x + m[7] * y + m[11] * z + m[15] || 1;
    return [(m[0] * x + m[4] * y + m[8] * z + m[12]) / w, (m[1] * x + m[5] * y + m[9] * z + m[13]) / w, (m[2] * x + m[6] * y + m[10] * z + m[14]) / w];
  }

  /* ---- geometry ------------------------------------------------------- */
  // A geometry is {p: [x,y,z,...], n: [...], i: [...]} in plain arrays until
  // upload; merge() bakes transforms so a model's static parts share one
  // draw call.

  function geo() { return { p: [], n: [], i: [] }; }

  // lathe revolves a profile of [r, y] points (bottom to top) around Y. A
  // turn sharper than creaseDeg splits the vertex, so caps stay crisp while
  // curves stay smooth.
  function lathe(profile, seg, creaseDeg, arc) {
    var g = geo(), cosT = Math.cos((creaseDeg || 38) * Math.PI / 180), segN = [], j, i;
    arc = arc || Math.PI * 2;
    for (j = 0; j < profile.length - 1; j++) {
      var dr = profile[j + 1][0] - profile[j][0], dy = profile[j + 1][1] - profile[j][1];
      var l = Math.sqrt(dr * dr + dy * dy) || 1;
      segN.push([dy / l, -dr / l]);
    }
    var rings = [];
    for (j = 0; j < profile.length; j++) {
      var p = profile[j], na = segN[j - 1], nb = segN[j];
      if (!na) rings.push([p[0], p[1], nb[0], nb[1]]);
      else if (!nb) rings.push([p[0], p[1], na[0], na[1]]);
      else if (na[0] * nb[0] + na[1] * nb[1] < cosT) rings.push([p[0], p[1], na[0], na[1]], [p[0], p[1], nb[0], nb[1]]);
      else {
        var mr = na[0] + nb[0], my = na[1] + nb[1], ml = Math.sqrt(mr * mr + my * my) || 1;
        rings.push([p[0], p[1], mr / ml, my / ml]);
      }
    }
    var rn = rings.length;
    for (i = 0; i <= seg; i++) {
      var th = i / seg * arc, c = Math.cos(th), s = Math.sin(th);
      for (j = 0; j < rn; j++) {
        var q = rings[j];
        g.p.push(q[0] * c, q[1], q[0] * s);
        g.n.push(q[2] * c, q[3], q[2] * s);
      }
    }
    for (i = 0; i < seg; i++) {
      for (j = 0; j < rn - 1; j++) {
        var a0 = i * rn + j, a1 = a0 + 1, b0 = (i + 1) * rn + j, b1 = b0 + 1;
        g.i.push(a0, a1, b0, a1, b1, b0);
      }
    }
    return g;
  }

  function cylinder(r, h, seg, bevel) {
    var b = bevel || 0, y0 = 0, y1 = h;
    var prof = b ? [[0, y0], [r - b, y0], [r, y0 + b], [r, y1 - b], [r - b, y1], [0, y1]] : [[0, y0], [r, y0], [r, y1], [0, y1]];
    return lathe(prof, seg || 40, b ? 50 : 38);
  }

  function sphere(r, seg) {
    var prof = [], n = Math.max(6, Math.round((seg || 32) / 2));
    for (var k = 0; k <= n; k++) {
      var a = -Math.PI / 2 + k / n * Math.PI;
      prof.push([Math.cos(a) * r, Math.sin(a) * r]);
    }
    prof[0][0] = 0; prof[n][0] = 0;
    return lathe(prof, seg || 32, 89);
  }

  function torus(R, r, seg, side, arc) {
    var g = geo(), i, j;
    seg = seg || 64; side = side || 16; arc = arc || Math.PI * 2;
    for (i = 0; i <= seg; i++) {
      var u = i / seg * arc, cu = Math.cos(u), su = Math.sin(u);
      for (j = 0; j <= side; j++) {
        var v = j / side * Math.PI * 2, cv = Math.cos(v), sv = Math.sin(v);
        g.p.push((R + r * cv) * cu, r * sv, (R + r * cv) * su);
        g.n.push(cv * cu, sv, cv * su);
      }
    }
    for (i = 0; i < seg; i++) {
      for (j = 0; j < side; j++) {
        var a = i * (side + 1) + j, b = a + side + 1;
        g.i.push(a, a + 1, b, a + 1, b + 1, b);
      }
    }
    return g;
  }

  // rbox is a box with rounded edges of radius r: each face is a grid whose
  // edge samples are spaced so the clamp-and-offset mapping lands them on
  // even angles of the bevel.
  function rbox(w, h, d, r) {
    var g = geo(), hw = w / 2, hh = h / 2, hd = d / 2;
    r = Math.min(r || 0, hw, hh, hd);
    var ix = hw - r, iy = hh - r, iz = hd - r;
    function coords(half, inner) {
      if (r <= 0) return [-half, half];
      var out = [], steps = 3, k;
      for (k = steps; k >= 0; k--) out.push(-(inner + r * Math.tan(k / steps * Math.PI / 4)));
      for (k = 0; k <= steps; k++) out.push(inner + r * Math.tan(k / steps * Math.PI / 4));
      return out;
    }
    var cx = coords(hw, ix), cy = coords(hh, iy), cz = coords(hd, iz);
    function face(axis, sign, us, vs) {
      var base = g.p.length / 3;
      for (var a = 0; a < us.length; a++) {
        for (var b = 0; b < vs.length; b++) {
          var p;
          if (axis === 0) p = [sign * hw, us[a], vs[b]];
          else if (axis === 1) p = [us[a], sign * hh, vs[b]];
          else p = [us[a], vs[b], sign * hd];
          var q = [clamp(p[0], -ix, ix), clamp(p[1], -iy, iy), clamp(p[2], -iz, iz)];
          var dvec = sub(p, q), dl = len(dvec), n;
          if (dl < 1e-6) { n = [0, 0, 0]; n[axis] = sign; } else n = mulS(dvec, 1 / dl);
          var pos = r > 0 ? add(q, mulS(n, r)) : p;
          g.p.push(pos[0], pos[1], pos[2]);
          g.n.push(n[0], n[1], n[2]);
        }
      }
      var nv = vs.length;
      for (a = 0; a < us.length - 1; a++) {
        for (b = 0; b < nv - 1; b++) {
          var i0 = base + a * nv + b, i1 = i0 + 1, i2 = i0 + nv, i3 = i2 + 1;
          // winding: keep front faces outward for every face orientation
          var flip = (axis === 0 && sign > 0) || (axis === 1 && sign < 0) || (axis === 2 && sign > 0);
          if (flip) g.i.push(i0, i2, i1, i1, i2, i3); else g.i.push(i0, i1, i2, i1, i3, i2);
        }
      }
    }
    face(0, 1, cy, cz); face(0, -1, cy, cz);
    face(1, 1, cx, cz); face(1, -1, cx, cz);
    face(2, 1, cx, cy); face(2, -1, cx, cy);
    return g;
  }

  // crystal is an icosahedron with flat facets (normals per face).
  function crystal(r, stretch) {
    var t = (1 + Math.sqrt(5)) / 2, s = stretch || 1;
    var v = [[-1, t, 0], [1, t, 0], [-1, -t, 0], [1, -t, 0], [0, -1, t], [0, 1, t], [0, -1, -t], [0, 1, -t], [t, 0, -1], [t, 0, 1], [-t, 0, -1], [-t, 0, 1]];
    var f = [[0, 11, 5], [0, 5, 1], [0, 1, 7], [0, 7, 10], [0, 10, 11], [1, 5, 9], [5, 11, 4], [11, 10, 2], [10, 7, 6], [7, 1, 8],
      [3, 9, 4], [3, 4, 2], [3, 2, 6], [3, 6, 8], [3, 8, 9], [4, 9, 5], [2, 4, 11], [6, 2, 10], [8, 6, 7], [9, 8, 1]];
    var g = geo();
    for (var k = 0; k < v.length; k++) { var nn = norm(v[k]); v[k] = [nn[0] * r, nn[1] * r * s, nn[2] * r]; }
    for (k = 0; k < f.length; k++) {
      var a = v[f[k][0]], b = v[f[k][1]], c = v[f[k][2]];
      var n = norm(cross(sub(b, a), sub(c, a)));
      var base = g.p.length / 3;
      g.p.push(a[0], a[1], a[2], b[0], b[1], b[2], c[0], c[1], c[2]);
      g.n.push(n[0], n[1], n[2], n[0], n[1], n[2], n[0], n[1], n[2]);
      g.i.push(base, base + 1, base + 2);
    }
    return g;
  }

  // prism is an upright triangular prism (flat-shaded).
  function prism(r, h) {
    var g = geo(), pts = [], k;
    for (k = 0; k < 3; k++) { var a = k / 3 * Math.PI * 2 + Math.PI / 2; pts.push([Math.cos(a) * r, Math.sin(a) * r]); }
    function quad(p0, p1, p2, p3) {
      var n = norm(cross(sub(p1, p0), sub(p2, p0))), b = g.p.length / 3;
      [p0, p1, p2, p3].forEach(function (p) { g.p.push(p[0], p[1], p[2]); g.n.push(n[0], n[1], n[2]); });
      g.i.push(b, b + 1, b + 2, b, b + 2, b + 3);
    }
    for (k = 0; k < 3; k++) {
      var A = pts[k], B = pts[(k + 1) % 3];
      quad([A[0], 0, A[1]], [A[0], h, A[1]], [B[0], h, B[1]], [B[0], 0, B[1]]);
    }
    var top = g.p.length / 3;
    pts.forEach(function (p) { g.p.push(p[0], h, p[1]); g.n.push(0, 1, 0); });
    g.i.push(top, top + 2, top + 1);
    var bot = g.p.length / 3;
    pts.forEach(function (p) { g.p.push(p[0], 0, p[1]); g.n.push(0, -1, 0); });
    g.i.push(bot, bot + 1, bot + 2);
    return g;
  }

  // merge bakes each [geometry, matrix] pair into one geometry.
  function merge(list) {
    var g = geo();
    list.forEach(function (it) {
      var src = it[0], m = it[1] || ident(), nm = normalMat(m), base = g.p.length / 3;
      for (var k = 0; k < src.p.length; k += 3) {
        var p = xform(m, [src.p[k], src.p[k + 1], src.p[k + 2]]);
        var x = src.n[k], y = src.n[k + 1], z = src.n[k + 2];
        var n = norm([nm[0] * x + nm[3] * y + nm[6] * z, nm[1] * x + nm[4] * y + nm[7] * z, nm[2] * x + nm[5] * y + nm[8] * z]);
        g.p.push(p[0], p[1], p[2]);
        g.n.push(n[0], n[1], n[2]);
      }
      for (k = 0; k < src.i.length; k++) g.i.push(src.i[k] + base);
    });
    return g;
  }

  // ribbon lays a flat strip of width w along an XZ polyline; y is a height
  // or a function of (x, z). Its second attribute carries [distance along,
  // side] for the trace shader.
  function ribbon(pts, w, y) {
    var g = { p: [], n: [], i: [] }, acc = 0;
    for (var k = 0; k < pts.length; k++) {
      var a = pts[Math.max(0, k - 1)], b = pts[Math.min(pts.length - 1, k + 1)];
      var dx = b[0] - a[0], dz = b[1] - a[1], l = Math.sqrt(dx * dx + dz * dz) || 1;
      var nx = -dz / l * w / 2, nz = dx / l * w / 2;
      if (k > 0) { var px = pts[k][0] - pts[k - 1][0], pz = pts[k][1] - pts[k - 1][1]; acc += Math.sqrt(px * px + pz * pz); }
      var hy = typeof y === 'function' ? y(pts[k][0], pts[k][1]) : y;
      g.p.push(pts[k][0] + nx, hy, pts[k][1] + nz, pts[k][0] - nx, hy, pts[k][1] - nz);
      g.n.push(acc, -1, 0, acc, 1, 0);
      if (k > 0) { var i0 = (k - 1) * 2; g.i.push(i0, i0 + 1, i0 + 2, i0 + 1, i0 + 3, i0 + 2); }
    }
    g.length = acc;
    return g;
  }

  // curve samples a quadratic bezier between two XZ points that bows by
  // `bend` (a fraction of the distance) to the left of travel.
  function curve(a, b, bend, steps) {
    var mx = (a[0] + b[0]) / 2, mz = (a[1] + b[1]) / 2, dx = b[0] - a[0], dz = b[1] - a[1];
    var cx = mx - dz * (bend || 0), cz = mz + dx * (bend || 0), out = [];
    steps = steps || 24;
    for (var k = 0; k <= steps; k++) {
      var t = k / steps, u = 1 - t;
      out.push([u * u * a[0] + 2 * u * t * cx + t * t * b[0], u * u * a[1] + 2 * u * t * cz + t * t * b[1]]);
    }
    return out;
  }

  // arcPts samples a circle arc of radius r from angle a0 to a1 (radians,
  // measured from +Z toward +X, the scene's convention).
  function arcPts(r, a0, a1, steps) {
    var out = [];
    steps = steps || 32;
    for (var k = 0; k <= steps; k++) { var a = mix(a0, a1, k / steps); out.push([Math.sin(a) * r, Math.cos(a) * r]); }
    return out;
  }

  /* ---- colors ---------------------------------------------------------- */

  var probe = null;
  // color resolves any CSS color (a token's value included) to linear-ish
  // [r,g,b,a] in 0..1 by letting the browser parse it.
  function color(css) {
    if (!probe) { probe = document.createElement('canvas').getContext('2d'); }
    probe.fillStyle = '#000';
    probe.fillStyle = css;
    var v = probe.fillStyle, m;
    if (v.charAt(0) === '#') {
      return [parseInt(v.substr(1, 2), 16) / 255, parseInt(v.substr(3, 2), 16) / 255, parseInt(v.substr(5, 2), 16) / 255, 1];
    }
    m = v.match(/rgba?\(([^)]+)\)/);
    if (m) {
      var p = m[1].split(',').map(function (s) { return parseFloat(s); });
      return [p[0] / 255, p[1] / 255, p[2] / 255, p.length > 3 ? p[3] : 1];
    }
    return [0.5, 0.5, 0.5, 1];
  }
  function rgbMix(a, b, t) { return [mix(a[0], b[0], t), mix(a[1], b[1], t), mix(a[2], b[2], t), mix(a[3] == null ? 1 : a[3], b[3] == null ? 1 : b[3], t)]; }
  function luma(c) { return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]; }

  /* ---- shaders -------------------------------------------------------- */

  var VS_LIT = [
    '#version 300 es',
    'layout(location=0) in vec3 aPos;',
    'layout(location=1) in vec3 aNrm;',
    'uniform mat4 uModel; uniform mat4 uVP; uniform mat3 uNM;',
    'out vec3 vW; out vec3 vN;',
    'void main(){ vec4 w = uModel * vec4(aPos, 1.0); vW = w.xyz; vN = uNM * aNrm; gl_Position = uVP * w; }'
  ].join('\n');

  var FS_LIT = [
    '#version 300 es',
    'precision highp float;',
    'in vec3 vW; in vec3 vN; out vec4 o;',
    'uniform vec3 uCam; uniform vec3 uColor; uniform vec3 uEmis; uniform float uEmit;',
    'uniform float uAlpha; uniform float uGrey; uniform float uRim; uniform float uFlat; uniform float uGloss;',
    'uniform vec3 uSky; uniform vec3 uGround; uniform vec3 uFog; uniform vec2 uFogR; uniform vec3 uRimC; uniform float uDark;',
    'void main(){',
    '  vec3 N = normalize(vN);',
    '  if (uFlat > 0.5) N = normalize(cross(dFdx(vW), dFdy(vW)));',
    '  if (!gl_FrontFacing && uFlat < 0.5) N = -N;',
    '  vec3 V = normalize(uCam - vW);',
    '  vec3 L = normalize(vec3(-0.45, 1.0, 0.35));',
    '  vec3 L2 = normalize(vec3(0.6, 0.35, -0.7));',
    '  float diff = max(dot(N, L), 0.0);',
    '  float fill = max(dot(N, L2), 0.0) * 0.28;',
    '  float hemi = 0.5 + 0.5 * N.y;',
    '  vec3 H = normalize(L + V);',
    '  float spec = pow(max(dot(N, H), 0.0), mix(18.0, 90.0, uGloss)) * mix(0.12, 0.55, uGloss);',
    '  float fres = pow(1.0 - max(dot(N, V), 0.0), 2.6);',
    '  vec3 amb = mix(uGround, uSky, hemi);',
    '  vec3 c = uColor * (amb * 0.62 + diff * 0.72 + fill) + spec * mix(vec3(1.0), uColor, 0.25);',
    '  c += fres * uRim * uRimC;',
    '  c += uEmis * uEmit;',
    '  float l = dot(c, vec3(0.2126, 0.7152, 0.0722));',
    '  c = mix(c, vec3(l) * mix(0.92, 1.25, uDark) + uFog * 0.1, uGrey);',
    '  float d = length(uCam - vW);',
    '  c = mix(c, uFog, smoothstep(uFogR.x, uFogR.y, d) * 0.85);',
    '  o = vec4(c, uAlpha);',
    '}'
  ].join('\n');

  // The plate: the base's deck. Zone rings and ticks come from polar
  // coordinates; the work and door zones carry a faint hex deck grid.
  var FS_PLATE = [
    '#version 300 es',
    'precision highp float;',
    'in vec3 vW; in vec3 vN; out vec4 o;',
    'uniform vec3 uCam; uniform vec3 uColor; uniform vec3 uLine; uniform vec3 uGlow; uniform vec3 uFog; uniform vec2 uFogR;',
    'uniform vec4 uRings; uniform float uTime; uniform float uDark; uniform float uR; uniform float uHex;',
    'float ring(float r, float at, float w){ return 1.0 - smoothstep(0.0, w, abs(r - at)); }',
    'float hexDist(vec2 p){ p = abs(p); return max(dot(p, vec2(0.5, 0.8660254)), p.x); }',
    'vec2 hexLocal(vec2 uv){ vec2 r = vec2(1.0, 1.7320508); vec2 h = r * 0.5; vec2 a = mod(uv, r) - h; vec2 b = mod(uv - h, r) - h; return dot(a, a) < dot(b, b) ? a : b; }',
    'void main(){',
    '  float r = length(vW.xz);',
    '  float a = atan(vW.x, vW.z);',
    '  float px = fwidth(r) * 1.2;',
    '  vec3 c = uColor;',
    '  float e = 0.0;',
    '  e += ring(r, uRings.x, px) * 0.9 + ring(r, uRings.y, px) * 0.5 + ring(r, uRings.z, px) * 0.9;',
    '  e += ring(r, uRings.w, px * 1.6) * 1.2;',
    '  vec2 huv = vW.xz / uHex;',
    '  float he = 0.5 - hexDist(hexLocal(huv));',
    '  float hw = fwidth(huv.x) * 1.3;',
    '  float deck = step(uRings.x + 0.4, r) * (1.0 - smoothstep(uRings.w - 2.5, uRings.w - 0.8, r));',
    '  e += (1.0 - smoothstep(0.0, hw, he)) * deck * 0.32;',
    '  float tk = abs(fract(a / 6.2831853 * 72.0) - 0.5);',
    '  float tw = fwidth(a / 6.2831853 * 72.0);',
    '  float band = step(uRings.z - 1.0, r) * step(r, uRings.z - 0.3);',
    '  e += (1.0 - smoothstep(0.0, tw * 1.5, tk)) * band * 0.8;',
    '  float tk2 = abs(fract(a / 6.2831853 * 12.0) - 0.5);',
    '  float band2 = step(uRings.w - 1.8, r) * step(r, uRings.w - 0.4);',
    '  e += (1.0 - smoothstep(0.0, fwidth(a / 6.2831853 * 12.0) * 1.5, tk2)) * band2 * 0.9;',
    '  c = mix(c, uLine, clamp(e, 0.0, 1.0) * 0.55);',
    '  c += uGlow * smoothstep(uRings.w * 0.2, 0.0, r) * 0.1 * mix(0.35, 1.0, uDark);',
    '  float edge = smoothstep(uR - 0.3, uR, r);',
    '  c = mix(c, uLine, edge * 0.4);',
    '  float d = length(uCam - vW);',
    '  c = mix(c, uFog, smoothstep(uFogR.x, uFogR.y, d) * 0.85);',
    '  o = vec4(c, 1.0);',
    '}'
  ].join('\n');

  var VS_TRACE = [
    '#version 300 es',
    'layout(location=0) in vec3 aPos;',
    'layout(location=1) in vec3 aAlong;',
    'uniform mat4 uVP;',
    'out vec2 vA; out vec3 vW;',
    'void main(){ vA = aAlong.xy; vW = aPos; gl_Position = uVP * vec4(aPos, 1.0); }'
  ].join('\n');

  // A trace is a faint static line; light moves along it only while data
  // does: uPk holds up to four packet positions (distance along the trace,
  // negative when unused) and uDir their direction of travel.
  var FS_TRACE = [
    '#version 300 es',
    'precision highp float;',
    'in vec2 vA; in vec3 vW; out vec4 o;',
    'uniform vec3 uColor; uniform float uOn; uniform float uLen; uniform float uSel; uniform float uDark;',
    'uniform vec4 uPk; uniform vec4 uDir;',
    'float comet(float x, float p, float dir){',
    '  if (p < 0.0) return 0.0;',
    '  float d = (x - p) * dir;',
    '  return d > 0.0 ? exp(-d * d * 6.0) : exp(-d * d * 0.35);',
    '}',
    'void main(){',
    '  float side = 1.0 - abs(vA.y);',
    '  float core = smoothstep(0.0, 0.7, side);',
    '  float dash = step(0.5, fract(vA.x / 0.6));',
    '  float base = mix(0.12 * dash, 0.22, uOn);',
    '  float hot = comet(vA.x, uPk.x, uDir.x) + comet(vA.x, uPk.y, uDir.y) + comet(vA.x, uPk.z, uDir.z) + comet(vA.x, uPk.w, uDir.w);',
    '  float a = (base + hot * 1.1 + uSel * 0.4) * core;',
    '  float ends = smoothstep(0.0, 0.8, vA.x) * smoothstep(0.0, 0.8, uLen - vA.x);',
    '  o = vec4(uColor * (1.0 + hot * 0.7), a * ends * mix(0.9, 1.0, uDark));',
    '}'
  ].join('\n');

  var VS_SPRITE = [
    '#version 300 es',
    'layout(location=0) in vec2 aCorner;',
    'layout(location=1) in vec4 aCS;',
    'layout(location=2) in vec4 aCol;',
    'uniform mat4 uVP; uniform vec3 uRight; uniform vec3 uUp;',
    'out vec2 vUV; out vec4 vCol;',
    'void main(){ vec3 p = aCS.xyz + (uRight * aCorner.x + uUp * aCorner.y) * aCS.w; vUV = aCorner; vCol = aCol; gl_Position = uVP * vec4(p, 1.0); }'
  ].join('\n');

  var FS_SPRITE = [
    '#version 300 es',
    'precision highp float;',
    'in vec2 vUV; in vec4 vCol; out vec4 o;',
    'void main(){ float d = length(vUV); if (d > 1.0) discard;',
    '  float halo = pow(1.0 - d, 2.4); float core = smoothstep(0.32, 0.0, d);',
    '  o = vec4(vCol.rgb, vCol.a * clamp(halo * 0.8 + core * 0.9, 0.0, 1.0)); }'
  ].join('\n');

  var VS_SKY = [
    '#version 300 es',
    'layout(location=0) in vec2 aCorner;',
    'out vec2 vP;',
    'void main(){ vP = aCorner; gl_Position = vec4(aCorner, 0.9999, 1.0); }'
  ].join('\n');

  var FS_SKY = [
    '#version 300 es',
    'precision highp float;',
    'in vec2 vP; out vec4 o;',
    'uniform vec3 uTop; uniform vec3 uBot; uniform vec3 uA; uniform vec3 uB; uniform vec2 uAsp; uniform float uStars; uniform float uTime; uniform vec2 uLook;',
    'float h(vec2 p){ return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }',
    'void main(){',
    '  vec2 uv = vP * 0.5 + 0.5;',
    '  vec3 c = mix(uBot, uTop, smoothstep(0.0, 1.0, uv.y));',
    '  vec2 q = vec2(vP.x * uAsp.x, vP.y);',
    '  c += uA * smoothstep(1.1, 0.0, length(q - vec2(0.75, 0.75)));',
    '  c += uB * smoothstep(1.2, 0.0, length(q - vec2(-0.9, -0.2)));',
    '  vec2 g = (q + uLook) * 90.0;',
    '  vec2 cell = floor(g); vec2 f = fract(g) - 0.5;',
    '  float s = h(cell); vec2 off = vec2(h(cell + 7.1), h(cell + 3.3)) - 0.5;',
    '  float star = step(0.985, s) * smoothstep(0.09, 0.0, length(f - off * 0.6));',
    '  float tw = 0.55 + 0.45 * fract(s * 91.7);',
    '  c += vec3(star * tw * uStars);',
    '  o = vec4(c, 1.0);',
    '}'
  ].join('\n');

  function compile(gl, type, src) {
    var sh = gl.createShader(type);
    gl.shaderSource(sh, src);
    gl.compileShader(sh);
    if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
      var log = gl.getShaderInfoLog(sh);
      gl.deleteShader(sh);
      throw new Error('orrery: shader: ' + log);
    }
    return sh;
  }

  function program(gl, vs, fs) {
    var p = gl.createProgram();
    gl.attachShader(p, compile(gl, gl.VERTEX_SHADER, vs));
    gl.attachShader(p, compile(gl, gl.FRAGMENT_SHADER, fs));
    gl.linkProgram(p);
    if (!gl.getProgramParameter(p, gl.LINK_STATUS)) throw new Error('orrery: link: ' + gl.getProgramInfoLog(p));
    var u = {}, n = gl.getProgramParameter(p, gl.ACTIVE_UNIFORMS);
    for (var k = 0; k < n; k++) { var info = gl.getActiveUniform(p, k); u[info.name] = gl.getUniformLocation(p, info.name); }
    return { p: p, u: u };
  }

  /* ---- renderer ------------------------------------------------------- */

  function Renderer(canvas) {
    // low-power: on a machine with two GPUs this stays on the integrated one
    var gl = canvas.getContext('webgl2', { antialias: true, alpha: false, premultipliedAlpha: false, powerPreference: 'low-power', desynchronized: false });
    if (!gl) throw new Error('orrery: WebGL2 is not available');
    this.gl = gl;
    this.canvas = canvas;
    this.progs = {
      lit: program(gl, VS_LIT, FS_LIT),
      plate: program(gl, VS_LIT, FS_PLATE),
      trace: program(gl, VS_TRACE, FS_TRACE),
      sprite: program(gl, VS_SPRITE, FS_SPRITE),
      sky: program(gl, VS_SKY, FS_SKY)
    };
    // shared quads
    this.quad = this._quad();
    this.spriteBuf = gl.createBuffer();
    this.spriteCap = 0;
    this.spriteVAO = this._spriteVAO();
    gl.enable(gl.DEPTH_TEST);
    gl.enable(gl.CULL_FACE);
    gl.cullFace(gl.BACK);
  }

  Renderer.prototype._quad = function () {
    var gl = this.gl, vao = gl.createVertexArray(), b = gl.createBuffer();
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, b);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
    return { vao: vao, buf: b };
  };

  Renderer.prototype._spriteVAO = function () {
    var gl = this.gl, vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.quad.buf);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.spriteBuf);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 4, gl.FLOAT, false, 32, 0);
    gl.vertexAttribDivisor(1, 1);
    gl.enableVertexAttribArray(2);
    gl.vertexAttribPointer(2, 4, gl.FLOAT, false, 32, 16);
    gl.vertexAttribDivisor(2, 1);
    gl.bindVertexArray(null);
    return vao;
  };

  // mesh uploads a geometry; `along` marks a ribbon (its n attribute carries
  // the trace coordinates, not normals).
  Renderer.prototype.mesh = function (g) {
    var gl = this.gl, vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    var pb = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, pb);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(g.p), gl.STATIC_DRAW);
    gl.enableVertexAttribArray(0);
    gl.vertexAttribPointer(0, 3, gl.FLOAT, false, 0, 0);
    var nb = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, nb);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(g.n), gl.STATIC_DRAW);
    gl.enableVertexAttribArray(1);
    gl.vertexAttribPointer(1, 3, gl.FLOAT, false, 0, 0);
    var big = g.p.length / 3 > 65535;
    var ib = gl.createBuffer();
    gl.bindBuffer(gl.ELEMENT_ARRAY_BUFFER, ib);
    gl.bufferData(gl.ELEMENT_ARRAY_BUFFER, big ? new Uint32Array(g.i) : new Uint16Array(g.i), gl.STATIC_DRAW);
    gl.bindVertexArray(null);
    return { vao: vao, count: g.i.length, type: big ? gl.UNSIGNED_INT : gl.UNSIGNED_SHORT, length: g.length || 0, bufs: [pb, nb, ib] };
  };

  // release frees the GPU context at once instead of waiting for collection.
  Renderer.prototype.release = function () {
    var ext = this.gl.getExtension('WEBGL_lose_context');
    if (ext) ext.loseContext();
  };

  // resize sizes the drawing buffer; it reports a change, because a resized
  // canvas is cleared and must be drawn again at once.
  Renderer.prototype.resize = function (w, h, dpr) {
    var c = this.canvas, W = Math.max(1, Math.round(w * dpr)), H = Math.max(1, Math.round(h * dpr)), changed = false;
    if (c.width !== W || c.height !== H) { c.width = W; c.height = H; changed = true; }
    this.gl.viewport(0, 0, W, H);
    return changed;
  };

  Renderer.prototype.sprites = function (data, count) {
    var gl = this.gl;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.spriteBuf);
    if (count > this.spriteCap) {
      this.spriteCap = Math.max(count, this.spriteCap * 2, 256);
      gl.bufferData(gl.ARRAY_BUFFER, this.spriteCap * 32, gl.DYNAMIC_DRAW);
    }
    gl.bufferSubData(gl.ARRAY_BUFFER, 0, data, 0, count * 8);
  };

  /* ---- camera --------------------------------------------------------- */

  // Orbit: yaw/pitch/dist around a target, eased toward goals every frame.
  function Camera() {
    this.target = [0, 0, 0]; this.yaw = 0; this.pitch = 0.62; this.dist = 90;
    this.goal = { target: [0, 0, 0], yaw: 0, pitch: 0.62, dist: 90 };
    this.fov = 0.62; this.aspect = 1; this.minD = 8; this.maxD = 175;
    this.fly = null;
  }
  Camera.prototype.eye = function () {
    var cp = Math.cos(this.pitch);
    return [this.target[0] + Math.sin(this.yaw) * cp * this.dist, this.target[1] + Math.sin(this.pitch) * this.dist, this.target[2] + Math.cos(this.yaw) * cp * this.dist];
  };
  Camera.prototype.step = function (dt, calm) {
    var g = this.goal, k = calm ? 1 : 1 - Math.pow(0.0009, dt);
    if (calm) this.shiftNow = this.shift || 0;
    if (this.fly) {
      var f = this.fly; f.t += dt / f.dur;
      var e = easeInOut(f.t);
      this.target = mix3(f.from.target, f.to.target, e);
      this.yaw = mix(f.from.yaw, f.to.yaw, e);
      this.pitch = mix(f.from.pitch, f.to.pitch, e);
      // a gentle pull-back at mid flight reads as travel, not a cut
      this.dist = mix(f.from.dist, f.to.dist, e) + Math.sin(e * Math.PI) * f.lift;
      if (f.t >= 1) { this.fly = null; g.target = this.target.slice(); g.yaw = this.yaw; g.pitch = this.pitch; g.dist = this.dist; }
      return true;
    }
    var moving = Math.abs(g.yaw - this.yaw) + Math.abs(g.pitch - this.pitch) + Math.abs(g.dist - this.dist) + len(sub(g.target, this.target)) > 1e-4;
    this.yaw = mix(this.yaw, g.yaw, k);
    this.pitch = mix(this.pitch, g.pitch, k);
    this.dist = mix(this.dist, g.dist, k);
    this.target = mix3(this.target, g.target, k);
    return moving;
  };
  Camera.prototype.flyTo = function (to, dur, calm) {
    // take the short way round
    var dy = to.yaw - this.yaw;
    while (dy > Math.PI) { to.yaw -= Math.PI * 2; dy -= Math.PI * 2; }
    while (dy < -Math.PI) { to.yaw += Math.PI * 2; dy += Math.PI * 2; }
    if (calm) {
      this.fly = null; this.target = to.target.slice(); this.yaw = to.yaw; this.pitch = to.pitch; this.dist = to.dist;
      this.goal = { target: to.target.slice(), yaw: to.yaw, pitch: to.pitch, dist: to.dist };
      return;
    }
    var travel = len(sub(to.target, this.target));
    this.fly = { t: 0, dur: dur || 1.1, from: { target: this.target.slice(), yaw: this.yaw, pitch: this.pitch, dist: this.dist }, to: to, lift: Math.min(10, travel * 0.25) };
  };
  // matrices builds view and projection. shift is a horizontal lens shift in
  // NDC units: a panel covering the right of the canvas sets it so the target
  // sits centred in the part that is still visible (picking inverts the same
  // matrix, so it stays exact).
  Camera.prototype.matrices = function () {
    var eye = this.eye(), view = lookAt(eye, this.target, [0, 1, 0]);
    var proj = perspective(this.fov, this.aspect, 0.5, 600);
    this.shiftNow = mix(this.shiftNow || 0, this.shift || 0, 0.12);
    proj[8] = this.shiftNow;
    return { eye: eye, view: view, proj: proj, vp: mul(proj, view) };
  };

  // ray returns the world-space ray through a canvas point (CSS px).
  function ray(m, x, y, w, h) {
    var inv = invert(m.vp), nx = x / w * 2 - 1, ny = 1 - y / h * 2;
    var a = xform(inv, [nx, ny, -1]), b = xform(inv, [nx, ny, 1]);
    return { o: a, d: norm(sub(b, a)) };
  }

  function hitSphere(r, c, rad) {
    var oc = sub(r.o, c), b = dot(oc, r.d), cc = dot(oc, oc) - rad * rad, disc = b * b - cc;
    if (disc < 0) return -1;
    var t = -b - Math.sqrt(disc);
    return t > 0 ? t : -1;
  }

  // project maps a world point to CSS px; z > 1 means behind the camera.
  function project(m, p, w, h) {
    var x = p[0], y = p[1], z = p[2], v = m.vp;
    var cx = v[0] * x + v[4] * y + v[8] * z + v[12], cy = v[1] * x + v[5] * y + v[9] * z + v[13];
    var cz = v[2] * x + v[6] * y + v[10] * z + v[14], cw = v[3] * x + v[7] * y + v[11] * z + v[15];
    if (cw <= 0) return null;
    return { x: (cx / cw * 0.5 + 0.5) * w, y: (1 - (cy / cw * 0.5 + 0.5)) * h, z: cz / cw, w: cw };
  }

  root.Orrery = {
    add: add, sub: sub, mulS: mulS, dot: dot, cross: cross, len: len, norm: norm, mix: mix, mix3: mix3, clamp: clamp,
    smooth: smooth, easeInOut: easeInOut,
    ident: ident, mul: mul, trs: trs, invert: invert, normalMat: normalMat, xform: xform, perspective: perspective, lookAt: lookAt,
    lathe: lathe, cylinder: cylinder, sphere: sphere, torus: torus, rbox: rbox, crystal: crystal, prism: prism, merge: merge,
    ribbon: ribbon, curve: curve, arcPts: arcPts,
    color: color, rgbMix: rgbMix, luma: luma,
    Renderer: Renderer, Camera: Camera, ray: ray, hitSphere: hitSphere, project: project
  };
})(window);

/* Seamless console -- the System screen's scene: the daemon drawn as a base
   on one deck, every part a structure on its own hex pad. Three rings, read
   from the rim inward: the doors where agents and the owner come in, the
   work the daemon does, and the core that keeps it. Outside the deck, the
   services it calls out to.

   Motion is evidence, never decoration: a structure moves only while it is
   working, and light runs along a trace only while data does. With nothing
   happening the scene is still and draws no frames at all. Every structure
   is an original design built from the engine's primitives.

   The scene owns presentation only. Which part is running, idle, degraded
   or off, and how busy it is, arrives as data (setData) and as pulses
   (pulse) from whoever mounts it; the scene never invents a state. */
(function (root) {
  'use strict';
  var O = root.Orrery;
  var TAU = Math.PI * 2;
  var DEG = Math.PI / 180;

  /* ---- layout --------------------------------------------------------- */

  var R_CORE = 7.6, R_EVENTS = 11.2, R_DAIS = 12.6, R_WORK = 22.0, R_DOORS = 31.0, R_PLATE = 35.0;
  var PAD = 0.36; // the top of a hex pad, where a structure stands

  // Every part: id, ring, angle (deg, 0 = facing the default camera), radius,
  // model recipe, hue token, scale. Labels are the owner-facing names.
  var PARTS = [
    { id: 'mcp', ring: 'doors', a: 0, model: 'comm', hue: 'pop', label: 'MCP' },
    { id: 'hooks', ring: 'doors', a: 60, model: 'gantry', hue: 'pop', label: 'Hooks' },
    { id: 'socket', ring: 'doors', a: 120, model: 'airlock', hue: 'pop', label: 'Socket' },
    { id: 'listener', ring: 'doors', a: 180, model: 'gate', hue: 'pop', label: 'Listener' },
    { id: 'console', ring: 'doors', a: 240, model: 'tower', hue: 'pop', label: 'Console' },
    { id: 'a2a', ring: 'doors', a: 300, model: 'relay', hue: 'pop', label: 'A2A' },

    { id: 'retrieval', ring: 'work', a: 0, model: 'refractor', hue: 'brand', label: 'Retrieval' },
    { id: 'tasks', ring: 'work', a: 30, model: 'yard', hue: 'violet', label: 'Tasks & plans' },
    { id: 'sessions', ring: 'work', a: 60, model: 'hangar', hue: 'violet', label: 'Sessions' },
    { id: 'labs', ring: 'work', a: 90, model: 'lab', hue: 'violet', label: 'Labs' },
    { id: 'approvals', ring: 'work', a: 120, model: 'lock', hue: 'brand', label: 'Approvals' },
    { id: 'screening', ring: 'work', a: 150, model: 'scanner', hue: 'brand', label: 'Write screening' },
    { id: 'updates', ring: 'work', a: 180, model: 'radar', hue: 'violet', label: 'Updates' },
    { id: 'commits', ring: 'work', a: 210, model: 'ledger', hue: 'violet', label: 'Commit records' },
    { id: 'feed', ring: 'work', a: 240, model: 'mast', hue: 'violet', label: 'Metadata feed' },
    { id: 'capture', ring: 'work', a: 270, model: 'collector', hue: 'violet', label: 'URL capture' },
    { id: 'gardener', ring: 'work', a: 300, model: 'greenhouse', hue: 'brand', label: 'Gardener' },
    { id: 'lifecycle', ring: 'work', a: 330, model: 'recycler', hue: 'brand', label: 'Lifecycle' },

    { id: 'database', ring: 'core', a: 0, r: 0, model: 'silos', hue: 'flow', label: 'Database', s: 1.7 },
    { id: 'watcher', ring: 'core', a: 0, model: 'sentry', hue: 'flow', label: 'File watcher' },
    { id: 'notes', ring: 'core', a: 52, model: 'racks', hue: 'flow', label: 'Note files' },
    { id: 'embeddings', ring: 'core', a: 115, model: 'vglobe', hue: 'flow', label: 'Embeddings' },
    { id: 'index', ring: 'core', a: 170, model: 'grid', hue: 'flow', label: 'Search index' },
    { id: 'llm', ring: 'core', a: 222, model: 'orb', hue: 'flow', label: 'Chat model' },
    { id: 'backups', ring: 'core', a: 270, model: 'vault', hue: 'flow', label: 'Backups' },
    { id: 'memories', ring: 'core', a: 314, model: 'crates', hue: 'flow', label: 'Memory files' },
    { id: 'events', ring: 'core', a: 196, r: R_EVENTS, model: 'eventring', hue: 'flow', label: 'Event log', noPad: true, s: 1 },

    { id: 'provider', ring: 'sky', a: 146, r: 43, y: 5.0, model: 'planet', hue: 'match', label: 'LLM provider', noPad: true, s: 1.9 },
    { id: 'github', ring: 'sky', a: 210, r: 43, y: 5.5, model: 'satellite', hue: 'match', label: 'GitHub releases', noPad: true, s: 1.5 }
  ];

  // Ring scale: structures grow with their ring so they read at the same size.
  var RING_SCALE = { doors: 1.4, work: 1.3, core: 1.15, sky: 1 };

  // Traces: what talks to what. Drones travel along them.
  var LINKS = [
    ['mcp', 'retrieval'], ['hooks', 'sessions'], ['hooks', 'retrieval'], ['socket', 'approvals'], ['listener', 'updates'],
    ['console', 'feed'], ['a2a', 'gardener'], ['mcp', 'tasks'],
    ['retrieval', 'database'], ['retrieval', 'embeddings'], ['sessions', 'database'], ['tasks', 'database'], ['labs', 'database'],
    ['approvals', 'database'], ['screening', 'memories'], ['updates', 'backups'], ['commits', 'database'], ['feed', 'events'],
    ['capture', 'notes'], ['gardener', 'memories'], ['lifecycle', 'memories'],
    ['memories', 'watcher'], ['notes', 'watcher'], ['watcher', 'database'], ['database', 'index'], ['database', 'events'],
    ['embeddings', 'database'], ['llm', 'database']
  ];
  var BEAMS = [['embeddings', 'provider'], ['llm', 'provider'], ['updates', 'github']];

  // Engraved ring captions, for the page to place (name, radius).
  var CAPTIONS = [['DOORS', R_PLATE - 1.7], ['WORK', (R_WORK + R_DOORS) / 2 - 0.4], ['CORE', R_DAIS + 1.0]];

  function ringRadius(p) {
    if (p.r != null) return p.r;
    return p.ring === 'doors' ? R_DOORS : p.ring === 'work' ? R_WORK : p.ring === 'core' ? R_CORE : 40;
  }

  function place(p) {
    if (p.model === 'eventring') return [0, 0.32, 0];
    var r = ringRadius(p), a = p.a * DEG;
    var base = p.ring === 'core' ? 0.32 : 0;
    return [Math.sin(a) * r, (p.y || 0) + base, Math.cos(a) * r];
  }

  // plateY is the height of the deck at (x, z): the core stands on a raised
  // dais, so a trace climbs onto it at the edge.
  function plateY(x, z) {
    var r = Math.sqrt(x * x + z * z);
    if (r <= R_DAIS - 0.3) return 0.345;
    if (r >= R_DAIS + 0.3) return 0.03;
    return O.mix(0.345, 0.03, O.smooth((r - R_DAIS + 0.3) / 0.6));
  }

  /* ---- palette from the console's tokens ------------------------------ */

  function tokens(el) {
    var cs = getComputedStyle(el || document.documentElement);
    function t(name, fb) { var v = cs.getPropertyValue(name).trim(); return O.color(v || fb); }
    var paper = t('--paper', '#07080e'), dark = O.luma(paper) < 0.5;
    return {
      dark: dark,
      void: t('--void', '#04050a'), paper: paper,
      surface: t('--surface-solid', '#0d1018'), s2: t('--surface-2', '#131725'), s3: t('--surface-3', '#1a1f30'),
      border: t('--border-strong', 'rgba(150,160,255,.19)'),
      ink: t('--ink', '#eceefb'), ink2: t('--ink-2', '#c1c6de'), muted: t('--muted', '#9097b3'),
      brand: t('--brand', '#8e96ff'), violet: t('--violet', '#b99bff'), pop: t('--pop', '#ff8a66'), flow: t('--flow', '#5fe3f6'),
      live: t('--live', '#4fdc9e'), warn: t('--warn', '#f7c065'), danger: t('--danger', '#ff7486'), match: t('--match', '#ffd166'),
      kinds: ['--k-constraint', '--k-convention', '--k-gotcha', '--k-decision', '--k-reference', '--k-runbook', '--k-protocol', '--k-stage']
        .map(function (k) { return t(k, '#8e96ff'); })
    };
  }

  /* ---- model recipes -------------------------------------------------- */
  // A recipe returns {parts: [{g, mat, m(t, st) -> local matrix}], h: label
  // height, rad: pick radius, fx(t, st, emit)}. Local y=0 is the deck; a
  // structure stands on its pad at y=PAD. Materials name roles (body, metal,
  // metalFlat, accent, lit, glass, k0..k7); the palette fills them per theme.

  function part(g, mat, m, extra) {
    var o = { g: g, mat: mat, m: m || null };
    if (extra) for (var k in extra) o[k] = extra[k];
    return o;
  }
  function still(t) { return function () { return t; }; }
  function at(x, y, z, ry, s, rx, rz) { return still(O.trs([x, y + PAD, z], ry || 0, s == null ? 1 : s, rx || 0, rz || 0)); }

  var G = {}; // shared geometries, built once
  function shared() {
    if (G.ready) return G;
    // the hex pad: a six-sided lathe, flat-shaded
    G.pad = O.lathe([[0, 0], [1.52, 0], [1.62, 0.08], [1.62, 0.26], [1.52, 0.33], [1.3, 0.36], [0, 0.36]], 6, 30);
    G.padRing = O.torus(1.56, 0.05, 6, 8);
    G.ripple = O.torus(1, 0.028, 72, 8);
    G.sphere = O.sphere(1, 28);
    G.dome = O.lathe((function () { var p = []; for (var k = 0; k <= 12; k++) { var a = k / 12 * Math.PI / 2; p.push([Math.cos(a), Math.sin(a)]); } p[12][0] = 0; return p; })(), 40, 89);
    G.crate = O.rbox(0.62, 0.42, 0.52, 0.05);
    G.stripe = O.rbox(0.64, 0.07, 0.54, 0.02);
    G.ready = true;
    return G;
  }

  var RECIPES = {
    // MCP: a lattice comm tower with a dish facing out toward incoming agents
    comm: function () {
      var dish = O.lathe([[0.06, -0.02], [0.45, 0.03], [0.85, 0.16], [1.1, 0.36], [1.04, 0.38], [0.8, 0.2], [0.42, 0.08], [0, 0.04]], 48, 70);
      var tower = O.lathe([[0.66, 0], [0.36, 2.2], [0, 2.2]], 4, 30);
      var ps = [part(tower, 'metalFlat', at(0, 0, 0, Math.PI / 4))];
      [0.55, 1.1, 1.65].forEach(function (y, i) {
        ps.push(part(O.torus(0.6 - i * 0.075, 0.035, 4, 6), 'accent', at(0, y, 0, Math.PI / 4)));
      });
      ps.push(part(O.cylinder(0.3, 0.14, 24, 0.04), 'metal', at(0, 2.2, 0)));
      ps.push(part(dish, 'body', function (t, st) { return O.trs([0, PAD + 2.45, 0], Math.sin(st.ph * 0.9) * 0.35, 1, -0.7); }));
      ps.push(part(O.cylinder(0.05, 0.8, 10, 0), 'accent', function (t, st) { return O.trs([0, PAD + 2.45, 0], Math.sin(st.ph * 0.9) * 0.35, 1, -0.7); }));
      return {
        h: 3.9, rad: 1.8, parts: ps, rippleAt: [0, PAD + 2.8, 0], rippleTilt: -0.7,
        fx: function (t, st, emit) {
          emit.glow([0, PAD + 3.25, 0], 0.55, 'danger', st.on * 0.35 + st.act * 0.6);
          emit.glow([0, PAD + 2.9, 0.35], 0.9 + st.act * 1.4, 'accent', 0.15 * st.on + st.act * 0.7);
        }
      };
    },
    // Hooks: a gantry crane; its trolley carries a crate across the beam
    gantry: function () {
      function tx(t, st) { return Math.sin(st.ph * 1.3) * 0.75; }
      return {
        h: 3.7, rad: 1.8,
        parts: [
          part(O.rbox(0.24, 2.8, 0.34, 0.06), 'metal', at(-1.2, 1.4, 0)),
          part(O.rbox(0.24, 2.8, 0.34, 0.06), 'metal', at(1.2, 1.4, 0)),
          part(O.rbox(2.85, 0.24, 0.38, 0.06), 'body', at(0, 2.86, 0)),
          part(O.rbox(2.7, 0.05, 0.4, 0.02), 'accent', at(0, 2.72, 0)),
          part(O.rbox(0.44, 0.2, 0.46, 0.06), 'metal', function (t, st) { return O.trs([tx(t, st), PAD + 2.62, 0], 0, 1); }),
          part(O.cylinder(0.025, 1.05, 8, 0), 'metal', function (t, st) { return O.trs([tx(t, st), PAD + 1.5, 0], 0, 1); }),
          part(G.crate, 'lit', function (t, st) { return O.trs([tx(t, st), PAD + 1.26, 0], 0, 1); })
        ],
        fx: function (t, st, emit) { if (st.act > 0.05) emit.glow([0, PAD + 1.3, 0], 0.9 * st.act + 0.3, 'accent', st.act); }
      };
    },
    // Socket: a bunker with an airlock hatch; the kernel checks who knocks
    airlock: function () {
      return {
        h: 2.4, rad: 1.7,
        parts: [
          part(O.rbox(2.4, 1.2, 1.8, 0.3), 'body', at(0, 0.6, 0)),
          part(O.rbox(1.6, 0.12, 1.1, 0.05), 'metal', at(0, 1.24, -0.1)),
          part(O.torus(0.5, 0.1, 40, 12), 'metal', at(0, 0.66, 0.9, 0, 1, Math.PI / 2)),
          part(O.cylinder(0.42, 0.08, 36, 0.02), 'lit', at(0, 0.66, 0.86, 0, 1, Math.PI / 2)),
          part(O.rbox(0.1, 0.34, 0.1, 0.03), 'accent', at(0, 0.62, 0.96))
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 0.66, 1.05], 0.9, 'accent', 0.25 * st.on + st.act * 0.6); }
      };
    },
    // Listener: the gate every request walks through
    gate: function () {
      return {
        h: 3.9, rad: 1.9,
        parts: [
          part(O.rbox(0.5, 2.7, 0.7, 0.12), 'body', at(-1.3, 1.35, 0)),
          part(O.rbox(0.5, 2.7, 0.7, 0.12), 'body', at(1.3, 1.35, 0)),
          part(O.torus(1.3, 0.22, 48, 14, Math.PI), 'metal', still(O.trs([0, PAD + 2.7, 0], 0, 1, -Math.PI / 2, 0))),
          part(O.rbox(2.05, 0.05, 0.06, 0.02), 'accent', function (t, st) { return O.trs([0, PAD + 0.25 + ((st.ph * 0.9) % 1) * 2.3, 0], 0, [st.act > 0.02 ? 1 : 0.0001, 1, 1]); })
        ],
        fx: function (t, st, emit) {
          emit.glow([-1.3, PAD + 2.8, 0.36], 0.45, 'accent', 0.45 * st.on);
          emit.glow([1.3, PAD + 2.8, 0.36], 0.45, 'accent', 0.45 * st.on);
          emit.glow([0, PAD + 1.4, 0], 1.2, 'accent', st.act * 0.6);
        }
      };
    },
    // Console: a control tower where the owner looks over the base
    tower: function () {
      var cabin = O.lathe([[0.3, 0], [0.95, 0.22], [1.04, 0.74], [0.64, 1.0], [0, 1.02]], 40, 40);
      return {
        h: 4.2, rad: 1.6,
        parts: [
          part(O.cylinder(0.32, 2.2, 24, 0.04), 'metal', at(0, 0, 0)),
          part(cabin, 'body', at(0, 2.2, 0)),
          part(O.torus(1.0, 0.06, 48, 8), 'lit', at(0, 2.66, 0)),
          part(O.cylinder(0.03, 0.75, 8, 0), 'metal', at(0, 3.2, 0))
        ],
        fx: function (t, st, emit) {
          emit.glow([0, PAD + 3.98, 0], 0.4, 'danger', st.on * 0.4);
          emit.glow([0, PAD + 2.66, 0], 1.6, 'accent', 0.15 * st.on + st.act * 0.5);
        }
      };
    },
    // A2A: a relay mast with two linked rings
    relay: function () {
      var ring = O.torus(0.55, 0.1, 48, 12);
      return {
        h: 3.6, rad: 1.5,
        parts: [
          part(O.cylinder(0.1, 1.6, 14, 0), 'metal', at(0, 0, 0)),
          part(O.cylinder(0.45, 0.2, 28, 0.06), 'metal', at(0, 0, 0)),
          part(ring, 'accent', function (t, st) { return O.trs([-0.3, PAD + 2.25, 0], st.ph * 1.6, 1, Math.PI / 2); }),
          part(ring, 'body', function (t, st) { return O.trs([0.3, PAD + 2.25, 0], -st.ph * 1.6, 1, 0, Math.PI / 2); })
        ]
      };
    },
    // Retrieval: a refractor that splits the light of a query into answers
    refractor: function () {
      return {
        h: 3.8, rad: 1.7,
        parts: [
          part(O.lathe([[0.85, 0], [0.75, 0.5], [0.42, 0.62], [0, 0.62]], 6, 30), 'metalFlat', at(0, 0, 0, Math.PI / 6)),
          part(O.prism(0.95, 1.8), 'glass', function (t, st) { return O.trs([0, PAD + 0.62, 0], st.ph * 0.7, 1); }),
          part(O.prism(0.48, 1.5), 'lit', function (t, st) { return O.trs([0, PAD + 0.77, 0], st.ph * 0.7, 1); }),
          part(O.rbox(0.05, 0.05, 2.2, 0.02), 'accent', at(0, 1.5, 1.7)),
          part(O.rbox(0.04, 0.04, 2.0, 0.02), 'k0', function (t, st) { return O.trs([0, PAD + 1.5, -1.3], 0.28, [1, 1, 0.4 + 0.6 * st.on]); }),
          part(O.rbox(0.04, 0.04, 2.0, 0.02), 'k2', function (t, st) { return O.trs([0, PAD + 1.5, -1.3], 0, [1, 1, 0.4 + 0.6 * st.on]); }),
          part(O.rbox(0.04, 0.04, 2.0, 0.02), 'k3', function (t, st) { return O.trs([0, PAD + 1.5, -1.3], -0.28, [1, 1, 0.4 + 0.6 * st.on]); })
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 1.6, 0], 1.0 + st.act, 'accent', 0.18 * st.on + st.act * 0.7); }
      };
    },
    // Tasks & plans: a cargo yard; claimed work is lifted and lit
    yard: function () {
      var ps = [part(O.rbox(2.5, 0.1, 2.0, 0.04), 'metal', at(0, 0.05, 0))];
      var slots = [[-0.75, -0.55, 0], [0, -0.55, 0], [0.75, -0.55, 0], [-0.75, 0.55, 0], [0.75, 0.55, 0], [-0.75, -0.55, 1], [0.75, 0.55, 1]];
      slots.forEach(function (s, i) {
        var hot = i === 1 || i === 6;
        ps.push(part(G.crate, hot ? 'k' + (i % 8) : 'body', function (t, st) {
          var lift = hot ? 0.08 * st.on + 0.3 * st.act : 0;
          return O.trs([s[0], PAD + 0.31 + s[2] * 0.44 + lift, s[1]], 0, 1);
        }));
      });
      ps.push(part(O.cylinder(0.08, 1.9, 12, 0), 'metal', at(0, 0.1, 0.55)));
      ps.push(part(O.rbox(1.6, 0.08, 0.12, 0.03), 'accent', function (t, st) { return O.trs([0, PAD + 1.95, 0.55], st.ph * 0.8, 1); }));
      return { h: 3.0, rad: 1.7, parts: ps };
    },
    // Sessions: a hangar; live agents circle above it
    hangar: function () {
      return {
        h: 3.4, rad: 1.7,
        parts: [
          part(G.dome, 'body', at(0, 0, 0, 0, [1.25, 0.95, 1.25])),
          part(O.rbox(0.9, 0.62, 0.4, 0.08), 'lit', at(0, 0.31, 1.06)),
          part(O.torus(1.5, 0.03, 80, 8), 'metal', still(O.trs([0, PAD + 2.2, 0], 0, 1, 0.38, 0.18)))
        ],
        fx: function (t, st, emit) {
          var n = st.count == null ? 3 : Math.min(12, st.count);
          var m = O.trs([0, PAD + 2.2, 0], 0, 1, 0.38, 0.18);
          // one steady light per live agent; the door flashes when one starts or ends
          for (var k = 0; k < n; k++) {
            var a = k / Math.max(1, n) * TAU + st.ph * 0.5;
            emit.spark(O.xform(m, [Math.cos(a) * 1.5, 0, Math.sin(a) * 1.5]), 0.24, 'accent', 0.95);
          }
          emit.glow([0, PAD + 0.4, 1.3], 0.9, 'accent', 0.2 * st.on + st.act * 0.6);
        }
      };
    },
    // Labs: a bench with a flask bubbling while trials run
    lab: function () {
      var body = O.lathe([[0, 0], [0.5, 0.04], [0.78, 0.32], [0.8, 0.68], [0.55, 1.0], [0.22, 1.18], [0.2, 1.8], [0.26, 1.87], [0.18, 1.9]], 48, 60);
      var liquid = O.lathe([[0, 0.02], [0.46, 0.06], [0.7, 0.33], [0.72, 0.56], [0, 0.56]], 40, 50);
      return {
        h: 3.2, rad: 1.5,
        parts: [
          part(O.rbox(2.0, 0.5, 1.1, 0.08), 'metal', at(0, 0.25, 0)),
          part(liquid, 'lit', at(0, 0.52, 0)),
          part(body, 'glass', at(0, 0.5, 0)),
          part(O.rbox(0.5, 0.36, 0.5, 0.06), 'body', at(-0.65, 0.68, 0))
        ],
        fx: function (t, st, emit) {
          if (st.act < 0.02) return;
          for (var k = 0; k < 6; k++) {
            var ph = (st.ph * 0.6 + k / 6) % 1;
            emit.spark([Math.sin(k * 2.1) * 0.22 * (1 - ph), PAD + 0.85 + ph * 1.75, Math.cos(k * 1.3) * 0.18], 0.09 + (1 - ph) * 0.05, 'accent', (1 - ph) * 0.9 * Math.min(1, st.act * 3));
          }
        }
      };
    },
    // Approvals: a vault lock on a bunker; the shackle lifts while a lease lives
    lock: function () {
      return {
        h: 3.5, rad: 1.6,
        parts: [
          part(O.rbox(2.0, 0.55, 1.4, 0.12), 'metal', at(0, 0.27, 0)),
          part(O.rbox(1.35, 1.05, 0.62, 0.16), 'body', at(0, 1.08, 0)),
          part(O.cylinder(0.13, 0.08, 18, 0), 'accent', at(0, 1.18, 0.31, 0, 1, Math.PI / 2)),
          part(O.torus(0.42, 0.1, 36, 12, Math.PI), 'metal', function (t, st) { return O.trs([0, PAD + 2.08 + st.open * 0.35, 0], 0, 1, -Math.PI / 2, 0); }),
          part(O.cylinder(0.1, 0.5, 14, 0), 'metal', function (t, st) { return O.trs([-0.42, PAD + 1.58 + st.open * 0.35, 0], 0, 1); }),
          part(O.cylinder(0.1, 0.5, 14, 0), 'metal', at(0.42, 1.58, 0))
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 1.18, 0.45], 0.6, 'accent', 0.4 * st.on + st.act * 0.5); }
      };
    },
    // Write screening: a scanner gate; held writes wait in front of it
    scanner: function () {
      return {
        h: 3.6, rad: 1.8,
        parts: [
          part(O.rbox(0.32, 2.5, 0.34, 0.08), 'body', at(-1.05, 1.25, 0)),
          part(O.rbox(0.32, 2.5, 0.34, 0.08), 'body', at(1.05, 1.25, 0)),
          part(O.rbox(2.4, 0.2, 0.4, 0.06), 'metal', at(0, 2.55, 0)),
          part(O.rbox(1.78, 0.035, 0.18, 0.01), 'accent', function (t, st) { return O.trs([0, PAD + 0.35 + (0.5 + 0.5 * Math.sin(st.ph * 2.2)) * 1.9, 0], 0, 1); })
        ],
        fx: function (t, st, emit) {
          var n = Math.min(4, st.count || 0);
          for (var k = 0; k < n; k++) emit.spark([-0.6 + k * 0.4, PAD + 0.2, 1.1], 0.17, 'warn', 0.95);
        }
      };
    },
    // Updates: a radar sweeping for new releases
    radar: function () {
      var dish = O.lathe([[0, 0], [0.55, 0.06], [1.0, 0.32], [0.95, 0.35], [0.5, 0.13], [0, 0.07]], 40, 70);
      return {
        h: 3.6, rad: 1.6,
        parts: [
          part(O.lathe([[0.7, 0], [0.55, 0.45], [0, 0.45]], 8, 30), 'metalFlat', at(0, 0, 0)),
          part(O.cylinder(0.11, 1.3, 14, 0), 'metal', function (t, st) { return O.trs([0, PAD + 0.45, 0], st.ph * 1.6, 1); }),
          part(dish, 'body', function (t, st) { return O.mul(O.trs([0, PAD + 1.85, 0], st.ph * 1.6, 1), O.trs([0, 0, 0], 0, 1, -1.0)); }),
          part(G.sphere, 'accent', function (t, st) { return O.mul(O.trs([0, PAD + 1.85, 0], st.ph * 1.6, 1), O.trs([0, 0.42, 0.62], 0, 0.08)); })
        ]
      };
    },
    // Commit records: a ledger on a lectern
    ledger: function () {
      var page = O.rbox(0.95, 0.06, 1.25, 0.03);
      return {
        h: 3.0, rad: 1.5,
        parts: [
          part(O.cylinder(0.18, 1.0, 16, 0), 'metal', at(0, 0, 0)),
          part(O.rbox(2.2, 0.1, 1.5, 0.04), 'body', at(0, 1.05, 0, 0, 1, -0.32)),
          part(page, 'body', still(O.mul(O.trs([0, PAD + 1.12, 0], 0, 1, -0.32), O.trs([-0.48, 0.1, 0], 0, 1, 0, 0.18)))),
          part(page, 'body', still(O.mul(O.trs([0, PAD + 1.12, 0], 0, 1, -0.32), O.trs([0.48, 0.1, 0], 0, 1, 0, -0.18)))),
          part(page, 'lit', function (t, st) {
            var f = Math.sin(st.ph * 1.5) * 0.5 + 0.5;
            return O.mul(O.trs([0, PAD + 1.16, 0], 0, 1, -0.32), O.mul(O.trs([0, 0, 0], 0, 1, 0, -0.18 + f * 0.36), O.trs([0.46, 0.04, 0], 0, [0.98, 0.5, 0.96])));
          })
        ]
      };
    },
    // Metadata feed: a tall mast that blinks with each frame it sends
    mast: function () {
      return {
        h: 4.2, rad: 1.4,
        parts: [
          part(O.cylinder(0.5, 0.3, 24, 0.06), 'metal', at(0, 0, 0)),
          part(O.cylinder(0.09, 3.0, 14, 0), 'metal', at(0, 0.3, 0)),
          part(O.rbox(1.3, 0.06, 0.06, 0.02), 'metal', at(0, 2.0, 0)),
          part(O.rbox(0.9, 0.06, 0.06, 0.02), 'metal', at(0, 2.6, 0)),
          part(G.sphere, 'accent', at(0, 3.35, 0, 0, 0.14))
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 3.35, 0], 0.8, 'accent', st.on * 0.25 + st.act * 0.7); }
      };
    },
    // URL capture: a collector funnel catching pages
    collector: function () {
      var f = O.lathe([[0.14, 0], [0.16, 0.5], [0.32, 0.9], [1.05, 1.8], [1.0, 1.83], [0.27, 0.93], [0.1, 0.52], [0.08, 0]], 44, 60);
      return {
        h: 3.2, rad: 1.6,
        parts: [
          part(O.cylinder(0.6, 0.25, 24, 0.06), 'metal', at(0, 0, 0)),
          part(f, 'body', at(0, 0.25, 0)),
          part(O.rbox(0.5, 0.03, 0.66, 0.01), 'lit', function (t, st) { var ph = 1 - st.act; return O.trs([0, PAD + 3.1 - ph * 2.4, 0], st.ph * 1.2, st.act > 0.02 ? 1 - ph * 0.6 : 0.0001, 0.3, 0); })
        ]
      };
    },
    // Gardener: a greenhouse; each leaf is a proposal waiting for the owner
    greenhouse: function () {
      var leaf = O.sphere(1, 18), ps = [];
      ps.push(part(O.cylinder(1.25, 0.2, 40, 0.05), 'metal', at(0, 0, 0)));
      ps.push(part(O.lathe([[0, 0], [0.42, 0], [0.52, 0.45], [0.55, 0.5], [0, 0.5]], 32, 40), 'body', at(0, 0.2, 0)));
      ps.push(part(O.cylinder(0.05, 1.2, 10, 0), 'lit', function (t, st) { return O.trs([0, PAD + 0.7, 0], 0, 1, 0, Math.sin(st.ph * 2) * 0.06); }));
      for (var k = 0; k < 5; k++) {
        (function (k) {
          ps.push(part(leaf, 'accent', function (t, st) {
            var shown = st.count == null ? 3 : st.count;
            var s = k < shown ? 1 : 0.0001;
            var sway = Math.sin(st.ph * 2 + k) * 0.08;
            var a = k * 2.4, hh = 1.0 + k * 0.2;
            var base = O.trs([0, PAD + 0.7, 0], 0, 1, 0, Math.sin(st.ph * 2) * 0.06);
            return O.mul(base, O.trs([Math.cos(a) * 0.3, hh - 0.7, Math.sin(a) * 0.3], -a, [0.34 * s, 0.07 * s, 0.16 * s], 0, 0.5 + sway));
          }));
        })(k);
      }
      ps.push(part(G.dome, 'glass', at(0, 0.2, 0, 0, [1.18, 1.25, 1.18])));
      return { h: 3.2, rad: 1.6, parts: ps };
    },
    // Lifecycle: a recycler; retired memories run through it
    recycler: function () {
      var glass = O.lathe([[0, 0], [0.62, 0.02], [0.6, 0.35], [0.12, 0.95], [0.6, 1.55], [0.62, 1.88], [0, 1.9]], 40, 55);
      var sand = O.lathe([[0, 0], [0.5, 0.03], [0.32, 0.32], [0, 0.42]], 32, 50);
      return {
        h: 3.2, rad: 1.4,
        parts: [
          part(O.cylinder(0.78, 0.14, 6, 0.04), 'metalFlat', at(0, 0, 0)),
          part(O.cylinder(0.78, 0.14, 6, 0.04), 'metalFlat', at(0, 2.04, 0)),
          part(sand, 'accent', function (t, st) { var f = 0.6 + 0.4 * ((st.ph * 0.08) % 1); return O.trs([0, PAD + 0.16, 0], 0, [1, f, 1]); }),
          part(glass, 'glass', at(0, 0.14, 0))
        ],
        fx: function (t, st, emit) {
          if (st.act < 0.02) return;
          var ph = (st.ph * 1.3) % 1;
          emit.spark([0, PAD + 1.1 - ph * 0.5, 0], 0.05, 'accent', 0.9);
        }
      };
    },
    // Database: three storage silos joined by a walkway; bands light on writes
    silos: function () {
      var silo = O.lathe([[0, 0], [0.56, 0], [0.56, 2.15], [0.46, 2.45], [0.2, 2.58], [0, 2.6]], 40, 40), ps = [];
      [0, 1, 2].forEach(function (k) {
        var a = k / 3 * TAU + Math.PI / 6, x = Math.sin(a) * 0.78, z = Math.cos(a) * 0.78;
        ps.push(part(silo, 'body', at(x, 0, z)));
        [0.55, 1.15, 1.75].forEach(function (y, j) { ps.push(part(O.torus(0.565, 0.035, 48, 8), 'accent', at(x, y, z), { band: j })); });
      });
      ps.push(part(O.cylinder(0.32, 2.0, 24, 0.04), 'metal', at(0, 0, 0)));
      ps.push(part(O.torus(1.12, 0.05, 64, 8), 'metal', at(0, 2.2, 0)));
      ps.push(part(O.cylinder(0.36, 0.12, 24, 0.04), 'lit', at(0, 2.0, 0)));
      return {
        h: 3.5, rad: 1.6, parts: ps,
        fx: function (t, st, emit) {
          var ph = (st.ph * 0.5) % 1;
          if (st.act > 0.02) emit.glow([0, PAD + 0.4 + ph * 2.0, 0], 1.8, 'accent', 0.35 * st.act * Math.sin(ph * Math.PI));
          emit.glow([0, PAD + 2.3, 0], 1.2 + st.act, 'accent', 0.18 * st.on + st.act * 0.6);
        }
      };
    },
    // Memory files: crates on a pallet, striped by kind; a write drops one in
    crates: function () {
      var ps = [part(O.rbox(1.9, 0.12, 1.5, 0.04), 'metal', at(0, 0.06, 0))];
      var slots = [[-0.42, -0.34, 0], [0.42, -0.34, 0], [-0.42, 0.34, 0], [0.42, 0.34, 0], [-0.42, -0.34, 1], [0.42, 0.34, 1], [0, 0, 2]];
      slots.forEach(function (s, i) {
        var drops = i === 6;
        ps.push(part(G.crate, 'body', function (t, st) {
          var drop = drops && st.dropT < 1 ? (1 - O.smooth(st.dropT)) * 2.6 : 0;
          return O.trs([s[0], PAD + 0.33 + s[2] * 0.43 + drop, s[1]], drops ? 0.3 : 0, 1);
        }));
        ps.push(part(G.stripe, 'k' + (i % 8), function (t, st) {
          var drop = drops && st.dropT < 1 ? (1 - O.smooth(st.dropT)) * 2.6 : 0;
          return O.trs([s[0], PAD + 0.33 + s[2] * 0.43 + drop, s[1]], drops ? 0.3 : 0, 1);
        }));
      });
      return { h: 2.6, rad: 1.3, parts: ps };
    },
    // Note files: a shelf of binders, one per long-form note; the newest lifts
    // when a note is written. Every piece has its own space: nothing overlaps.
    racks: function () {
      var H = [0.95, 1.06, 0.86, 1.0, 0.92], ps = [];
      ps.push(part(O.rbox(2.0, 0.12, 0.86, 0.04), 'metal', at(0, 0.06, 0)));
      ps.push(part(O.rbox(2.0, 1.25, 0.08, 0.03), 'metal', at(0, 0.745, -0.39)));
      ps.push(part(O.rbox(0.08, 0.92, 0.74, 0.03), 'metal', at(-0.96, 0.58, 0)));
      ps.push(part(O.rbox(0.08, 0.92, 0.74, 0.03), 'metal', at(0.96, 0.58, 0)));
      H.forEach(function (h, k) {
        var x = -0.68 + k * 0.34, newest = k === H.length - 1;
        function lift(st) { return newest ? st.act * 0.32 : 0; }
        ps.push(part(O.rbox(0.27, h, 0.64, 0.045), newest ? 'lit' : 'body', function (t, st) {
          return O.trs([x, PAD + 0.12 + h / 2 + lift(st), 0.02], 0, 1);
        }));
        ps.push(part(O.rbox(0.2, 0.09, 0.02, 0.008), 'k' + ((k * 3) % 8), function (t, st) {
          return O.trs([x, PAD + 0.12 + h * 0.72 + lift(st), 0.345], 0, 1);
        }));
      });
      return { h: 2.5, rad: 1.3, parts: ps };
    },
    // Embeddings: a vector globe of points spun over an emitter
    vglobe: function () {
      var pts = [], n = 46, g = Math.PI * (3 - Math.sqrt(5));
      for (var k = 0; k < n; k++) { var y = 1 - (k / (n - 1)) * 2, rr = Math.sqrt(1 - y * y), th = g * k; pts.push([Math.cos(th) * rr, y, Math.sin(th) * rr]); }
      return {
        h: 3.6, rad: 1.5,
        parts: [
          part(O.lathe([[0.8, 0], [0.62, 0.42], [0.3, 0.5], [0, 0.5]], 40, 30), 'metal', at(0, 0, 0)),
          part(O.torus(0.42, 0.05, 40, 8), 'accent', at(0, 0.52, 0)),
          part(O.torus(1.05, 0.02, 80, 6), 'metal', function (t, st) { return O.trs([0, PAD + 2.0, 0], st.ph * 0.4, 1, 0.42); })
        ],
        fx: function (t, st, emit) {
          var spin = st.ph * 0.9, c = Math.cos(spin), s = Math.sin(spin);
          for (var k = 0; k < pts.length; k++) {
            var p = pts[k], x = p[0] * c - p[2] * s, z = p[0] * s + p[2] * c;
            var hot = st.act > 0.05 && ((k * 7 + Math.floor(st.ph * 4)) % 11) === 0;
            emit.spark([x * 0.95, PAD + 2.0 + p[1] * 0.95, z * 0.95], hot ? 0.17 : 0.09, 'accent', (st.on > 0.5 ? 0.8 : 0.35) * (0.55 + 0.45 * (z + 1) / 2));
          }
          emit.glow([0, PAD + 2.0, 0], 1.5, 'accent', 0.14 * st.on + st.act * 0.6);
          emit.glow([0, PAD + 0.6, 0], 0.7, 'accent', 0.35 * st.on);
        }
      };
    },
    // Search index: a grid of keys that ripples as items are indexed
    grid: function () {
      var cube = O.rbox(0.26, 0.26, 0.26, 0.05), ps = [part(O.rbox(1.8, 0.12, 1.8, 0.04), 'metal', at(0, 0.06, 0))];
      for (var x = 0; x < 4; x++) {
        for (var z = 0; z < 4; z++) {
          (function (x, z) {
            var lit = (x * 7 + z * 3) % 5 === 0;
            ps.push(part(cube, lit ? 'accent' : 'body', function (t, st) {
              var wave = Math.sin(st.ph * 3 - (x + z) * 0.7) * 0.5 + 0.5;
              return O.trs([(x - 1.5) * 0.4, PAD + 0.3 + st.act * wave * 0.5, (z - 1.5) * 0.4], 0, 1);
            }));
          })(x, z);
        }
      }
      return { h: 2.3, rad: 1.2, parts: ps };
    },
    // Chat model: an orb in a cradle
    orb: function () {
      return {
        h: 3.0, rad: 1.3,
        parts: [
          part(O.cylinder(0.45, 0.3, 28, 0.06), 'metal', at(0, 0, 0)),
          part(O.torus(0.62, 0.05, 48, 8), 'metal', at(0, 1.1, 0, 0, 1, 0.5)),
          part(G.sphere, 'lit', function (t, st) { return O.trs([0, PAD + 1.1, 0], 0, 0.36 + 0.06 * st.act); }),
          part(G.sphere, 'glass', at(0, 1.1, 0, 0, 0.66))
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 1.1, 0], 1.2, 'accent', 0.16 * st.on + st.act * 0.6); }
      };
    },
    // File watcher: a sentry lens that turns toward the files
    sentry: function () {
      return {
        h: 3.0, rad: 1.2,
        parts: [
          part(O.cylinder(0.4, 0.2, 6, 0.04), 'metalFlat', at(0, 0, 0)),
          part(O.cylinder(0.08, 1.2, 12, 0), 'metal', at(0, 0.2, 0)),
          part(O.torus(0.48, 0.08, 40, 12), 'metal', function (t, st) { return O.trs([0, PAD + 1.7, 0], Math.sin(st.ph) * 0.9, 1, Math.PI / 2); }),
          part(O.cylinder(0.43, 0.05, 36, 0.01), 'glass', function (t, st) { return O.mul(O.trs([0, PAD + 1.7, 0], Math.sin(st.ph) * 0.9, 1, Math.PI / 2), O.trs([0, -0.025, 0], 0, 1)); })
        ],
        fx: function (t, st, emit) { emit.glow([0, PAD + 1.7, 0], 0.6, 'accent', 0.25 * st.on + st.act * 0.6); }
      };
    },
    // Backups: a vault whose dial turns when an archive is taken
    vault: function () {
      return {
        h: 2.6, rad: 1.3,
        parts: [
          part(O.rbox(1.5, 1.3, 1.2, 0.12), 'body', at(0, 0.65, 0)),
          part(O.cylinder(0.34, 0.1, 32, 0.03), 'metal', function (t, st) { return O.mul(O.trs([0.12, PAD + 0.7, 0.6], 0, 1, Math.PI / 2), O.trs([0, 0, 0], st.dial, 1)); }),
          part(O.rbox(0.08, 0.5, 0.08, 0.03), 'accent', at(-0.5, 0.7, 0.62))
        ]
      };
    },
    // Event log: the ring of light around the core; each event runs along it
    eventring: function () {
      return {
        h: 0.9, rad: 0.9, ringPick: R_EVENTS,
        parts: [part(O.torus(R_EVENTS, 0.04, 260, 10), 'lit', still(O.trs([0, 0.06, 0], 0, 1)))],
        fx: function (t, st, emit) {
          // each recorded event sends light once round the ring
          if (st.act < 0.02) return;
          for (var k = 0; k < 6; k++) {
            var a = st.ph * 1.4 + k / 6 * TAU;
            emit.spark([Math.sin(a) * R_EVENTS, 0.12, Math.cos(a) * R_EVENTS], 0.26, 'accent', Math.min(1, st.act * 1.6));
          }
        }
      };
    },
    // LLM provider: a planet off the deck, with a moon or three
    planet: function () {
      return {
        h: 2.6, rad: 2.6,
        parts: [
          part(G.sphere, 'lit', function (t, st) { return O.trs([0, 0, 0], st.ph * 0.1, 1.5); }),
          part(O.torus(2.4, 0.03, 120, 8), 'accent', still(O.trs([0, 0, 0], 0, 1, 0.42, 0.25))),
          part(O.torus(2.9, 0.012, 120, 6), 'accent', still(O.trs([0, 0, 0], 0, 1, 0.42, 0.25)))
        ],
        fx: function (t, st, emit) {
          emit.glow([0, 0, 0], 5.0, 'accent', 0.1 * st.on + st.act * 0.3 + 0.04);
          for (var k = 0; k < 3; k++) {
            var a = st.ph * (0.5 + k * 0.2) + k * 2.1;
            emit.spark([Math.cos(a) * (2.4 + k * 0.25), Math.sin(a) * 0.9, Math.sin(a) * (2.4 + k * 0.25)], 0.14, 'accent', 0.8 * st.on);
          }
        }
      };
    },
    // GitHub releases: a relay satellite the update check calls
    satellite: function () {
      return {
        h: 2.2, rad: 2.0,
        parts: [
          part(O.rbox(0.7, 0.7, 1.0, 0.1), 'body', function (t, st) { return O.trs([0, 0, 0], st.ph * 0.4, 1, 0.2); }),
          part(O.rbox(1.7, 0.04, 0.7, 0.02), 'accent', function (t, st) { return O.mul(O.trs([0, 0, 0], st.ph * 0.4, 1, 0.2), O.trs([1.35, 0, 0], 0, 1)); }),
          part(O.rbox(1.7, 0.04, 0.7, 0.02), 'accent', function (t, st) { return O.mul(O.trs([0, 0, 0], st.ph * 0.4, 1, 0.2), O.trs([-1.35, 0, 0], 0, 1)); }),
          part(O.cylinder(0.08, 0.6, 10, 0), 'metal', function (t, st) { return O.mul(O.trs([0, 0, 0], st.ph * 0.4, 1, 0.2), O.trs([0, 0.35, 0], 0, 1)); })
        ],
        fx: function (t, st, emit) { emit.glow([0, 0, 0], 2.6, 'accent', 0.1 * st.on + st.act * 0.3 + 0.03); }
      };
    }
  };

  /* ---- the scene -------------------------------------------------------- */

  function Scene(canvas, overlay, opts) {
    opts = opts || {};
    this.canvas = canvas;
    this.overlay = overlay;
    this.r = new O.Renderer(canvas);
    this.cam = new O.Camera();
    this.calm = !!opts.calm;
    this.onSelect = opts.onSelect || function () {};
    this.onHover = opts.onHover || function () {};
    this.labelText = opts.labels || {};
    this.sel = null; this.hover = null;
    this.t = 0; this.last = 0;
    this.effects = [];
    this.pkts = [];
    this.spr = new Float32Array(8 * 4096); this.sprN = 0;
    shared();
    this.pal = tokens();
    this._build();
    this._labels();
    this.home(true);
  }

  Scene.prototype._build = function () {
    var r = this.r, self = this;
    this.mPlate = r.mesh(O.lathe([[0, 0], [R_PLATE, 0], [R_PLATE, 0.001]], 200, 89));
    this.mRim = r.mesh(O.lathe([[R_PLATE - 0.05, -1.1], [R_PLATE + 0.3, -0.7], [R_PLATE + 0.36, -0.02], [R_PLATE, 0.0]], 220, 40));
    this.mUnder = r.mesh(O.lathe([[0, -2.0], [R_PLATE * 0.6, -1.85], [R_PLATE - 0.05, -1.1]], 140, 60));
    this.mDais = r.mesh(O.lathe([[0, 0], [R_DAIS, 0], [R_DAIS, 0.18], [R_DAIS - 0.2, 0.32], [0, 0.32]], 200, 40));
    this.mPad = r.mesh(G.pad);
    this.mPadRing = r.mesh(G.padRing);
    this.mRipple = r.mesh(G.ripple);
    this.mEventRipple = r.mesh(O.torus(R_EVENTS, 0.05, 200, 8));
    this.parts = PARTS.map(function (p) {
      var recipe = RECIPES[p.model]();
      var node = {
        p: p, id: p.id, pos: place(p), s: p.s || RING_SCALE[p.ring] || 1, recipe: recipe,
        meshes: recipe.parts.map(function (pp) { return { mesh: r.mesh(pp.g), mat: pp.mat, m: pp.m, band: pp.band }; }),
        st: { on: 0, act: 0, ph: 0, rate: 0, count: null, open: 0, dial: 0, dropT: 9, state: 'off' },
        want: 'off', label: null
      };
      node.anchor = O.add(node.pos, [0, recipe.h * node.s, 0]);
      return node;
    });
    this.byId = {};
    this.parts.forEach(function (n) { self.byId[n.id] = n; });
    // traces on the deck, from pad edge to pad edge; a trace to the event
    // ring ends on the ring, facing the part it comes from
    function spot(n, other) {
      if (n.p.model !== 'eventring') return { p: [n.pos[0], n.pos[2]], r: n.p.noPad ? 0.3 : 1.7 * n.s };
      var ox = other.pos[0], oz = other.pos[2];
      var a = (ox * ox + oz * oz) < 0.01 ? n.p.a * DEG : Math.atan2(ox, oz);
      return { p: [Math.sin(a) * R_EVENTS, Math.cos(a) * R_EVENTS], r: 0.15 };
    }
    this.links = LINKS.map(function (l) {
      var a = self.byId[l[0]], b = self.byId[l[1]];
      var sa = spot(a, b), sb = spot(b, a), pa = sa.p, pb = sb.p;
      var d = O.len([pb[0] - pa[0], 0, pb[1] - pa[1]]) || 1;
      var u = [(pb[0] - pa[0]) / d, (pb[1] - pa[1]) / d];
      var s = [pa[0] + u[0] * sa.r, pa[1] + u[1] * sa.r], e = [pb[0] - u[0] * sb.r, pb[1] - u[1] * sb.r];
      var bend = ((l[0].length + l[1].length) % 3 - 1) * 0.12;
      var pts = O.curve(s, e, bend, 36);
      var g = O.ribbon(pts, 0.2, plateY);
      return { a: a, b: b, pts: pts, mesh: r.mesh(g), len: g.length };
    });
    this.beams = BEAMS.map(function (l) { return { a: self.byId[l[0]], b: self.byId[l[1]] }; });
  };

  Scene.prototype._labels = function () {
    var self = this, ov = this.overlay;
    if (!ov) return;
    this.parts.forEach(function (n) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'orr-label';
      b.setAttribute('data-part', n.id);
      b.setAttribute('data-ring', n.p.ring);
      b.innerHTML = '<span class="orr-dot" aria-hidden="true"></span><span class="orr-name"></span>';
      b.querySelector('.orr-name').textContent = self.labelText[n.id] || n.p.label;
      b.addEventListener('click', function (e) { e.stopPropagation(); self.select(n.id, true); });
      b.addEventListener('mouseenter', function () { self.hover = n.id; self.onHover(n.id); });
      b.addEventListener('mouseleave', function () { if (self.hover === n.id) { self.hover = null; self.onHover(null); } });
      b.addEventListener('focus', function () { self.hover = n.id; });
      ov.appendChild(b);
      n.label = b;
    });
  };

  // setData takes {id: {state, rate, count, open}} for any subset of parts.
  Scene.prototype.setData = function (map) {
    var self = this;
    Object.keys(map || {}).forEach(function (id) {
      var n = self.byId[id], d = map[id];
      if (!n || !d) return;
      n.want = d.state || 'off';
      n.st.state = n.want;
      if (d.rate != null) n.st.rate = O.clamp(d.rate, 0, 1);
      if (d.count != null) n.st.count = d.count;
      if (d.open != null) n.st.openWant = d.open ? 1 : 0;
      if (n.label) {
        n.label.setAttribute('data-state', n.want);
        n.label.setAttribute('aria-label', n.p.label + ': ' + stateWords(n.want));
      }
    });
    this.dirty = true;
  };

  function stateWords(s) {
    return s === 'on' ? 'running' : s === 'idle' ? 'running, quiet' : s === 'warn' ? 'needs attention' : s === 'absent' ? 'not in this build' : 'off';
  }
  function running(n) { return n.want === 'on' || n.want === 'idle' || n.want === 'warn'; }

  // pulse marks work at a part: it moves while the work lasts, a ripple marks
  // the moment, and light runs along its traces (to `to` alone when given),
  // the way the data went.
  Scene.prototype.pulse = function (id, opts) {
    var n = this.byId[id];
    if (!n || !running(n)) return;
    opts = opts || {};
    n.st.act = Math.min(1, n.st.act + (opts.strength || 0.7));
    if (n.p.model === 'crates') n.st.dropT = 0;
    if (n.p.model === 'vault') n.st.dialGoal = (n.st.dialGoal || 0) + 2.4;
    if (!this.calm) {
      this.effects.push({ n: n, t0: this.t, dur: 1.2 });
      var self = this;
      this.links.forEach(function (l) {
        if (opts.to && l.a.id !== opts.to && l.b.id !== opts.to) return;
        if ((l.a === n || l.b === n) && self.pkts.length < 48) self.pkts.push({ l: l, t0: self.t, dur: 0.5 + l.len * 0.045, rev: l.b === n, hue: n.p.hue });
      });
    }
    this.dirty = true;
  };

  // home frames the whole deck; instant places the camera there without a
  // flight (the first frame, and calm mode).
  Scene.prototype.home = function (instant) {
    var w = this.canvas.clientWidth || 800, h = this.canvas.clientHeight || 600;
    var dist = O.clamp(41 / (Math.tan(this.cam.fov / 2) * (w / Math.max(1, h))), 84, 150);
    this.cam.flyTo({ target: [0, -2.2, 2.6], yaw: -0.38, pitch: 0.6, dist: dist }, 1.3, this.calm || instant);
    this.dirty = true;
  };

  Scene.prototype.select = function (id, fly) {
    var n = id ? this.byId[id] : null;
    this.sel = n ? n.id : null;
    this.parts.forEach(function (p) { if (p.label) p.label.classList.toggle('is-sel', p === n); });
    if (n && fly) {
      var yaw = n.p.ring === 'sky' || n.pos[0] * n.pos[0] + n.pos[2] * n.pos[2] < 0.01 ? this.cam.yaw : Math.atan2(n.pos[0], n.pos[2]);
      var dist = n.p.ring === 'sky' ? 26 : n.p.model === 'eventring' ? 34 : 14 + n.s * 4;
      var target = n.p.model === 'eventring' ? [0, 0.6, 0] : [n.pos[0], n.pos[1] + n.recipe.h * n.s * 0.42, n.pos[2]];
      this.cam.flyTo({ target: target, yaw: yaw + (n.p.ring === 'core' ? 0.35 : 0.22), pitch: n.p.ring === 'sky' ? 0.18 : 0.42, dist: dist }, 1.25, this.calm);
    }
    this.onSelect(this.sel);
    this.dirty = true;
  };

  // pick returns the id of the part under a canvas point (CSS px), or null.
  Scene.prototype.pick = function (x, y) {
    var m = this.cam.matrices(), w = this.canvas.clientWidth, h = this.canvas.clientHeight;
    var ry = O.ray(m, x, y, w, h), best = null, bt = Infinity;
    this.parts.forEach(function (n) {
      if (n.recipe.ringPick) {
        if (Math.abs(ry.d[1]) < 1e-4) return;
        var t = (0.45 - ry.o[1]) / ry.d[1];
        if (t <= 0) return;
        var px = ry.o[0] + ry.d[0] * t, pz = ry.o[2] + ry.d[2] * t, rr = Math.sqrt(px * px + pz * pz);
        if (Math.abs(rr - n.recipe.ringPick) < 0.6 && t < bt) { bt = t; best = n.id; }
        return;
      }
      var c = [n.pos[0], n.pos[1] + n.recipe.h * n.s * 0.4, n.pos[2]];
      var hit = O.hitSphere(ry, c, n.recipe.rad * n.s);
      if (hit > 0 && hit < bt) { bt = hit; best = n.id; }
    });
    return best;
  };

  Scene.prototype.theme = function () { this.pal = tokens(); this.dirty = true; };

  /* ---- frame ------------------------------------------------------------- */

  Scene.prototype.frame = function (now) {
    var dt = this.last ? Math.min(0.15, (now - this.last) / 1000) : 0.016;
    this.last = now;
    if (!this.calm) this.t += dt;
    var t = this.t, pal = this.pal, r = this.r, gl = r.gl, self = this;
    var moving = this.cam.step(dt, this.calm);
    this.moving = moving;

    this.parts.forEach(function (n) {
      var target = (n.want === 'on' || n.want === 'warn') ? 1 : n.want === 'idle' ? 0.55 : 0;
      n.st.on = self.calm ? target : O.mix(n.st.on, target, 1 - Math.pow(0.02, dt));
      if (n.st.act > 0) n.st.ph += dt * (0.6 + n.st.act * 2.2);
      n.st.act = Math.max(0, n.st.act - dt * 1.1);
      n.st.open = O.mix(n.st.open, n.st.openWant || 0, 1 - Math.pow(0.01, dt));
      if (n.st.dropT < 9) n.st.dropT += dt * 1.4;
      if (n.st.dialGoal != null) n.st.dial = O.mix(n.st.dial, n.st.dialGoal, 1 - Math.pow(0.05, dt));
    });

    var W = this.canvas.clientWidth, H = this.canvas.clientHeight;
    this.cam.aspect = W / Math.max(1, H);
    var m = this.cam.matrices();
    var bg = pal.dark ? pal.void : pal.paper;
    var dark = pal.dark ? 1 : 0;
    var fog = pal.dark ? O.rgbMix(pal.void, pal.paper, 0.5) : O.rgbMix(pal.paper, pal.void, 0.35);
    var FOG = [120, 300];

    gl.clearColor(bg[0], bg[1], bg[2], 1);
    gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);

    // sky
    var ps = r.progs.sky;
    gl.useProgram(ps.p);
    gl.disable(gl.DEPTH_TEST);
    var top = pal.dark ? O.rgbMix(pal.void, pal.brand, 0.05) : O.rgbMix(pal.paper, [1, 1, 1, 1], 0.4);
    var bot = pal.dark ? O.rgbMix(pal.paper, pal.s2, 0.6) : O.rgbMix(pal.paper, pal.void, 0.6);
    gl.uniform3fv(ps.u.uTop, top.slice(0, 3));
    gl.uniform3fv(ps.u.uBot, bot.slice(0, 3));
    gl.uniform3fv(ps.u.uA, O.mulS(pal.brand.slice(0, 3), pal.dark ? 0.12 : 0.05));
    gl.uniform3fv(ps.u.uB, O.mulS(pal.flow.slice(0, 3), pal.dark ? 0.07 : 0.035));
    gl.uniform2f(ps.u.uAsp, this.cam.aspect, 1);
    gl.uniform1f(ps.u.uStars, pal.dark ? 0.75 : 0.0);
    gl.uniform1f(ps.u.uTime, t);
    gl.uniform2f(ps.u.uLook, this.cam.yaw * 0.35, this.cam.pitch * 0.35);
    gl.bindVertexArray(r.quad.vao);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    gl.enable(gl.DEPTH_TEST);

    var pl = r.progs.lit;
    gl.useProgram(pl.p);
    gl.uniformMatrix4fv(pl.u.uVP, false, m.vp);
    gl.uniform3fv(pl.u.uCam, m.eye);
    gl.uniform3fv(pl.u.uFog, fog.slice(0, 3));
    gl.uniform2f(pl.u.uFogR, FOG[0], FOG[1]);
    gl.uniform3fv(pl.u.uSky, pal.dark ? O.rgbMix(pal.s3, pal.brand, 0.25).slice(0, 3) : [1, 1, 1]);
    gl.uniform3fv(pl.u.uGround, pal.dark ? pal.void.slice(0, 3) : O.rgbMix(pal.paper, pal.void, 0.6).slice(0, 3));
    gl.uniform3fv(pl.u.uRimC, pal.dark ? O.rgbMix(pal.brand, pal.ink, 0.3).slice(0, 3) : O.rgbMix(pal.brand, [1, 1, 1, 1], 0.4).slice(0, 3));
    gl.uniform1f(pl.u.uDark, dark);

    function mat(role, hue, grey, alpha) {
      var accent = pal[hue] || pal.brand, c, e = [0, 0, 0], emit = 0, gloss = 0.4, rim = pal.dark ? 0.35 : 0.15, flat = 0, a = alpha;
      var body = pal.dark ? O.rgbMix(pal.s3, pal.ink, 0.08) : O.rgbMix(pal.surface, pal.s2, 0.5);
      var metal = pal.dark ? O.rgbMix(pal.s3, pal.muted, 0.32) : O.rgbMix(pal.s3, pal.muted, 0.18);
      if (role === 'body') { c = body; gloss = 0.5; }
      else if (role === 'metal' || role === 'metalFlat') { c = metal; gloss = 0.75; flat = role === 'metalFlat' ? 1 : 0; }
      else if (role === 'accent') {
        c = pal.dark ? O.rgbMix(accent, pal.s3, 0.35) : O.rgbMix(accent, pal.surface, 0.12);
        e = accent; emit = pal.dark ? 0.75 : 0.25; gloss = 0.6;
      } else if (role === 'lit') {
        c = pal.dark ? O.rgbMix(accent, pal.s3, 0.5) : O.rgbMix(accent, pal.surface, 0.55);
        e = accent; emit = pal.dark ? 0.55 : 0.18; gloss = 0.3;
      } else if (role === 'glass') {
        c = pal.dark ? O.rgbMix(pal.s2, accent, 0.25) : O.rgbMix(pal.surface, accent, 0.18);
        gloss = 1; rim = pal.dark ? 0.9 : 0.5; a = Math.min(alpha, pal.dark ? 0.36 : 0.42);
      } else if (role.charAt(0) === 'k') {
        var kc = pal.kinds[+role.substr(1) % pal.kinds.length];
        c = pal.dark ? O.rgbMix(kc, pal.s3, 0.4) : O.rgbMix(kc, pal.surface, 0.25); e = kc; emit = pal.dark ? 0.5 : 0.15; gloss = 0.5;
      } else { c = body; }
      return { c: c, e: e, emit: emit, gloss: gloss, rim: rim, flat: flat, a: a, grey: grey };
    }

    function draw(mesh, model, M, opaquePass) {
      if (opaquePass !== (M.a >= 0.999)) return;
      gl.uniformMatrix4fv(pl.u.uModel, false, model);
      gl.uniformMatrix3fv(pl.u.uNM, false, O.normalMat(model));
      gl.uniform3fv(pl.u.uColor, M.c.slice(0, 3));
      gl.uniform3fv(pl.u.uEmis, M.e.slice(0, 3));
      gl.uniform1f(pl.u.uEmit, M.emit);
      gl.uniform1f(pl.u.uAlpha, M.a);
      gl.uniform1f(pl.u.uGrey, M.grey);
      gl.uniform1f(pl.u.uRim, M.rim);
      gl.uniform1f(pl.u.uFlat, M.flat);
      gl.uniform1f(pl.u.uGloss, M.gloss);
      gl.bindVertexArray(mesh.vao);
      gl.drawElements(gl.TRIANGLES, mesh.count, mesh.type, 0);
    }

    // the deck's rim and underside, the dais, then the engraved face
    var deckBody = { c: pal.dark ? O.rgbMix(pal.surface, pal.s3, 0.6) : O.rgbMix(pal.surface, pal.s3, 0.5), e: [0, 0, 0], emit: 0, gloss: 0.7, rim: pal.dark ? 0.6 : 0.2, flat: 0, a: 1, grey: 0 };
    gl.disable(gl.CULL_FACE);
    draw(this.mRim, O.ident(), deckBody, true);
    draw(this.mUnder, O.ident(), deckBody, true);
    gl.enable(gl.CULL_FACE);
    draw(this.mDais, O.ident(), { c: pal.dark ? O.rgbMix(pal.surface, pal.s3, 0.9) : O.rgbMix(pal.surface, pal.s2, 0.8), e: pal.flow, emit: pal.dark ? 0.03 : 0, gloss: 0.6, rim: pal.dark ? 0.45 : 0.15, flat: 0, a: 1, grey: 0 }, true);

    var pp = r.progs.plate;
    gl.useProgram(pp.p);
    gl.uniformMatrix4fv(pp.u.uVP, false, m.vp);
    gl.uniformMatrix4fv(pp.u.uModel, false, O.ident());
    gl.uniformMatrix3fv(pp.u.uNM, false, [1, 0, 0, 0, 1, 0, 0, 0, 1]);
    gl.uniform3fv(pp.u.uCam, m.eye);
    gl.uniform3fv(pp.u.uColor, (pal.dark ? O.rgbMix(pal.surface, pal.s2, 0.5) : O.rgbMix(pal.surface, pal.paper, 0.4)).slice(0, 3));
    gl.uniform3fv(pp.u.uLine, (pal.dark ? O.rgbMix(pal.brand, pal.s3, 0.35) : O.rgbMix(pal.brand, pal.surface, 0.35)).slice(0, 3));
    gl.uniform3fv(pp.u.uGlow, pal.brand.slice(0, 3));
    gl.uniform3fv(pp.u.uFog, fog.slice(0, 3));
    gl.uniform2f(pp.u.uFogR, FOG[0], FOG[1]);
    gl.uniform4f(pp.u.uRings, R_DAIS + 0.3, R_WORK, R_DOORS - 3.6, R_PLATE - 0.7);
    gl.uniform1f(pp.u.uTime, t);
    gl.uniform1f(pp.u.uDark, dark);
    gl.uniform1f(pp.u.uR, R_PLATE);
    gl.uniform1f(pp.u.uHex, 2.4);
    gl.bindVertexArray(this.mPlate.vao);
    gl.drawElements(gl.TRIANGLES, this.mPlate.count, this.mPlate.type, 0);

    // structures: opaque pass, then transparent (sorted far to near)
    gl.useProgram(pl.p);
    var ordered = this.parts.slice().sort(function (a, b) { return O.len(O.sub(b.pos, m.eye)) - O.len(O.sub(a.pos, m.eye)); });
    [true, false].forEach(function (opaque) {
      if (!opaque) { gl.enable(gl.BLEND); gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA); gl.depthMask(false); }
      ordered.forEach(function (n) {
        var off = !running(n);
        var grey = off ? 1 - n.st.on : 0;
        var ghost = off ? O.mix(0.6, 1, n.st.on) : 1;
        var base = O.trs(n.pos, n.p.ring === 'core' || n.p.ring === 'sky' ? 0 : n.p.a * DEG, n.s);
        var sel = self.sel === n.id || self.hover === n.id;
        if (!n.p.noPad) {
          var pm = mat('metalFlat', n.p.hue, grey, ghost);
          if (sel) pm.rim += 0.5;
          draw(self.mPad, O.mul(base, O.trs([0, 0, 0], Math.PI / 6, 1)), pm, opaque);
          var sm = mat('accent', n.want === 'warn' ? 'warn' : 'live', grey, ghost);
          sm.emit *= n.want === 'warn' ? 0.9 : n.st.on;
          if (off) { sm.c = pal.dark ? O.rgbMix(pal.s3, pal.muted, 0.2) : O.rgbMix(pal.s3, pal.muted, 0.3); sm.emit = 0; }
          draw(self.mPadRing, O.mul(base, O.trs([0, 0.05, 0], Math.PI / 6, 1)), sm, opaque);
        }
        n.meshes.forEach(function (pt) {
          var M = mat(pt.mat, n.p.hue, grey, ghost);
          if (pt.band != null && !off) M.emit *= 0.6 + n.st.act * (1.2 + Math.sin(n.st.ph * 4 - pt.band * 0.9));
          if (off) M.emit = 0;
          else M.emit *= 0.35 + 0.65 * n.st.on + n.st.act * 0.6;
          if (sel) M.rim += 0.35;
          draw(pt.mesh, O.mul(base, pt.m ? pt.m(t, n.st) : O.ident()), M, opaque);
        });
      });
      if (!opaque) {
        self.effects = self.effects.filter(function (fx) { return t - fx.t0 < fx.dur; });
        self.effects.forEach(function (fx) {
          var k = (t - fx.t0) / fx.dur, n = fx.n;
          var M = mat('accent', n.p.hue, 0, (1 - k) * 0.9);
          M.emit = 1.2;
          if (n.p.model === 'eventring') { draw(self.mEventRipple, O.trs([0, 0.42 + k * 0.6, 0], 0, 1 + k * 0.05), M, false); return; }
          var at = n.recipe.rippleAt || [0, PAD + 0.05, 0];
          draw(self.mRipple, O.mul(O.trs(n.pos, n.p.ring === 'core' ? 0 : n.p.a * DEG, n.s), O.trs(at, 0, 1.5 + k * 2.6, n.recipe.rippleTilt || 0)), M, false);
        });
        gl.depthMask(true); gl.disable(gl.BLEND);
      }
    });

    // traces
    var pt2 = r.progs.trace;
    gl.useProgram(pt2.p);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.SRC_ALPHA, pal.dark ? gl.ONE : gl.ONE_MINUS_SRC_ALPHA);
    gl.depthMask(false);
    gl.disable(gl.CULL_FACE);
    gl.uniformMatrix4fv(pt2.u.uVP, false, m.vp);
    gl.uniform1f(pt2.u.uDark, dark);
    this.links.forEach(function (l) {
      var on = Math.min(l.a.st.on, l.b.st.on);
      var hue = pal[l.a.p.hue] || pal.brand;
      var pk = [-1, -1, -1, -1], dir = [1, 1, 1, 1], c = 0;
      self.pkts.forEach(function (q) {
        if (q.l !== l || c > 3) return;
        var k = (t - q.t0) / q.dur;
        pk[c] = (q.rev ? 1 - k : k) * l.len; dir[c] = q.rev ? -1 : 1; c++;
      });
      gl.uniform3fv(pt2.u.uColor, (pal.dark ? hue : O.rgbMix(hue, pal.ink, 0.15)).slice(0, 3));
      gl.uniform4fv(pt2.u.uPk, pk);
      gl.uniform4fv(pt2.u.uDir, dir);
      gl.uniform1f(pt2.u.uOn, on);
      gl.uniform1f(pt2.u.uLen, l.len);
      gl.uniform1f(pt2.u.uSel, self.sel && (l.a.id === self.sel || l.b.id === self.sel) ? 1 : 0);
      gl.bindVertexArray(l.mesh.vao);
      gl.drawElements(gl.TRIANGLES, l.mesh.count, l.mesh.type, 0);
    });
    gl.enable(gl.CULL_FACE);

    // billboard light: pad lights, structure fx, drones' cargo, beams, engines
    this.sprN = 0;
    this.parts.forEach(function (n) {
      var off = !running(n);
      var base = O.trs(n.pos, n.p.ring === 'core' || n.p.ring === 'sky' ? 0 : n.p.a * DEG, n.s);
      self._hue = pal[n.p.hue] || pal.brand;
      if (!n.p.noPad && !off) {
        // landing lights chase round a running pad; amber all at once when it needs attention
        for (var c = 0; c < 6; c++) {
          var ang = c / 6 * TAU - Math.PI / 6, lp = self._local(n, base, [Math.cos(ang) * 1.62, 0.4, Math.sin(ang) * 1.62]);
          var lv = n.want === 'warn' ? 0.9 : n.want === 'idle' ? 0.3 : 0.5 + n.st.act * 0.5;
          self._spr(lp, 0.16 * n.s, n.want === 'warn' ? 'warn' : 'live', lv * n.st.on, false);
        }
      }
      if (!n.recipe.fx || (off && n.st.on < 0.05)) return;
      var wrap = {
        glow: function (p, size, hue, a) { self._spr(self._local(n, base, p), size * n.s, hue === 'accent' ? null : hue, a, true); },
        spark: function (p, size, hue, a) { self._spr(self._local(n, base, p), size * n.s, hue === 'accent' ? null : hue, a, false); }
      };
      self._hue = pal[n.p.hue] || pal.brand;
      n.recipe.fx(t, n.st, wrap);
    });
    if (this.sel) {
      var sn = this.byId[this.sel];
      this._hue = pal.brand;
      if (sn.p.ring !== 'sky' && sn.p.model !== 'eventring') this._spr([sn.pos[0], sn.pos[1] + 0.4, sn.pos[2]], 3.4 * sn.s, null, 0.24, true);
    }
    this.pkts = this.pkts.filter(function (pk) { return t - pk.t0 < pk.dur; });
    this.pkts.forEach(function (pk) {
      var q = self._pkt(pk, t);
      self._hue = pal[pk.hue] || pal.brand;
      self._spr(q, 0.55, null, 0.9, false);
      self._spr(q, 1.3, null, 0.35, true);
    });
    this.beams.forEach(function (bm) {
      var on = Math.min(1, bm.a.st.act * 1.5);
      if (on < 0.03) return;
      self._hue = pal[bm.b.p.hue] || pal.match;
      var a = O.add(bm.a.pos, [0, bm.a.recipe.h * bm.a.s * 0.6, 0]), b = bm.b.pos;
      for (var k = 0; k < 18; k++) {
        var u = ((k / 18) + bm.a.st.ph * 0.25) % 1;
        var p = O.mix3(a, b, u);
        p[1] += Math.sin(u * Math.PI) * 5;
        self._spr(p, 0.18, null, on * 0.7 * Math.sin(u * Math.PI), false);
      }
    });
    if (this.sprN) {
      var ps2 = r.progs.sprite;
      gl.useProgram(ps2.p);
      gl.uniformMatrix4fv(ps2.u.uVP, false, m.vp);
      var v = m.view;
      gl.uniform3f(ps2.u.uRight, v[0], v[4], v[8]);
      gl.uniform3f(ps2.u.uUp, v[1], v[5], v[9]);
      gl.enable(gl.BLEND);
      gl.depthMask(false);
      gl.blendFunc(gl.SRC_ALPHA, pal.dark ? gl.ONE : gl.ONE_MINUS_SRC_ALPHA);
      r.sprites(this.spr, this.sprN);
      gl.bindVertexArray(r.spriteVAO);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, this.sprN);
    }
    gl.depthMask(true);
    gl.disable(gl.BLEND);
    gl.bindVertexArray(null);

    this._placeLabels(m, W, H);
    var working = this.parts.some(function (n) {
      var goal = (n.want === 'on' || n.want === 'warn') ? 1 : n.want === 'idle' ? 0.55 : 0;
      return n.st.act > 0.004 || Math.abs(n.st.on - goal) > 0.004 || n.st.dropT < 1.2 || Math.abs(n.st.open - (n.st.openWant || 0)) > 0.004;
    });
    var busy = moving || working || this.effects.length > 0 || this.pkts.length > 0;
    this.animating = busy;
    this.dirty = false;
    return busy;
  };

  // _pkt is the position of a packet's light along its trace.
  Scene.prototype._pkt = function (pk, t) {
    var k = O.clamp((t - pk.t0) / pk.dur, 0, 1), pts = pk.l.pts;
    if (pk.rev) k = 1 - k;
    var f = k * (pts.length - 1), i = Math.min(pts.length - 2, Math.floor(f)), u = f - i;
    var px = O.mix(pts[i][0], pts[i + 1][0], u), pz = O.mix(pts[i][1], pts[i + 1][1], u);
    return [px, plateY(px, pz) + 0.18, pz];
  };

  Scene.prototype._local = function (n, base, p) { return O.xform(base, p); };

  Scene.prototype._spr = function (p, size, hue, a, soft) {
    if (this.sprN >= 4096 || a <= 0.003) return;
    var c = hue ? this.pal[hue] : this._hue, o = this.sprN * 8, d = this.spr;
    if (!this.pal.dark) { a *= soft ? 0.55 : 0.8; }
    d[o] = p[0]; d[o + 1] = p[1]; d[o + 2] = p[2]; d[o + 3] = soft ? size : size * 0.5;
    d[o + 4] = c[0]; d[o + 5] = c[1]; d[o + 6] = c[2]; d[o + 7] = Math.min(1, a) * (soft ? 0.6 : 1);
    this.sprN++;
  };

  // _placeLabels pins each part's label above it, then declutters: nearest
  // first, a label that would overlap one already placed is hidden (the
  // selected and hovered ones always show). Hidden labels stay reachable
  // from the parts index under the view.
  Scene.prototype._placeLabels = function (m, W, H) {
    var self = this, zoomed = this.cam.dist < 40, list = [];
    this.parts.forEach(function (n) {
      var b = n.label;
      if (!b) return;
      var anchor = n.p.model === 'eventring' ? [Math.sin(n.p.a * DEG) * R_EVENTS, 1.0, Math.cos(n.p.a * DEG) * R_EVENTS] : n.anchor;
      var s = O.project(m, anchor, W, H);
      var right = W - (self.clipRight || 0);
      if (!s || s.z > 1 || s.x < -60 || s.x > right - 30 || s.y < -30 || s.y > H + 30) { b.style.visibility = 'hidden'; return; }
      if (!n.w) { n.w = b.offsetWidth || 90; n.h = b.offsetHeight || 20; }
      list.push({ n: n, b: b, s: s, pri: (self.sel === n.id || self.hover === n.id) ? 0 : 1 });
    });
    list.sort(function (a, b) { return a.pri - b.pri || a.s.w - b.s.w; });
    var placed = [];
    list.forEach(function (it) {
      var x0 = it.s.x - it.n.w / 2 - 3, y0 = it.s.y - it.n.h - 3, x1 = x0 + it.n.w + 6, y1 = it.s.y + 3;
      var clash = it.pri && placed.some(function (q) { return x0 < q[2] && x1 > q[0] && y0 < q[3] && y1 > q[1]; });
      var b = it.b;
      if (clash) { b.style.visibility = 'hidden'; return; }
      placed.push([x0, y0, x1, y1]);
      b.style.visibility = '';
      b.style.transform = 'translate(' + Math.round(it.s.x) + 'px,' + Math.round(it.s.y) + 'px) translate(-50%,-100%)';
      var depth = O.clamp((it.s.w - 20) / 100, 0, 1);
      b.style.opacity = it.pri ? String(O.mix(1, zoomed ? 0.88 : 0.72, depth)) : '1';
      b.style.zIndex = String(1000 - Math.round(it.s.w));
    });
  };

  root.SeamlessScene = { Scene: Scene, PARTS: PARTS, LINKS: LINKS, CAPTIONS: CAPTIONS };
})(window);

/* The System screen's controller: it mounts the scene on the page's canvas,
   feeds it the server's parts (the JSON in #sys-data), and turns the live
   stream into motion. Every recorded event the layout re-dispatches as
   seam:event lights the path its data took (data.chains); nothing moves
   otherwise. The numbers refresh from /console/system?format=json while the
   page is visible, and the layout's live morph is off for this page
   (window.SEAM_NO_LIVE_REFRESH, set in the template): the canvas, its labels
   and the readout are this script's.

   Frame pacing keeps the GPU idle: the display's rate while the owner drives
   the camera, 30 fps while work animates, and no frames at all otherwise --
   or when the tab is hidden or the view is scrolled away. The canvas keeps
   to a pixel budget, so a 5K or 6K display is not asked for every pixel. */
(function () {
  'use strict';
  var O = window.Orrery, S = window.SeamlessScene;
  var stage = document.getElementById('sys-stage');
  var dataEl = document.getElementById('sys-data');
  if (!stage || !dataEl || !O || !S) return;
  var data;
  try { data = JSON.parse(dataEl.textContent); } catch (e) { return; }

  var reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');
  var canvas = document.getElementById('sys-canvas');
  var labels = document.getElementById('sys-labels'), caps = document.getElementById('sys-captions');
  var readout = document.getElementById('sys-readout'), roBody = document.getElementById('sys-ro-body');
  var calmBtn = document.getElementById('sys-calm'), homeBtn = document.getElementById('sys-home');
  var RINGS = { doors: 'Doors', work: 'Work', core: 'Core', sky: 'Outside' };
  var BLURB = { doors: 'where your agents and you come in', work: 'what Seamless does with it', core: 'where knowledge is kept on disk', sky: 'services on the internet it calls' };

  var byId = {};
  function index(d) { byId = {}; (d.parts || []).forEach(function (p) { byId[p.id] = p; }); }
  index(data);

  function live(p) { return p && (p.state === 'on' || p.state === 'idle' || p.state === 'warn'); }
  function stateMap() {
    var m = {};
    data.parts.forEach(function (p) {
      var recent = 0;
      (p.spark || []).slice(-10).forEach(function (v) { recent += v; });
      m[p.id] = { state: p.state, rate: Math.min(1, recent / 40), count: p.count == null ? null : p.count, open: !!p.open };
    });
    return m;
  }

  var scene = null;
  var labelText = {};
  data.parts.forEach(function (p) { labelText[p.id] = p.label; });
  try {
    scene = new S.Scene(canvas, labels, { calm: !!(reduce && reduce.matches), labels: labelText, onSelect: showReadout, onHover: hoverPart });
    scene.setData(stateMap());
  } catch (e) {
    scene = null;
    var nogl = document.getElementById('sys-nogl');
    if (nogl) nogl.hidden = false;
    canvas.hidden = true;
  }

  /* ---- readout ------------------------------------------------------ */

  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }
  function stateWord(s) { return s === 'on' ? 'Running' : s === 'idle' ? 'Quiet' : s === 'warn' ? 'Needs attention' : s === 'absent' ? 'Not in this build' : 'Off'; }
  function sw(s) { return s === 'absent' ? 'off' : s; }
  var HUE = { doors: 'var(--pop)', work: 'var(--brand)', core: 'var(--flow)', sky: 'var(--match)' };

  function sparkSVG(h) {
    var n = h.length, W = 300, H = 60, max = 4;
    for (var i = 0; i < n; i++) if (h[i] > max) max = h[i];
    var pts = h.map(function (v, k) { return [k / Math.max(1, n - 1) * W, H - 4 - v / max * (H - 10)]; });
    var line = pts.map(function (p, k) { return (k ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1); }).join(' ');
    var last = pts[n - 1];
    return '<svg viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none" aria-hidden="true">' +
      '<defs><linearGradient id="sys-sparkfill" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="currentColor" stop-opacity="0.3"/><stop offset="1" stop-color="currentColor" stop-opacity="0"/></linearGradient></defs>' +
      '<line class="gl" x1="0" x2="' + W + '" y1="' + (H / 2) + '" y2="' + (H / 2) + '"/><line class="gl" x1="0" x2="' + W + '" y1="' + (H - 2) + '" y2="' + (H - 2) + '"/>' +
      '<path class="ar" d="' + line + ' L' + W + ' ' + H + ' L0 ' + H + ' Z"/><path class="ln" d="' + line + '"/>' +
      '<circle class="ptr" cx="' + last[0] + '" cy="' + last[1] + '" r="6"/><circle class="pt" cx="' + last[0] + '" cy="' + last[1] + '" r="3"/></svg>';
  }

  var shown = null;
  var X = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>';
  function showReadout(id) {
    document.querySelectorAll('[data-pick]').forEach(function (b) { b.setAttribute('aria-current', b.getAttribute('data-pick') === id ? 'true' : 'false'); });
    if (!id || !byId[id]) {
      readout.classList.remove('open'); readout.setAttribute('aria-hidden', 'true'); shown = null; setShift();
      if (location.hash) history.replaceState(null, '', location.pathname + location.search);
      return;
    }
    var p = byId[id];
    shown = id;
    var h = '<div class="sys-ro-top"><p class="sys-ro-ring">' + esc(RINGS[p.ring]) + ' &middot; ' + esc(BLURB[p.ring]) + '</p>' +
      '<button class="sys-ro-close" type="button" data-ro-close aria-label="Close">' + X + '</button></div>';
    h += '<div><h2 class="sys-ro-name">' + esc(p.label) + '</h2><p class="sys-ro-role">' + esc(p.role) + '</p><p class="sys-ro-state"><span class="sys-pill ' + p.state + '">' + stateWord(p.state) + '</span></p></div>';
    h += '<div class="sys-hero" style="--hue:' + HUE[p.ring] + '"><div class="sys-hero-num" id="sys-hero">' + esc(p.hero.value) + (p.hero.unit ? '<small>' + esc(p.hero.unit) + '</small>' : '') + '</div><div class="sys-hero-label">' + esc(p.hero.label) + '</div>';
    if (p.spark && p.sparkLabel) {
      h += '<figure class="sys-spark" style="color:' + HUE[p.ring] + '"><div id="sys-spark">' + sparkSVG(p.spark) + '</div><figcaption><span>' + esc(p.sparkLabel) + ', last ' + data.sparkMinutes + ' min</span><span id="sys-now">now ' + p.spark[p.spark.length - 1] + '</span></figcaption></figure>';
    }
    h += '</div>';
    if (p.metrics && p.metrics.length) {
      h += '<dl class="sys-metrics">' + p.metrics.map(function (m) { return '<div class="sys-metric"><dt>' + esc(m.label) + '</dt><dd>' + esc(m.value) + (m.unit ? '<small> ' + esc(m.unit) + '</small>' : '') + '</dd></div>'; }).join('') + '</dl>';
    }
    if (p.turnOn) {
      h += '<div class="sys-turnon"><h3>Turn it on</h3><p>' + esc(p.turnOn.text) + '</p>' + (p.turnOn.code ? '<pre class="sys-code">' + esc(p.turnOn.code) + '</pre>' : '') + '</div>';
    }
    h += '<div><h3>What it does</h3><p class="sys-body">' + esc(p.what) + '</p></div>';
    if (p.talks && p.talks.length) {
      h += '<div><h3>Works with</h3><div class="sys-chips">' + p.talks.filter(function (t) { return byId[t]; }).map(function (t) {
        return '<button class="sys-chip" type="button" data-jump="' + esc(t) + '"><span class="sys-sw ' + sw(byId[t].state) + '"></span>' + esc(byId[t].label) + '</button>';
      }).join('') + '</div></div>';
    }
    if (p.facts && p.facts.length) {
      h += '<div><h3>Settings</h3><dl class="sys-facts">' + p.facts.map(function (f) { return '<dt>' + esc(f.label) + '</dt><dd>' + esc(f.value) + '</dd>'; }).join('') + '</dl></div>';
    }
    if (p.href) h += '<a class="sys-go" href="' + esc(p.href) + '">' + esc(p.hrefLabel || 'Open') + ' &rarr;</a>';
    roBody.innerHTML = h;
    readout.classList.add('open');
    readout.setAttribute('aria-hidden', 'false');
    if (location.hash !== '#' + id) history.replaceState(null, '', '#' + id);
    setShift();
  }
  function refreshShown() {
    if (!shown || !byId[shown]) return;
    var p = byId[shown], hero = document.getElementById('sys-hero'), sp = document.getElementById('sys-spark'), now = document.getElementById('sys-now');
    if (hero) hero.innerHTML = esc(p.hero.value) + (p.hero.unit ? '<small>' + esc(p.hero.unit) + '</small>' : '');
    if (sp && p.spark) sp.innerHTML = sparkSVG(p.spark);
    if (now && p.spark) now.textContent = 'now ' + p.spark[p.spark.length - 1];
  }

  readout.addEventListener('click', function (e) {
    var t = e.target.closest('button');
    if (!t) return;
    if (t.hasAttribute('data-ro-close')) pick(null);
    else if (t.hasAttribute('data-jump')) pick(t.getAttribute('data-jump'), true);
  });
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-pick]');
    if (!b) return;
    pick(b.getAttribute('data-pick'), true);
    stage.scrollIntoView({ behavior: reduce && reduce.matches ? 'auto' : 'smooth', block: 'center' });
  });

  function pick(id, fly) {
    if (scene) scene.select(id, fly); else showReadout(id);
    kick();
  }
  function setShift() {
    if (!scene) return;
    var wide = stage.clientWidth > 760 && readout.classList.contains('open');
    scene.cam.shift = wide ? (readout.offsetWidth + 24) / stage.clientWidth : 0;
    scene.clipRight = wide ? readout.offsetWidth + 24 : 0;
    kick();
  }
  function hoverPart(id) { canvas.style.cursor = id ? 'pointer' : ''; }

  /* ---- tally filter --------------------------------------------------- */

  document.querySelectorAll('[data-tally]').forEach(function (b) {
    b.addEventListener('click', function () {
      var s = b.getAttribute('data-tally'), on = b.getAttribute('aria-pressed') !== 'true';
      document.querySelectorAll('[data-tally]').forEach(function (x) { x.setAttribute('aria-pressed', 'false'); });
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
      stage.classList.toggle('dim', on);
      document.querySelectorAll('.orr-label').forEach(function (l) {
        var p = byId[l.getAttribute('data-part')];
        l.classList.toggle('is-match', on && !!p && sw(p.state) === s);
      });
    });
  });

  /* ---- camera controls -------------------------------------------------- */

  var ptrs = {}, drag = null, pinch = null, driving = false;
  function pan(dx, dy) {
    var c = scene.cam, k = c.dist * 0.0018;
    var right = [Math.cos(c.yaw), 0, -Math.sin(c.yaw)], fwd = [-Math.sin(c.yaw), 0, -Math.cos(c.yaw)];
    c.goal.target = O.add(c.goal.target, O.add(O.mulS(right, -dx * k), O.mulS(fwd, dy * k)));
    var r = Math.sqrt(c.goal.target[0] * c.goal.target[0] + c.goal.target[2] * c.goal.target[2]);
    if (r > 40) c.goal.target = O.mulS(c.goal.target, 40 / r);
  }
  if (scene) {
    canvas.addEventListener('pointerdown', function (e) {
      canvas.setPointerCapture(e.pointerId);
      driving = true;
      ptrs[e.pointerId] = { x: e.clientX, y: e.clientY };
      var ids = Object.keys(ptrs);
      if (ids.length === 2) {
        var a = ptrs[ids[0]], b = ptrs[ids[1]];
        pinch = { d: Math.hypot(a.x - b.x, a.y - b.y), dist: scene.cam.goal.dist };
        drag = null;
      } else {
        drag = { x: e.clientX, y: e.clientY, moved: 0, pan: e.button === 2 || e.shiftKey };
        scene.cam.fly = null;
      }
      kick();
    });
    canvas.addEventListener('pointermove', function (e) {
      if (ptrs[e.pointerId]) ptrs[e.pointerId] = { x: e.clientX, y: e.clientY };
      if (pinch) {
        var ids = Object.keys(ptrs);
        if (ids.length === 2) {
          var a = ptrs[ids[0]], b = ptrs[ids[1]], d = Math.hypot(a.x - b.x, a.y - b.y);
          scene.cam.goal.dist = O.clamp(pinch.dist * pinch.d / Math.max(20, d), scene.cam.minD, scene.cam.maxD);
          kick();
        }
        return;
      }
      if (!drag) {
        if (e.pointerType === 'mouse') {
          var hv = scene.pick(e.offsetX, e.offsetY);
          if (hv !== scene.hover) { scene.hover = hv; hoverPart(hv); kick(); }
        }
        return;
      }
      var dx = e.clientX - drag.x, dy = e.clientY - drag.y;
      drag.moved += Math.abs(dx) + Math.abs(dy);
      drag.x = e.clientX; drag.y = e.clientY;
      if (drag.moved > 4) canvas.classList.add('dragging');
      if (drag.pan) pan(dx, dy);
      else {
        scene.cam.goal.yaw -= dx * 0.0055;
        scene.cam.goal.pitch = O.clamp(scene.cam.goal.pitch + dy * 0.004, 0.1, 1.38);
      }
      kick();
    });
    var up = function (e) {
      delete ptrs[e.pointerId];
      if (pinch && Object.keys(ptrs).length < 2) { pinch = null; return; }
      if (drag && drag.moved <= 4) {
        var id = scene.pick(e.offsetX, e.offsetY);
        pick(id, !!id);
      }
      drag = null;
      driving = false;
      canvas.classList.remove('dragging');
      kick();
    };
    canvas.addEventListener('pointerup', up);
    canvas.addEventListener('pointercancel', function (e) { delete ptrs[e.pointerId]; drag = null; pinch = null; driving = false; canvas.classList.remove('dragging'); });
    canvas.addEventListener('contextmenu', function (e) { e.preventDefault(); });
    canvas.addEventListener('wheel', function (e) {
      e.preventDefault();
      var c = scene.cam;
      c.fly = null;
      c.goal.dist = O.clamp(c.goal.dist * Math.exp(e.deltaY * (e.ctrlKey ? 0.01 : 0.0013)), c.minD, c.maxD);
      kick();
    }, { passive: false });
    canvas.addEventListener('keydown', function (e) {
      var c = scene.cam, k = e.key;
      if (k === 'ArrowLeft') c.goal.yaw += 0.12;
      else if (k === 'ArrowRight') c.goal.yaw -= 0.12;
      else if (k === 'ArrowUp') c.goal.pitch = O.clamp(c.goal.pitch + 0.08, 0.1, 1.38);
      else if (k === 'ArrowDown') c.goal.pitch = O.clamp(c.goal.pitch - 0.08, 0.1, 1.38);
      else if (k === '+' || k === '=') c.goal.dist = O.clamp(c.goal.dist * 0.85, c.minD, c.maxD);
      else if (k === '-' || k === '_') c.goal.dist = O.clamp(c.goal.dist / 0.85, c.minD, c.maxD);
      else if (k === 'Escape') { pick(null); scene.home(); }
      else return;
      e.preventDefault();
      kick();
    });
  }
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && shown) { pick(null); if (scene) scene.home(); } });
  if (homeBtn) homeBtn.addEventListener('click', function () { pick(null); if (scene) scene.home(); kick(); });
  if (calmBtn) {
    calmBtn.setAttribute('aria-pressed', scene && scene.calm ? 'true' : 'false');
    calmBtn.addEventListener('click', function () {
      var on = calmBtn.getAttribute('aria-pressed') !== 'true';
      calmBtn.setAttribute('aria-pressed', on ? 'true' : 'false');
      if (scene) { scene.calm = on; kick(); }
    });
  }

  /* ---- ring captions ------------------------------------------------------ */

  var capEls = S.CAPTIONS.map(function (c) { var d = document.createElement('div'); d.className = 'sys-cap'; d.textContent = c[0]; caps.appendChild(d); return d; });
  function placeCaptions() {
    if (!scene) return;
    var m = scene.cam.matrices(), W = canvas.clientWidth, H = canvas.clientHeight, a = scene.cam.yaw - 0.42;
    S.CAPTIONS.forEach(function (c, i) {
      var s = O.project(m, [Math.sin(a) * c[1], i === 2 ? 0.36 : 0.05, Math.cos(a) * c[1]], W, H);
      if (!s) { capEls[i].style.visibility = 'hidden'; return; }
      capEls[i].style.visibility = '';
      capEls[i].style.transform = 'translate(' + Math.round(s.x) + 'px,' + Math.round(s.y) + 'px) translate(-50%,-50%)';
    });
  }

  /* ---- frame pacing -------------------------------------------------------- */

  var raf = 0, timer = 0, visible = !document.hidden, lastDraw = 0;
  var PIXELS = 3.6e6;
  function size() {
    if (!scene) return;
    var w = canvas.clientWidth, h = canvas.clientHeight;
    var dpr = Math.max(1, Math.min(window.devicePixelRatio || 1, 2, Math.sqrt(PIXELS / Math.max(1, w * h))));
    setShift();
    // a resize clears the canvas: redraw now, never leave it blank until the next tick
    if (scene.r.resize(w, h, dpr) && visible) { lastDraw = performance.now(); scene.frame(lastDraw); placeCaptions(); }
  }
  function fpsNow() {
    if (driving || scene.moving || (scene.cam.fly && !scene.calm)) return 60;
    if (scene.animating !== false || scene.dirty) return 30;
    return 0;
  }
  function loop(now) {
    raf = 0;
    if (!scene || !visible) return;
    var fps = fpsNow();
    if (fps >= 60 || now - lastDraw >= 1000 / Math.max(fps, 1) - 4) {
      lastDraw = now;
      scene.frame(now);
      placeCaptions();
    }
    schedule();
  }
  function schedule() {
    if (raf || timer || !scene || !visible) return;
    var fps = fpsNow();
    if (!fps) return; // still: nothing to draw until the next change
    if (fps >= 60) { raf = requestAnimationFrame(loop); return; }
    timer = setTimeout(function () { timer = 0; raf = requestAnimationFrame(loop); }, Math.max(0, 1000 / fps - (performance.now() - lastDraw) - 4));
  }
  function kick() {
    if (!scene || !visible) return;
    scene.dirty = true;
    if (timer) { clearTimeout(timer); timer = 0; }
    if (!raf) raf = requestAnimationFrame(loop);
  }
  if (scene) {
    new ResizeObserver(function () { size(); kick(); }).observe(stage);
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (es) { visible = es[0].isIntersecting && !document.hidden; kick(); }).observe(stage);
    }
    document.addEventListener('visibilitychange', function () { visible = !document.hidden; if (visible) refresh(); kick(); });
    window.addEventListener('pagehide', function () { visible = false; scene.r.release(); });
    new MutationObserver(function () { scene.theme(); kick(); }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    if (window.matchMedia) window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function () { scene.theme(); kick(); });
    if (reduce) reduce.addEventListener('change', function () { scene.calm = reduce.matches; if (calmBtn) calmBtn.setAttribute('aria-pressed', reduce.matches ? 'true' : 'false'); kick(); });
    size();
    kick();
  }
  var want = (location.hash || '').replace(/^#/, '');
  if (want && byId[want]) { if (!scene || scene.calm) pick(want, true); else setTimeout(function () { pick(want, true); }, 250); }

  /* ---- live: real events light the path their data took ------------------ */

  function play(kind) {
    var chain = data.chains[kind];
    if (!scene) return;
    if (live(byId.events)) scene.pulse('events', { strength: 0.35, to: '__none' });
    if (!chain) return;
    var hops = chain.hops.filter(function (id) { return live(byId[id]); });
    hops.forEach(function (id, i) {
      setTimeout(function () {
        scene.pulse(id, { to: hops[i + 1] || '__none', strength: 0.6 });
        var p = byId[id];
        if (p && p.spark && p.spark.length) { p.spark[p.spark.length - 1]++; if (shown === id) refreshShown(); }
        kick();
      }, i * 240);
    });
    (chain.also || []).forEach(function (pair, j) {
      if (!live(byId[pair[0]]) || !live(byId[pair[1]])) return;
      setTimeout(function () { scene.pulse(pair[0], { to: pair[1], strength: 0.4 }); kick(); }, (hops.length + j) * 240);
    });
  }
  var burst = 0;
  document.addEventListener('seam:event', function (e) {
    var d = e.detail || {};
    if (!d.kind || !visible) return;
    play(d.kind);
    // the numbers follow shortly after a burst settles
    if (burst) clearTimeout(burst);
    burst = setTimeout(refresh, 2500);
  });

  /* ---- refresh: states and numbers from the server ------------------------ */

  var busy = false;
  function refresh() {
    if (busy || document.hidden) return;
    busy = true;
    fetch('/console/system?format=json', { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d || !d.parts) return;
        data = d; index(d);
        if (scene) scene.setData(stateMap());
        document.querySelectorAll('[data-tally]').forEach(function (b) {
          var n = b.querySelector('strong');
          if (n && d.tally) n.textContent = d.tally[b.getAttribute('data-tally')] || 0;
        });
        document.querySelectorAll('[data-pick]').forEach(function (b) {
          var p = byId[b.getAttribute('data-pick')], st = b.querySelector('.sys-st'), dot = b.querySelector('.sys-sw');
          if (!p) return;
          if (st) st.textContent = stateWord(p.state);
          if (dot) dot.className = 'sys-sw ' + sw(p.state);
        });
        refreshShown();
        kick();
      })
      .catch(function () {})
      .finally(function () { busy = false; });
  }
  setInterval(function () { if (visible) refresh(); }, 30000);
})();
