/* The command palette.

   Cmd/Ctrl-K anywhere. It searches everything addressable — scenarios by name,
   category, tag and description text, runs by id, analyses — and offers the
   handful of actions that are otherwise a click into a menu.

   Matching happens in the browser against an index fetched once. A local
   library is a few dozen entries, so a request per keystroke would buy nothing
   and cost the thing that matters most here: feeling instant. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  // Static destinations, always available and always searchable, so the palette
  // is useful even before the index has arrived.
  var COMMANDS = [
    { kind: 'go', title: 'Scenarios', subtitle: 'all cases', url: '/', terms: ['home', 'library', 'index'] },
    { kind: 'go', title: 'New scenario', subtitle: 'editor with a live step view', url: '/playground', terms: ['create', 'author', 'write', 'playground'] },
    { kind: 'go', title: 'Build with AI', subtitle: 'describe it in prose', url: '/builder', terms: ['ai', 'llm', 'chat', 'generate', 'assistant'] },
    { kind: 'go', title: 'Compare runs', subtitle: 'diff two runs step by step', url: '/compare', terms: ['diff', 'side by side'] },
    { kind: 'go', title: 'Settings', subtitle: 'model endpoint, MCP, history', url: '/settings', terms: ['config', 'llm', 'api key', 'mcp'] }
  ];

  var el = null, input = null, listEl = null, emptyEl = null;
  var items = COMMANDS.slice();
  var results = [];
  var cursor = 0;

  // ------------------------------------------------------------- matching

  // score ranks a candidate against the query. A word-boundary prefix beats a
  // mid-word hit, which beats a subsequence match, so typing "gap" puts "Gap
  // locks are precise" above a scenario that merely mentions gaps in prose.
  function score(item, q) {
    var title = item.title.toLowerCase();
    var hay = (item.title + ' ' + (item.subtitle || '') + ' ' +
      (item.detail || '') + ' ' + (item.terms || []).join(' ')).toLowerCase();

    if (title === q) return 1000;
    if (title.indexOf(q) === 0) return 900 - title.length;
    if (wordStart(title, q)) return 800 - title.length;
    if (title.indexOf(q) > 0) return 700 - title.length;
    if (wordStart(hay, q)) return 500;
    if (hay.indexOf(q) >= 0) return 400;
    // Subsequence: "dlk" finds "deadlock". Ranked last, because it matches
    // almost anything on a short query.
    if (subsequence(title, q)) return 200;
    if (subsequence(hay, q)) return 100;
    return -1;
  }

  function wordStart(hay, q) {
    var i = hay.indexOf(q);
    while (i >= 0) {
      if (i === 0 || /[\s\-_/·.]/.test(hay[i - 1])) return true;
      i = hay.indexOf(q, i + 1);
    }
    return false;
  }

  function subsequence(hay, q) {
    var i = 0;
    for (var j = 0; j < hay.length && i < q.length; j++) {
      if (hay[j] === q[i]) i++;
    }
    return i === q.length;
  }

  function search(list, q) {
    q = String(q || '').trim().toLowerCase();
    if (!q) {
      // With no query, show the commands and then whatever is most recent.
      return list.filter(function (it) { return it.kind === 'go'; })
        .concat(list.filter(function (it) { return it.kind !== 'go'; }).slice(0, 8));
    }
    var scored = [];
    list.forEach(function (it) {
      var s = score(it, q);
      if (s >= 0) scored.push({ item: it, score: s });
    });
    // Ties keep their original order, which is the library's own ordering
    // rather than something arbitrary.
    scored.sort(function (a, b) { return b.score - a.score; });
    return scored.slice(0, 40).map(function (s) { return s.item; });
  }

  // ------------------------------------------------------------ rendering

  var KIND_LABEL = {
    scenario: 'scenario', run: 'run', analysis: 'analysis', draft: 'draft', go: 'go to'
  };

  function render() {
    if (!results.length) {
      listEl.innerHTML = '';
      emptyEl.hidden = false;
      return;
    }
    emptyEl.hidden = true;
    listEl.innerHTML = results.map(function (it, i) {
      var line = '<div class="pal-item' + (i === cursor ? ' is-active' : '') +
        '" data-index="' + i + '" role="option" aria-selected="' + (i === cursor) + '">' +
        '<span class="pal-kind pal-kind-' + esc(it.kind) + '">' +
        esc(KIND_LABEL[it.kind] || it.kind) + '</span>' +
        '<span class="pal-body">' +
        '<span class="pal-title">' + esc(it.title) + '</span>';
      if (it.subtitle) line += '<span class="pal-sub">' + esc(it.subtitle) + '</span>';
      line += '</span>';
      if (it.detail) line += '<span class="pal-detail">' + esc(it.detail) + '</span>';
      return line + '</div>';
    }).join('');
    scrollCursorIntoView();
  }

  function scrollCursorIntoView() {
    var active = listEl.querySelector('.pal-item.is-active');
    if (!active) return;
    var box = listEl.getBoundingClientRect();
    var row = active.getBoundingClientRect();
    if (row.bottom > box.bottom) listEl.scrollTop += row.bottom - box.bottom;
    else if (row.top < box.top) listEl.scrollTop -= box.top - row.top;
  }

  function update() {
    results = search(items, input.value);
    cursor = 0;
    render();
  }

  // ---------------------------------------------------------------- shell

  function build() {
    if (el) return el;
    el = document.createElement('dialog');
    el.className = 'pal';
    el.innerHTML =
      '<div class="pal-inner">' +
      '<input class="pal-input" type="text" role="combobox" aria-expanded="true" ' +
      'aria-controls="pal-list" placeholder="Search scenarios, runs and actions…" ' +
      'spellcheck="false" autocomplete="off">' +
      '<div class="pal-list" id="pal-list" role="listbox"></div>' +
      '<div class="pal-empty" hidden>Nothing matches.</div>' +
      '<div class="pal-foot">' +
      '<span><kbd data-mod>⌘</kbd><kbd>K</kbd> toggle</span>' +
      '<span><kbd>↑</kbd><kbd>↓</kbd> move</span>' +
      '<span><kbd>Enter</kbd> open</span>' +
      '<span><kbd>Esc</kbd> close</span>' +
      '</div>' +
      '</div>';
    document.body.appendChild(el);

    input = el.querySelector('.pal-input');
    listEl = el.querySelector('.pal-list');
    emptyEl = el.querySelector('.pal-empty');

    input.addEventListener('input', update);
    input.addEventListener('keydown', onKey);

    listEl.addEventListener('mousemove', function (e) {
      var row = e.target.closest('[data-index]');
      if (!row) return;
      var i = Number(row.dataset.index);
      if (i === cursor) return;
      cursor = i;
      render();
    });
    listEl.addEventListener('click', function (e) {
      var row = e.target.closest('[data-index]');
      if (row) open(results[Number(row.dataset.index)]);
    });

    // Clicking the backdrop closes it. The dialog element fills the viewport,
    // so a click that lands on it rather than on the panel is a backdrop click.
    el.addEventListener('click', function (e) {
      if (e.target === el) close();
    });
    el.addEventListener('close', function () { input.value = ''; });
    return el;
  }

  function onKey(e) {
    if (e.key === 'ArrowDown' || (e.key === 'n' && e.ctrlKey)) {
      e.preventDefault();
      cursor = Math.min(cursor + 1, results.length - 1);
      render();
    } else if (e.key === 'ArrowUp' || (e.key === 'p' && e.ctrlKey)) {
      e.preventDefault();
      cursor = Math.max(cursor - 1, 0);
      render();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      open(results[cursor]);
    } else if (e.key === 'Home') {
      cursor = 0; render();
    } else if (e.key === 'End') {
      cursor = Math.max(results.length - 1, 0); render();
    }
  }

  function open(item) {
    if (!item) return;
    close();
    window.location.href = item.url;
  }

  function close() {
    if (el && el.open) el.close();
  }

  function load() {
    return fetch('/api/palette')
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (res) {
        if (!res || !res.ok) return;
        items = COMMANDS.concat(res.items || []);
        if (el && el.open) update();
      })
      .catch(function () {});
  }

  function show() {
    build();
    // Refetch on every open: scenarios and runs change while the page is up,
    // and the index is small enough that this is not worth caching harder.
    load();
    if (!el.open) el.showModal();
    input.value = '';
    update();
    input.focus();
  }

  document.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      if (el && el.open) close(); else show();
    }
  });

  // Anything that opens the palette by click, e.g. the sidebar's search row.
  document.addEventListener('click', function (e) {
    if (e.target.closest('[data-palette]')) {
      e.preventDefault();
      show();
    }
  });

  // The shortcut hint is written as ⌘K and corrected here rather than guessed
  // on the server, which cannot know what the reader is typing on.
  if (!/Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)) {
    document.querySelectorAll('[data-palette-key]').forEach(function (el) {
      if (el.title) el.title = el.title.replace('⌘K', 'Ctrl K');
    });
    document.querySelectorAll('.pal-foot [data-mod]').forEach(function (el) {
      el.textContent = 'Ctrl';
    });
  }

  window.DL = window.DL || {};
  window.DL.palette = show;
  // Pure, so the ranking can be tested without a browser.
  window.DL.paletteSearch = search;
})();
