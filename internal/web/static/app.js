/* Shared behaviour: theme toggle, sidebar filtering, button busy states.
   Everything here is progressive enhancement -- the pages render and navigate
   without it. */

(function () {
  'use strict';

  // ---------------------------------------------------------------- theme
  var toggle = document.getElementById('theme-toggle');
  if (toggle) {
    toggle.addEventListener('click', function () {
      var root = document.documentElement;
      var current = root.dataset.theme;
      if (!current) {
        // No explicit choice yet: flip away from whatever the OS is doing.
        var prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
        current = prefersDark ? 'dark' : 'light';
      }
      var next = current === 'dark' ? 'light' : 'dark';
      root.dataset.theme = next;
      try { localStorage.setItem('dl-theme', next); } catch (e) {}
    });
  }

  // ------------------------------------------------------- library filter
  // Two filters that compose: free text, and origin. A card has to satisfy both
  // to stay, and a category whose cards have all gone goes with them.
  var filter = document.getElementById('case-filter');
  var originBtns = document.querySelectorAll('[data-origin]:not([data-case])');
  if (filter || originBtns.length) {
    // Both filters live in the URL, so a reload keeps them and a filtered view
    // can be sent to someone. replaceState rather than pushState: typing into a
    // search box should not fill the back button with keystrokes.
    var params = new URLSearchParams(window.location.search);
    var origin = params.get('origin') || 'all';
    if (['all', 'builtin', 'custom'].indexOf(origin) === -1) origin = 'all';
    if (filter && params.get('q')) filter.value = params.get('q');

    var syncURL = function () {
      var next = new URLSearchParams(window.location.search);
      var q = filter ? filter.value.trim() : '';
      if (q) next.set('q', q); else next.delete('q');
      if (origin !== 'all') next.set('origin', origin); else next.delete('origin');
      var qs = next.toString();
      history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
    };

    var apply = function () {
      var q = filter ? filter.value.trim().toLowerCase() : '';
      document.querySelectorAll('[data-case]').forEach(function (el) {
        var hay = (el.dataset.name || el.textContent || '').toLowerCase();
        var textOK = q === '' || hay.indexOf(q) !== -1;
        var originOK = origin === 'all' || el.dataset.origin === origin;
        el.classList.toggle('is-hidden', !(textOK && originOK));
      });
      document.querySelectorAll('[data-group]').forEach(function (group) {
        var anyVisible = Array.prototype.some.call(
          group.querySelectorAll('[data-case]'),
          function (el) { return !el.classList.contains('is-hidden'); }
        );
        group.classList.toggle('is-hidden', !anyVisible);
      });
      var empty = document.getElementById('library-empty');
      if (empty) {
        empty.hidden = !!document.querySelector('[data-case]:not(.is-hidden)');
      }
      syncURL();
    };

    // The counts are computed from the page rather than passed down, so they
    // cannot disagree with what is actually rendered.
    (function countOrigins() {
      var totals = { all: 0, builtin: 0, custom: 0 };
      document.querySelectorAll('[data-case]').forEach(function (el) {
        totals.all++;
        if (totals[el.dataset.origin] !== undefined) totals[el.dataset.origin]++;
      });
      document.querySelectorAll('[data-origin-count]').forEach(function (el) {
        var n = totals[el.dataset.originCount];
        el.textContent = n ? String(n) : '0';
      });
    })();

    var paintOrigin = function () {
      originBtns.forEach(function (b) {
        b.classList.toggle('is-active', b.dataset.origin === origin);
      });
    };
    originBtns.forEach(function (btn) {
      btn.addEventListener('click', function () {
        origin = btn.dataset.origin;
        paintOrigin();
        apply();
      });
    });
    paintOrigin();

    // Restore whatever the URL asked for before anything is drawn.
    apply();

    if (filter) {
      filter.addEventListener('input', apply);
      // "/" focuses the filter, as long as we are not already typing somewhere.
      document.addEventListener('keydown', function (e) {
        if (e.key === '/' && !isTypingTarget(e.target)) {
          e.preventDefault();
          filter.focus();
          filter.select();
        }
      });
    }
  }

  // --------------------------------------------------------- details menus
  // A <details> dropdown stays open until it is clicked again, which is not
  // what a menu should do. Clicking anywhere else, or pressing Escape, closes
  // it -- the behaviour every other menu on the page already has.
  document.addEventListener('click', function (e) {
    document.querySelectorAll('details.menu[open]').forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute('open');
    });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var open = document.querySelector('details.menu[open]');
    if (!open) return;
    e.stopPropagation();
    open.removeAttribute('open');
    var summary = open.querySelector('summary');
    if (summary) summary.focus();
  });

  // ------------------------------------------------- submit busy feedback
  // Starting a run can take minutes on the very first pull, so the button has
  // to say something.
  document.querySelectorAll('button[data-loading]').forEach(function (btn) {
    var form = btn.closest('form');
    if (!form) return;
    form.addEventListener('submit', function () {
      btn.disabled = true;
      btn.textContent = btn.dataset.loading;
    });
  });

  // ---------------------------------------------------------- generic tabs
  // Any [data-tabs] group drives the [data-panel] sections that follow it. The
  // active tab is mirrored into the URL hash so a reload or a shared link lands
  // on the same tab.
  document.querySelectorAll('[data-tabs]').forEach(function (group) {
    var name = group.dataset.tabs;
    // Scope to the matching panel container: the run page has its own
    // [data-panel] elements in the dock that this must not touch.
    var host = document.querySelector('[data-panels="' + name + '"]');
    var panels = host ? host.querySelectorAll('[data-panel]') : [];

    function activate(tab, pushHash) {
      group.querySelectorAll('[data-tab]').forEach(function (t) {
        t.classList.toggle('is-active', t.dataset.tab === tab);
      });
      panels.forEach(function (p) {
        p.classList.toggle('is-active', p.dataset.panel === tab);
      });
      if (pushHash) {
        history.replaceState(null, '', '#' + name + '=' + tab);
      }
      // A panel whose content is expensive can load itself on first reveal
      // rather than on page load.
      document.dispatchEvent(new CustomEvent('dl-tab', {
        detail: { group: name, tab: tab }
      }));
    }

    group.querySelectorAll('[data-tab]').forEach(function (t) {
      t.addEventListener('click', function () { activate(t.dataset.tab, true); });
    });

    var m = location.hash.match(new RegExp('#' + name + '=([\\w-]+)'));
    if (m) activate(m[1], false);
  });

  // ------------------------------------------------- sticky header elevation
  // The header only casts a shadow once there is content scrolled underneath
  // it, which keeps the page flat while it is at rest.
  document.querySelectorAll('.page-header-sticky').forEach(function (header) {
    var scroller = header.closest('.main') || document.documentElement;
    var update = function () {
      header.classList.toggle('is-stuck', scroller.scrollTop > 4);
    };
    scroller.addEventListener('scroll', update, { passive: true });
    update();

    // Anything else that wants to stick has to stick *below* this header, and
    // its height is not a constant: the lede wraps, the actions wrap, and some
    // pages put a tab bar in it. Publishing the measured height as a custom
    // property lets CSS position the rest without guessing.
    var publish = function () {
      scroller.style.setProperty('--sticky-top', header.offsetHeight + 'px');
    };
    publish();
    if (window.ResizeObserver) new ResizeObserver(publish).observe(header);
    else window.addEventListener('resize', publish);

    // A section heading only earns its separating hairline once something is
    // actually passing under it.
    var sections = scroller.querySelectorAll('.grid-heading');
    if (sections.length) {
      var mark = function () {
        var edge = header.getBoundingClientRect().bottom;
        sections.forEach(function (h) {
          h.classList.toggle('is-stuck', h.getBoundingClientRect().top <= edge + 1);
        });
      };
      scroller.addEventListener('scroll', mark, { passive: true });
      mark();
    }
  });

  function isTypingTarget(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }

  window.DL = window.DL || {};
  window.DL.isTypingTarget = isTypingTarget;

  // Small helpers shared by run.js and playground.js.
  window.DL.escapeHTML = function (s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  };

  // raw sends the body as-is rather than JSON-encoding it, for endpoints that
  // take a whole file.
  window.DL.postJSON = function (url, body, raw) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': raw ? 'text/plain' : 'application/json' },
      body: raw ? body : (body === undefined ? '{}' : JSON.stringify(body))
    }).then(function (r) { return r.json(); });
  };

  window.DL.formatTime = function (iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    var pad = function (n, w) { return String(n).padStart(w || 2, '0'); };
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) +
      '.' + pad(d.getMilliseconds(), 3);
  };
})();
