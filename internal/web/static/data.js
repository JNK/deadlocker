/* The data pane: the tables the locks are actually about.

   The Locks tab says a transaction holds X,GAP on PRIMARY before key 7. That
   sentence is unreadable until you can see that the table has rows 5 and 9 and
   nothing in between. So this shows the table in index order and draws each
   lock on the row it locks, or in the gap it closes.

   Several tables can be open at once, side by side: a foreign-key scenario is
   two tables or it is nothing. Every view re-reads itself after each step, so
   what is on screen is what the database looks like right now. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var fmtTime = window.DL.formatTime;

  var root = document.getElementById('run-root');
  var host = document.getElementById('data-views');
  if (!root || !host) return;

  var runID = root.dataset.run;
  var VIEW_KEY = 'dl-data-views';
  var MAX_VIEWS = 4;

  var tables = [];       // TableInfo[] from the server
  var views = [];        // { id, table, index, data, error, loading }
  var seq = 0;
  var loaded = false;    // have we got the table list yet
  var dirty = false;     // something changed while the panel was hidden

  // ------------------------------------------------------------- plumbing

  function panel() {
    return document.querySelector('.dock-panel[data-panel="data"]');
  }

  function visible() {
    var p = panel();
    if (!p || !p.classList.contains('is-active')) return false;
    var split = document.querySelector('.run-split');
    return !(split && split.classList.contains('dock-collapsed'));
  }

  function get(url) {
    return fetch(url).then(function (r) { return r.json(); });
  }

  function tableByName(name) {
    for (var i = 0; i < tables.length; i++) {
      if (tables[i].name === name) return tables[i];
    }
    return null;
  }

  // actorName resolves a lock's owner. The run page knows every session,
  // including the standalone ones opened from the console, so it answers rather
  // than this file keeping a second list that could disagree.
  function actorName(id) {
    if (window.DL.runActorName) return window.DL.runActorName(id);
    return id || 'another session';
  }

  // ---------------------------------------------------------- persistence
  //
  // Which tables you want open is a property of the scenario, not of one run of
  // it: the second run of a two-table deadlock wants the same two tables. They
  // are remembered by name and only restored where those names exist.

  function remember() {
    try {
      localStorage.setItem(VIEW_KEY, JSON.stringify(views.map(function (v) {
        return { table: v.table, index: v.index };
      })));
    } catch (e) {}
  }

  function remembered() {
    try {
      var raw = JSON.parse(localStorage.getItem(VIEW_KEY) || '[]');
      return Array.isArray(raw) ? raw : [];
    } catch (e) { return []; }
  }

  // ------------------------------------------------------------- loading

  function loadTables() {
    return get('/run/' + runID + '/tables').then(function (res) {
      if (!res.ok) {
        host.innerHTML = '<div class="dock-empty">' + esc(res.error) + '</div>';
        return false;
      }
      tables = res.tables || [];
      loaded = true;
      if (!tables.length) {
        host.innerHTML = '<div class="dock-empty">This scenario has no tables yet. ' +
          'They appear here once the schema has been applied.</div>';
        return false;
      }
      if (!views.length) restore();
      render();
      refreshAll();
      return true;
    }).catch(function (err) {
      host.innerHTML = '<div class="dock-empty">' + esc(String(err)) + '</div>';
      return false;
    });
  }

  // restore opens what was open last time, falling back to the first table so
  // the pane is never empty on arrival.
  function restore() {
    remembered().forEach(function (v) {
      if (views.length >= MAX_VIEWS) return;
      if (tableByName(v.table)) add(v.table, v.index, true);
    });
    if (!views.length) add(tables[0].name, '', true);
  }

  function add(table, index, quiet) {
    if (views.length >= MAX_VIEWS) return;
    var info = tableByName(table) || tables[0];
    if (!info) return;
    views.push({
      id: ++seq,
      table: info.name,
      index: index || (info.indexes && info.indexes.length ? info.indexes[0].name : ''),
      data: null, error: '', loading: true
    });
    remember();
    if (!quiet) {
      render();
      refresh(views[views.length - 1]);
    }
  }

  function remove(id) {
    views = views.filter(function (v) { return v.id !== id; });
    remember();
    render();
  }

  function viewByID(id) {
    for (var i = 0; i < views.length; i++) {
      if (views[i].id === id) return views[i];
    }
    return null;
  }

  function refresh(view) {
    if (!view) return Promise.resolve();
    view.loading = true;
    paintMeta(view);
    var url = '/run/' + runID + '/table?name=' + encodeURIComponent(view.table) +
      '&index=' + encodeURIComponent(view.index || '');
    return get(url).then(function (res) {
      view.loading = false;
      if (!res.ok) {
        view.error = res.error;
        view.data = null;
      } else {
        view.error = '';
        view.data = res.view;
        // The server settles which index was actually used; a stale name in the
        // picker would then point at a view it did not produce.
        if (res.view && res.view.index) view.index = res.view.index;
      }
      paintBody(view);
      paintMeta(view);
    }).catch(function (err) {
      view.loading = false;
      view.error = String(err);
      paintBody(view);
    });
  }

  function refreshAll() {
    return Promise.all(views.map(refresh));
  }

  // A burst of events -- a step publishes locks, then state, then locks again --
  // should be one read, not three.
  var pending = 0;
  function refreshSoon() {
    if (!visible()) { dirty = true; return; }
    clearTimeout(pending);
    pending = setTimeout(function () {
      if (loaded) refreshAll();
      else loadTables();
    }, 120);
  }

  // -------------------------------------------------------------- render

  function render() {
    if (!views.length) {
      host.innerHTML = '<div class="dock-empty">No table open. ' +
        'Press <strong>Add a table</strong> to watch one.</div>';
      paintToolbar();
      return;
    }
    host.innerHTML = views.map(viewShellHTML).join('');
    views.forEach(function (v) {
      paintBody(v);
      paintMeta(v);
    });
    paintToolbar();
  }

  function paintToolbar() {
    var addBtn = document.getElementById('data-add');
    if (addBtn) {
      addBtn.disabled = !loaded || views.length >= MAX_VIEWS;
      addBtn.title = views.length >= MAX_VIEWS
        ? 'Four tables side by side is the most that stays readable'
        : 'Open another table beside this one';
    }
  }

  function viewShellHTML(v) {
    var info = tableByName(v.table);
    var indexes = (info && info.indexes) || [];
    var html = '<section class="data-view" data-view="' + v.id + '">';
    html += '<header class="data-view-head">';
    html += '<select class="data-pick-table" data-view="' + v.id + '" title="Which table">' +
      tables.map(function (t) {
        return '<option value="' + esc(t.name) + '"' + (t.name === v.table ? ' selected' : '') +
          '>' + esc(t.name) + '</option>';
      }).join('') + '</select>';

    // The index is not decoration: InnoDB locks index records, so the same
    // statement takes different-looking locks in different indexes, and reading
    // the table in that index's order is what makes them line up.
    html += '<select class="data-pick-index" data-view="' + v.id + '" title="Read in the order of this index">' +
      indexes.map(function (idx) {
        return '<option value="' + esc(idx.name) + '"' + (idx.name === v.index ? ' selected' : '') +
          '>' + esc(idx.name) + ' (' + esc((idx.key_columns || idx.columns || []).join(', ')) + ')' +
          (idx.unique ? ' · unique' : '') + '</option>';
      }).join('') + '</select>';

    html += '<span class="data-view-meta mono" data-meta="' + v.id + '"></span>';
    html += '<button class="data-view-close" type="button" data-close="' + v.id + '" ' +
      'aria-label="Close this table">' +
      '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>' +
      '</button>';
    html += '</header>';
    html += '<div class="data-view-body" data-body="' + v.id + '"></div>';
    html += '</section>';
    return html;
  }

  function paintMeta(v) {
    var el = host.querySelector('[data-meta="' + v.id + '"]');
    if (!el) return;
    if (v.loading && !v.data) { el.textContent = 'reading…'; return; }
    var d = v.data;
    if (!d) { el.textContent = ''; return; }
    var count = d.exact ? d.row_count + (d.row_count === 1 ? ' row' : ' rows')
      : '≈' + d.row_count + ' rows';
    el.textContent = count + (d.truncated ? ' · first ' + d.rows.length : '');
  }

  function paintBody(v) {
    var el = host.querySelector('[data-body="' + v.id + '"]');
    if (!el) return;
    if (v.error) {
      el.innerHTML = '<div class="data-note is-error">' + esc(v.error) + '</div>';
      return;
    }
    var d = v.data;
    if (!d) {
      el.innerHTML = '<div class="data-note">Reading…</div>';
      return;
    }
    // Keep where the reader was. This repaints after every step, and a table
    // that jumps back to the top each time is one you cannot read the bottom of.
    var keep = el.scrollTop;
    el.innerHTML = tableHTML(d);
    el.scrollTop = keep;
  }

  // ---------------------------------------------------------- the table

  function tableHTML(d) {
    if (d.err) {
      return '<div class="data-note is-error">' + esc(d.err) + '</div>' + unmatchedHTML(d);
    }
    var cols = d.columns || [];
    var width = cols.length + 1;

    var html = '<table class="data data-table"><thead><tr>' +
      '<th class="data-gutter" title="Locks covering this row"></th>';
    cols.forEach(function (c) { html += '<th>' + esc(c) + '</th>'; });
    html += '</tr></thead><tbody>';

    (d.rows || []).forEach(function (row) {
      if (row.gap_locks && row.gap_locks.length) {
        html += gapRowHTML(row.gap_locks, width,
          'the gap before this row — an insert landing here waits');
      }
      var locked = row.locks && row.locks.length;
      html += '<tr class="data-row' + (locked ? ' is-locked' : '') + '">';
      html += '<td class="data-gutter">' + (locked ? badges(row.locks) : '') + '</td>';
      (row.values || []).forEach(function (val, i) {
        var isNull = row.null && row.null[i];
        html += '<td class="mono' + (isNull ? ' is-null' : '') + '">' +
          (isNull ? 'NULL' : esc(val)) + '</td>';
      });
      html += '</tr>';
    });

    // The gap above the highest key. It is the one every ascending key lands
    // in, so it deserves a line of its own rather than a footnote.
    if (d.supremum && d.supremum.length) {
      html += gapRowHTML(d.supremum, width,
        'the gap above the highest key — the supremum pseudo-record');
    }

    if (!d.rows || !d.rows.length) {
      html += '<tr><td class="data-empty" colspan="' + width + '">The table is empty.</td></tr>';
    }
    html += '</tbody></table>';

    if (d.truncated) {
      html += '<div class="data-note">Showing the first ' + d.rows.length + ' rows.</div>';
    }
    return html + unmatchedHTML(d);
  }

  function gapRowHTML(locks, width, label) {
    return '<tr class="data-gap"><td class="data-gutter"></td>' +
      '<td colspan="' + (width - 1) + '"><span class="data-gap-line" aria-hidden="true"></span>' +
      badges(locks) + '<span class="data-gap-label">' + esc(label) + '</span></td></tr>';
  }

  // unmatchedHTML explains the locks that could not be drawn on anything. They
  // are two different situations and saying so is the point: a key that is
  // locked but not in the table is usually an uncommitted insert -- exactly the
  // thing that is invisible here and blocking anyway -- while a lock in another
  // index is perfectly ordinary and one click away from being visible.
  function unmatchedHTML(d) {
    var html = '';
    if ((d.unmatched || []).length) {
      html += '<div class="data-note is-unmatched">' +
        '<strong>Locked, but not in the table</strong> ' +
        badges(d.unmatched) +
        '<span>A key this index has a lock on with no row to show: inserted and not ' +
        'yet committed, or already deleted. This read sees committed rows only.</span>' +
        '</div>';
    }
    if ((d.other_index || []).length) {
      var names = {};
      d.other_index.forEach(function (l) { if (l.index) names[l.index] = true; });
      html += '<div class="data-note is-unmatched">' +
        '<strong>Locks in another index</strong> ' +
        badges(d.other_index) +
        '<span>InnoDB locks index records, and these are on ' +
        esc(Object.keys(names).join(', ') || 'another index') +
        ' — switch the index above to see which rows they sit on.</span>' +
        '</div>';
    }
    return html;
  }

  function badges(locks) {
    return locks.map(function (l) {
      var title = actorName(l.actor) + ' · ' + l.status + ' · ' +
        (l.object ? l.object + '.' : '') + (l.index || '') +
        (l.data ? ' at ' + l.data : '') + '\n' + (l.explain || '');
      return '<span class="lock-mode ' + modeClass(l) +
        (l.status === 'WAITING' ? ' is-waiting' : '') + '" title="' + esc(title) + '">' +
        esc(l.mode) + '<span class="lock-mode-who">' + esc(actorName(l.actor)) + '</span></span>';
    }).join('');
  }

  // The same vocabulary the Locks tab uses, so a mode is the same colour
  // wherever it is shown.
  function modeClass(l) {
    switch (l.kind) {
      case 'insert-intention': return 'is-insert-intention';
      case 'gap': return 'is-gap';
      case 'record': return 'is-record';
      case 'table': return 'is-table';
      default: return 'is-nextkey';
    }
  }

  // -------------------------------------------------------------- events

  host.addEventListener('change', function (e) {
    var t = e.target;
    var id = Number(t.dataset.view);
    var v = viewByID(id);
    if (!v) return;
    if (t.classList.contains('data-pick-table')) {
      v.table = t.value;
      // A different table has different indexes, so the old choice is gone.
      var info = tableByName(v.table);
      v.index = info && info.indexes && info.indexes.length ? info.indexes[0].name : '';
      v.data = null;
      remember();
      render();
      refresh(v);
      return;
    }
    if (t.classList.contains('data-pick-index')) {
      v.index = t.value;
      remember();
      refresh(v);
    }
  });

  host.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-close]');
    if (btn) remove(Number(btn.dataset.close));
  });

  var addBtn = document.getElementById('data-add');
  if (addBtn) {
    addBtn.addEventListener('click', function () {
      if (!tables.length) return;
      // Default to a table that is not already open: two panes showing the same
      // table is a thing you can ask for, but never the thing you meant.
      var open = views.map(function (v) { return v.table; });
      var next = tables.filter(function (t) { return open.indexOf(t.name) < 0; })[0] || tables[0];
      add(next.name, '');
    });
  }

  var refreshBtn = document.getElementById('data-refresh');
  if (refreshBtn) {
    refreshBtn.addEventListener('click', function () {
      if (loaded) refreshAll(); else loadTables();
    });
  }

  // Every lock snapshot is published after a step, a console statement or the
  // Locks button, which is exactly when the data can have changed too.
  window.addEventListener('dl-locks', function () { refreshSoon(); });
  window.addEventListener('dl-run-state', function (e) {
    var st = e.detail;
    if (!loaded && st && st.status && st.status !== 'preparing') refreshSoon();
  });

  // The panel is only read when it is open; until then a refresh is deferred
  // rather than run, so stepping through a scenario with the pane closed costs
  // nothing.
  window.addEventListener('dl-dock-tab', function (e) {
    if (e.detail !== 'data') return;
    if (!loaded) { loadTables(); return; }
    if (dirty) { dirty = false; refreshAll(); }
  });

  window.DL.dataPane = { refresh: refreshSoon, loaded: function () { return loaded; } };
})();
