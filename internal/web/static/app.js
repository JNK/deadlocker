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

  // ------------------------------------------------------- sidebar filter
  var filter = document.getElementById('case-filter');
  if (filter) {
    var apply = function () {
      var q = filter.value.trim().toLowerCase();
      document.querySelectorAll('[data-case]').forEach(function (el) {
        var hay = (el.dataset.name || el.textContent || '').toLowerCase();
        el.classList.toggle('is-hidden', q !== '' && hay.indexOf(q) === -1);
      });
      // Hide category headings that no longer have visible children.
      document.querySelectorAll('[data-group]').forEach(function (group) {
        var anyVisible = Array.prototype.some.call(
          group.querySelectorAll('[data-case]'),
          function (el) { return !el.classList.contains('is-hidden'); }
        );
        group.classList.toggle('is-hidden', !anyVisible);
      });
    };
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

  window.DL.postJSON = function (url, body) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? '{}' : JSON.stringify(body)
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
