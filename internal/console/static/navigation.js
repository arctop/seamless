/* Console data navigation: filters, sorts, time windows, GET searches, and
   owner mutation forms stay inside the current document. The server remains
   the single rendering source; this client fetches the next server-rendered
   page and morphs only the changed DOM into .main. Real links/forms remain as
   the no-JS fallback.

   window.SeamConsole also exposes the same morph to the SSE live refresher so
   every data refresh shares one no-reload path. */
(function () {
  'use strict';

  var inflight = null;
  var requestSeq = 0;
  var toastTimer = null;
  var currentViewURL = new URL(location.href);

  /* Held views. A server-versioned island names its version in data-live-v
     (the Overview's knowledge sky). A fetch of the page sends back the
     versions it holds (heldViews), and the server answers with only what
     changed since, using four markers the morph below understands:

       data-live-base   the version the answer was drawn against; it applies
                        only to a page still holding exactly that version.
       data-live-keep   this keyed node is unchanged: keep the current one.
       data-live-patch  only the listed keyed children changed: upsert them in
                        id order and leave the other children alone.
       data-live-gone   (a child of a patch) remove the child with this id.

     The markers describe the answer, never the page: none is copied onto the
     document. A page that turns out not to hold what the answer assumed
     throws StaleView; patchPage then forgets every held version, so the next
     fetch asks for the whole view. */
  var MARKERS = ['data-live-base', 'data-live-keep', 'data-live-patch', 'data-live-gone'];
  var MARKED = '[' + MARKERS.join('], [') + ']';
  function StaleView() { this.name = 'StaleView'; }
  function stale(error) { return error instanceof StaleView; }
  function marker(name) { return MARKERS.indexOf(name) >= 0; }
  function marked(node) {
    return node.nodeType === 1 && (node.matches(MARKED) || !!node.querySelector(MARKED));
  }

  function heldViews(headers) {
    var sky = document.querySelector('.main #sky[data-live-v]');
    if (sky) headers['X-Seam-Sky'] = sky.getAttribute('data-live-v');
    return headers;
  }
  function forgetHeldViews() {
    document.querySelectorAll('.main [data-live-v]').forEach(function (el) { el.removeAttribute('data-live-v'); });
  }

  function imported(node) { return document.importNode(node, true); }
  function inserted(node) {
    // An answer drawn against a held view only ever refers to nodes the page
    // has; one this page lacks means it does not hold that view.
    if (marked(node)) throw new StaleView();
    var copy = imported(node);
    if (copy.nodeType === 1) copy.classList.add('live-in');
    return copy;
  }
  function keyOf(node) {
    return node.nodeType === 1 && node.id ? node.nodeName + '#' + node.id : null;
  }
  function syncAttrs(from, to) {
    var targetAttrs = to.attributes;
    var currentAttrs = from.attributes;
    var i;
    var attr;
    for (i = 0; i < targetAttrs.length; i++) {
      attr = targetAttrs[i];
      if (marker(attr.name)) continue;
      if (from.getAttribute(attr.name) !== attr.value) from.setAttribute(attr.name, attr.value);
    }
    for (i = currentAttrs.length - 1; i >= 0; i--) {
      attr = currentAttrs[i];
      if (to.hasAttribute(attr.name)) continue;
      if (attr.name === 'open' && from.nodeName === 'DETAILS') continue;
      from.removeAttribute(attr.name);
    }
  }
  function syncControl(from, to) {
    if (from.nodeName === 'INPUT') {
      if (from.type !== 'file' && from.value !== to.value) from.value = to.value;
      if ((from.type === 'checkbox' || from.type === 'radio') && from.checked !== to.checked) from.checked = to.checked;
    } else if (from.nodeName === 'TEXTAREA') {
      if (from.value !== to.value) from.value = to.value;
    } else if (from.nodeName === 'SELECT') {
      if (from.value !== to.value) from.value = to.value;
    }
  }
  function morphNode(from, to) {
    if (from.nodeType !== to.nodeType ||
        (from.nodeType === 1 && from.nodeName !== to.nodeName)) {
      from.replaceWith(inserted(to));
      return;
    }
    if (from.nodeType !== 1) {
      if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue;
      return;
    }
    if (from.hasAttribute('data-live-skip')) return;
    if (to.hasAttribute('data-live-base') && to.getAttribute('data-live-base') !== from.getAttribute('data-live-v')) throw new StaleView();
    if (to.hasAttribute('data-live-keep') || to.hasAttribute('data-live-patch')) {
      // A stub must land on the node it names, never on whatever unkeyed
      // node happens to stand in its place.
      if (keyOf(from) !== keyOf(to)) throw new StaleView();
      if (to.hasAttribute('data-live-keep')) return;
      syncAttrs(from, to);
      patchChildren(from, to);
      return;
    }
    syncAttrs(from, to);
    morphChildren(from, to);
    syncControl(from, to);
  }
  // patchChildren applies a data-live-patch answer: every child it carries is
  // keyed; a gone stub removes its namesake, a known child morphs in place,
  // and a new one goes in id order (the order the server keeps them in). The
  // children it does not mention are unchanged and stay as they are.
  function patchChildren(from, to) {
    for (var fresh = to.firstElementChild; fresh; fresh = fresh.nextElementSibling) {
      if (!fresh.id) throw new StaleView();
      var current = document.getElementById(fresh.id);
      if (current && current.parentNode !== from) throw new StaleView();
      if (fresh.hasAttribute('data-live-gone')) {
        if (current) from.removeChild(current);
      } else if (current) {
        morphNode(current, fresh);
      } else {
        from.insertBefore(inserted(fresh), childAfter(from, fresh.id));
      }
    }
  }
  function childAfter(parent, id) {
    for (var child = parent.firstElementChild; child; child = child.nextElementSibling) {
      if (child.id > id) return child;
    }
    return null;
  }
  function morphChildren(from, to) {
    var keyed = {};
    var old;
    for (old = from.firstChild; old; old = old.nextSibling) {
      var key = keyOf(old);
      if (key) keyed[key] = old;
    }

    var oldChild = from.firstChild;
    var newChild = to.firstChild;
    while (newChild) {
      var nextNew = newChild.nextSibling;
      var newKey = keyOf(newChild);
      if (newKey && keyed[newKey]) {
        var match = keyed[newKey];
        delete keyed[newKey];
        if (match === oldChild) oldChild = oldChild.nextSibling;
        else from.insertBefore(match, oldChild);
        morphNode(match, newChild);
      } else if (oldChild) {
        var oldKey = keyOf(oldChild);
        if (oldKey && keyed[oldKey]) {
          from.insertBefore(inserted(newChild), oldChild);
        } else if (!oldKey && oldChild.nodeType === newChild.nodeType &&
                   (oldChild.nodeType !== 1 || oldChild.nodeName === newChild.nodeName)) {
          morphNode(oldChild, newChild);
          oldChild = oldChild.nextSibling;
        } else {
          from.insertBefore(inserted(newChild), oldChild);
        }
      } else {
        from.appendChild(inserted(newChild));
      }
      newChild = nextNew;
    }
    while (oldChild) {
      var nextOld = oldChild.nextSibling;
      from.removeChild(oldChild);
      oldChild = nextOld;
    }
    Object.keys(keyed).forEach(function (key) {
      if (keyed[key].parentNode === from) from.removeChild(keyed[key]);
    });
  }

  function stripAnimations() {
    setTimeout(function () {
      document.querySelectorAll('.live-in, nav.nav .count.bump').forEach(function (el) {
        el.classList.remove('live-in', 'bump');
      });
    }, 600);
  }

  // navShape is the sidebar's link set. It is stable across ordinary navigation
  // and changes only when an optional feature is toggled in Settings and its
  // entries appear or disappear.
  function navShape(nav) {
    return Array.prototype.map.call(nav.querySelectorAll('a'), function (link) {
      return link.getAttribute('href');
    }).join('\n');
  }

  // syncNav keeps the sidebar in step with the fetched document. Counts and the
  // active marker are patched in place, because that is what preserves the count
  // bump animation. A changed link set is a structural change the in-place patch
  // cannot express -- the two node lists no longer line up index by index -- so
  // the whole nav is morphed instead. Without that branch, switching an optional
  // feature off in Settings would leave the sidebar offering its screens until
  // the next full page load, and switching one on would hide them just as long.
  function syncNav(currentNav, freshNav) {
    if (navShape(currentNav) !== navShape(freshNav)) {
      morphNode(currentNav, freshNav);
      return;
    }
    var freshCounts = freshNav.querySelectorAll('.count');
    var currentCounts = currentNav.querySelectorAll('.count');
    if (freshCounts.length === currentCounts.length) {
      for (var i = 0; i < currentCounts.length; i++) {
        if (currentCounts[i].textContent === freshCounts[i].textContent) continue;
        currentCounts[i].textContent = freshCounts[i].textContent;
        // data-zero lets the collapsed rail drop an attention mark at zero.
        if (freshCounts[i].hasAttribute('data-zero')) currentCounts[i].setAttribute('data-zero', '');
        else currentCounts[i].removeAttribute('data-zero');
        currentCounts[i].classList.remove('bump');
        void currentCounts[i].offsetWidth;
        currentCounts[i].classList.add('bump');
      }
    }
    var freshLinks = freshNav.querySelectorAll('a');
    var currentLinks = currentNav.querySelectorAll('a');
    if (freshLinks.length === currentLinks.length) {
      for (var j = 0; j < currentLinks.length; j++) {
        currentLinks[j].classList.toggle('active', freshLinks[j].classList.contains('active'));
      }
    }
  }

  function patchPage(doc, source) {
    var freshMain = doc.querySelector('.main');
    var currentMain = document.querySelector('.main');
    var freshNav = doc.querySelector('nav.nav');
    var currentNav = document.querySelector('nav.nav');
    if (!freshMain || !currentMain || !freshNav || !currentNav) return false;

    try {
      morphNode(currentMain, freshMain);
    } catch (error) {
      // An answer drawn against a held view cannot stand in for the page: if
      // it does not apply, forget the held views and let the caller fetch the
      // whole view. The morph may have stopped part way, which that fetch
      // repairs.
      if (stale(error) || marked(freshMain)) {
        forgetHeldViews();
        throw stale(error) ? error : new StaleView();
      }
      // A morph edge case still stays inside this document. Replace only the
      // view contents with the fully rendered response; never reload the page.
      var children = Array.prototype.slice.call(freshMain.childNodes).map(function (node) { return imported(node); });
      currentMain.replaceChildren.apply(currentMain, children);
    }
    syncNav(currentNav, freshNav);
    // The account row names the console level: a level switch must reach it
    // too. Only its copy is morphed -- the theme toggle beside it keeps the
    // label its own script manages.
    var freshAccount = doc.querySelector('.side-foot .account-copy');
    var currentAccount = document.querySelector('.side-foot .account-copy');
    if (freshAccount && currentAccount) morphNode(currentAccount, freshAccount);
    if (doc.title) document.title = doc.title;
    try {
      if (window.IX && window.IX.enhance) window.IX.enhance(currentMain);
    } catch (e) {}
    document.dispatchEvent(new CustomEvent('seam:content-updated', {
      detail: { source: source || 'data' }
    }));
    stripAnimations();
    return true;
  }

  function flash(message, tone) {
    var toast = document.getElementById('live-toast');
    if (!toast) return;
    toast.textContent = message;
    toast.hidden = false;
    toast.classList.toggle('error', tone === 'error');
    toast.classList.remove('show');
    void toast.offsetWidth;
    toast.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      toast.classList.remove('show');
      setTimeout(function () { toast.hidden = true; }, 300);
    }, 2600);
  }

  function setBusy(on) {
    var main = document.querySelector('.main');
    if (!main) return;
    if (on) main.setAttribute('aria-busy', 'true');
    else main.removeAttribute('aria-busy');
  }

  function canonicalURL(doc, fallback) {
    var auto = doc.querySelector('.main .lib-layout[data-auto-url]');
    var raw = auto && auto.getAttribute('data-auto-url');
    try { return new URL(raw || fallback, location.origin).href; }
    catch (e) { return fallback; }
  }

  function load(rawURL, options) {
    options = options || {};
    var target;
    try { target = new URL(rawURL, location.href); }
    catch (e) { flash('Could not open that view.', 'error'); return Promise.resolve(false); }

    var method = (options.method || 'GET').toUpperCase();
    var mutation = method !== 'GET';
    if (inflight && inflight.mutation) {
      flash('Another update is still in progress.', 'error');
      return Promise.resolve(false);
    }
    if (inflight && inflight.controller) inflight.controller.abort();
    var controller = window.AbortController ? new AbortController() : null;
    inflight = { controller: controller, mutation: mutation };
    var seq = ++requestSeq;
    setBusy(true);

    var headers = { Accept: 'text/html', 'X-Seam-Partial': '1' };
    // A mutation is answered whole: a stale answer is retried, and a POST
    // must never be sent twice.
    if (!mutation) heldViews(headers);
    return fetch(target.href, {
      method: method,
      body: method === 'GET' || method === 'HEAD' ? undefined : options.body,
      credentials: 'same-origin',
      headers: headers,
      signal: controller ? controller.signal : undefined
    }).then(function (response) {
      var finalURL = new URL(response.url || target.href, location.href);
      if (finalURL.pathname === '/console/login') {
        location.assign(finalURL.href);
        return null;
      }
      if (!response.ok) throw new Error('http ' + response.status);
      return response.text().then(function (html) {
        if (seq !== requestSeq) return false;
        var doc = new DOMParser().parseFromString(html, 'text/html');
        if (!doc.querySelector('.sidebar') || !patchPage(doc, options.source || 'query')) {
          throw new Error('invalid console document');
        }
        var nextURL = canonicalURL(doc, finalURL.href);
        if (options.history !== 'none') {
          var state = { seamView: 'query' };
          if (options.history === 'replace') history.replaceState(state, '', nextURL);
          else history.pushState(state, '', nextURL);
        }
        currentViewURL = new URL(nextURL, location.href);
        if (currentViewURL.hash) {
          try {
            var anchor = document.getElementById(decodeURIComponent(currentViewURL.hash.slice(1)));
            if (anchor) anchor.scrollIntoView({ block: 'start' });
          } catch (e) {}
        }
        return true;
      });
    }).catch(function (error) {
      if (error && error.name === 'AbortError') return false;
      if (stale(error) && !mutation && !options.whole) {
        // patchPage has forgotten the held views, so this fetch is answered
        // whole.
        var whole = { whole: true };
        for (var key in options) {
          if (Object.prototype.hasOwnProperty.call(options, key)) whole[key] = options[key];
        }
        return load(rawURL, whole);
      }
      flash('Could not update this view. The current data is unchanged.', 'error');
      document.dispatchEvent(new CustomEvent('seam:content-error', { detail: { error: error } }));
      return false;
    }).finally(function () {
      if (seq !== requestSeq) return;
      inflight = null;
      setBusy(false);
    });
  }

  function queryLink(anchor) {
    return anchor.hasAttribute('data-seam-query') || !!anchor.closest('[data-seam-query]');
  }

  document.addEventListener('click', function (event) {
    if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button === 1) return;
    var anchor = event.target.closest ? event.target.closest('a[href]') : null;
    if (!anchor || !queryLink(anchor) || anchor.target || anchor.hasAttribute('download')) return;
    var target;
    try { target = new URL(anchor.href, location.href); } catch (e) { return; }
    if (target.origin !== location.origin) return;
    if (target.pathname === location.pathname && target.search === location.search && target.hash) return;
    event.preventDefault();
    if (target.href === location.href) return;
    load(target.href, { history: 'push', source: 'query' });
  });

  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (event.defaultPrevented || !form.matches) return;
    var submitter = event.submitter;
    var method = (submitter && submitter.hasAttribute('formmethod') ? submitter.formMethod : form.method || 'get').toLowerCase();
    var query = form.matches('form[data-seam-query]');
    var mutation = method === 'post' && !!form.closest('.main');
    if ((method === 'get' && !query) || (method !== 'get' && !mutation)) return;
    event.preventDefault();

    var action = submitter && submitter.hasAttribute('formaction') ? submitter.formAction : form.action || location.href;
    var target = new URL(action, location.href);
    var data;
    try { data = new FormData(form, submitter); }
    catch (e) {
      data = new FormData(form);
      if (submitter && submitter.name) data.append(submitter.name, submitter.value);
    }
    if (method === 'get') {
      target.search = '';
      data.forEach(function (value, key) {
        if (typeof value === 'string') target.searchParams.append(key, value);
      });
      load(target.href, { history: 'push', source: 'query' });
      return;
    }

    var body;
    if ((form.enctype || '').toLowerCase() === 'multipart/form-data') {
      body = data;
    } else {
      body = new URLSearchParams();
      data.forEach(function (value, key) {
        if (typeof value === 'string') body.append(key, value);
      });
    }
    load(target.href, { method: 'POST', body: body, history: 'push', source: 'mutation' });
  });

  document.addEventListener('change', function (event) {
    var form = event.target.closest ? event.target.closest('form[data-seam-query][data-seam-auto-submit]') : null;
    if (!form) return;
    if (typeof form.requestSubmit === 'function') form.requestSubmit();
  });

  function surfaceBase() {
    var reader = document.getElementById('lib-reader');
    return reader && reader.getAttribute('data-base');
  }
  function needsView(rawURL) {
    var target = new URL(rawURL, location.href);
    if (target.origin !== location.origin || target.pathname.indexOf('/console/') !== 0) return false;
    var base = surfaceBase();
    var sameSurface = base
      ? (target.pathname === base || target.pathname.indexOf(base + '/') === 0)
      : target.pathname === currentViewURL.pathname;
    return !sameSurface || target.search !== currentViewURL.search;
  }

  window.addEventListener('popstate', function () {
    if (needsView(location.href)) load(location.href, { history: 'none', source: 'history' });
  });

  try {
    var initial = history.state || {};
    initial.seamView = initial.seamView || 'page';
    history.replaceState(initial, '', location.href);
  } catch (e) {}

  window.SeamConsole = {
    flash: flash,
    heldViews: heldViews,
    load: load,
    morph: morphNode,
    needsView: needsView,
    patchPage: patchPage,
    stale: stale
  };
})();
