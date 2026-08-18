/* The Versions tab: a scenario's revision history.

   Scenarios are versioned the same way the configuration is — every save
   appends a revision — so an edit that turns out to be wrong can be undone
   without having reached for git. The list loads lazily, because most visits to
   a scenario page never open this tab. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var host = document.getElementById('versions-out');
  if (!host) return;

  var id = host.dataset.scenario;
  var loaded = false;
  var expanded = {};   // version -> YAML, so re-opening a preview is instant

  function stamp(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    return d.toLocaleString();
  }

  function rowHTML(v) {
    var meta = [v.lines + (v.lines === 1 ? ' line' : ' lines')];
    if (v.path) meta.push(v.path);

    // The badge shares a cell with the file details rather than taking one of
    // its own: an optional grid child means the current row has one more cell
    // than every other, which wrapped its buttons onto a second line.
    return '<div class="version-row' + (v.is_current ? ' is-current' : '') +
      '" data-version="' + v.version + '">' +
      '<div class="version-head">' +
      '<span class="version-num mono">v' + v.version + '</span>' +
      '<span class="version-when">' + esc(stamp(v.saved_at)) + '</span>' +
      '<span class="version-note">' + esc(v.note || 'saved') + '</span>' +
      '<span class="version-side">' +
      (v.is_current ? '<span class="version-current">on disk</span>' : '') +
      '<span class="version-meta mono">' + esc(meta.join(' · ')) + '</span>' +
      '</span>' +
      '<span class="version-actions">' +
      '<button class="btn btn-sm" type="button" data-preview="' + v.version + '">Preview</button>' +
      (v.is_current ? '' :
        '<button class="btn btn-sm" type="button" data-restore="' + v.version + '">Restore</button>') +
      '</span>' +
      '</div>' +
      '<pre class="yaml code-scroll version-source" hidden></pre>' +
      '</div>';
  }

  function render(versions) {
    if (!versions.length) {
      host.innerHTML = '<div class="dock-empty">No versions recorded yet.</div>';
      return;
    }
    host.innerHTML = '<div class="version-list">' + versions.map(rowHTML).join('') + '</div>';
  }

  function load() {
    host.innerHTML = '<div class="dock-empty">Loading…</div>';
    return fetch('/api/case/' + encodeURIComponent(id) + '/versions')
      .then(function (r) { return r.json(); })
      .then(function (res) {
        if (!res.ok) {
          host.innerHTML = '<div class="steps-warning">' + esc(res.error) + '</div>';
          return;
        }
        render(res.versions || []);
      })
      .catch(function (err) {
        host.innerHTML = '<div class="steps-warning">' + esc(String(err)) + '</div>';
      });
  }

  function showPreview(row, version) {
    var pre = row.querySelector('.version-source');
    if (!pre.hidden) { pre.hidden = true; return; }

    if (expanded[version] !== undefined) {
      paint(pre, expanded[version]);
      return;
    }
    pre.hidden = false;
    pre.textContent = 'Loading…';
    fetch('/api/case/' + encodeURIComponent(id) + '/versions/' + version)
      .then(function (r) { return r.json(); })
      .then(function (res) {
        if (!res.ok) { pre.textContent = res.error; return; }
        expanded[version] = res.yaml;
        paint(pre, res.yaml);
      });
  }

  function paint(pre, yaml) {
    pre.hidden = false;
    // The same highlighter the Source tab uses, so a preview and the live
    // source read identically.
    if (window.DL.highlightYAML) pre.innerHTML = window.DL.highlightYAML(yaml);
    else pre.textContent = yaml;
  }

  host.addEventListener('click', function (e) {
    var prev = e.target.closest('[data-preview]');
    if (prev) {
      showPreview(prev.closest('.version-row'), prev.dataset.preview);
      return;
    }
    var restore = e.target.closest('[data-restore]');
    if (!restore) return;

    var version = restore.dataset.restore;
    window.DL.confirm({
      title: 'Restore version ' + version + '?',
      body: 'The file is rewritten with what it said at that revision. The current ' +
        'state is kept as its own version, so this can be undone.',
      confirm: 'Restore it',
      cancel: 'Cancel'
    }).then(function (ok) {
      if (!ok) return;
      window.DL.postJSON('/api/case/' + encodeURIComponent(id) + '/versions/' + version + '/restore', {})
        .then(function (res) {
          if (!res.ok) {
            host.insertAdjacentHTML('afterbegin',
              '<div class="steps-warning">' + esc(res.error) + '</div>');
            return;
          }
          // The page is rendered from the file, so everything else on it —
          // sequence, source, actor list — is now stale.
          window.location.reload();
        });
    });
  });

  // The tab bar dispatches this when a panel becomes visible; loading here
  // rather than on page load keeps a scenario page cheap to open.
  function maybeLoad() {
    if (loaded) return;
    var panel = host.closest('.tab-panel');
    if (panel && !panel.classList.contains('is-active')) return;
    loaded = true;
    load();
  }

  document.addEventListener('dl-tab', maybeLoad);
  maybeLoad();

  // A scenario edited from the assistant or over MCP adds a revision, so the
  // list refreshes when one arrives — but only while the tab is actually open.
  window.addEventListener('dl-activity', function (e) {
    var kind = e.detail && e.detail.kind;
    if (!loaded || !kind || kind.indexOf('scenario.') !== 0) return;
    if (e.detail.scenario_id && e.detail.scenario_id !== id) return;
    load();
  });
})();
