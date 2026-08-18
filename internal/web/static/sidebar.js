/* Keeps the sidebar run list current.

   Runs can be started and stepped by an MCP client or by the assistant, not
   just by the person at the keyboard, so the list has to move on its own. It
   reconciles against the server by run id rather than replacing the markup:
   existing entries are updated in place and reordered by moving the nodes, so
   nothing flickers and a row being read does not vanish and reappear. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var host = document.getElementById('sidebar-history');
  if (!host) return;

  var pending = 0;

  function activeRunID() {
    var m = window.location.pathname.match(/^\/run\/([a-z0-9]+)/);
    return m ? m[1] : '';
  }

  function clockOf(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    var pad = function (n) { return String(n).padStart(2, '0'); };
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function chipHTML(e) {
    var meta = '<span class="mono">' + e.cursor + '/' + e.total + '</span>';
    if (e.live) meta += '<span class="run-chip-live">live</span>';
    // The outcome is a slug so it can be a class name; the label is the same
    // thing with the hyphen read as a space.
    else if (e.outcome) {
      meta += '<span class="outcome outcome-' + esc(e.outcome) + '">' +
        esc(e.outcome.replace(/-/g, ' ')) + '</span>';
    }
    meta += '<span class="run-chip-time">' + esc(clockOf(e.started_at)) + '</span>';

    return '<span class="dot status-' + esc(e.status) + '"></span>' +
      '<span class="run-chip-body">' +
      '<span class="run-chip-name">' + esc(e.case_name) + '</span>' +
      '<span class="run-chip-meta">' + meta + '</span>' +
      '</span>';
  }

  function classesFor(e, active) {
    return 'run-row' + (e.live ? ' is-live' : '') + (e.run_id === active ? ' is-active' : '');
  }

  var TRASH =
    '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" ' +
    'stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">' +
    '<path d="M4 6h16M9 6V4h6v2M7 6l1 14h8l1-14"/></svg>';

  function rowHTML(e) {
    return '<a class="run-chip" href="/run/' + esc(e.run_id) + '">' + chipHTML(e) + '</a>' +
      '<button class="run-forget" type="button" data-forget ' +
      'aria-label="Remove this run from the log" title="Remove from the log">' + TRASH + '</button>';
  }

  function reconcile(entries) {
    var active = activeRunID();
    var existing = {};
    host.querySelectorAll('[data-run-id]').forEach(function (el) {
      existing[el.dataset.runId] = el;
    });

    var ordered = entries.map(function (e) {
      var el = existing[e.run_id];
      if (el) {
        delete existing[e.run_id];
        // Only touch what changed, so a row the user is hovering stays put.
        var cls = classesFor(e, active);
        if (el.className !== cls) el.className = cls;
        // The signature covers only the chip, so the delete button is never
        // re-created underneath a pointer that is already on it.
        var next = chipHTML(e);
        if (el.dataset.sig !== next) {
          el.querySelector('.run-chip').innerHTML = next;
          el.dataset.sig = next;
        }
        if (e.live) el.dataset.live = '1'; else delete el.dataset.live;
        return el;
      }
      el = document.createElement('div');
      el.className = classesFor(e, active);
      el.dataset.runId = e.run_id;
      if (e.live) el.dataset.live = '1';
      el.innerHTML = rowHTML(e);
      el.dataset.sig = chipHTML(e);
      return el;
    });

    Object.keys(existing).forEach(function (id) { existing[id].remove(); });

    // Appending a node that is already in the DOM moves it, so reordering
    // reuses the elements rather than rebuilding them.
    var empty = host.querySelector('.sidebar-empty');
    if (empty && ordered.length) empty.remove();
    ordered.forEach(function (el) { host.appendChild(el); });

    if (!ordered.length && !host.querySelector('.sidebar-empty')) {
      var p = document.createElement('p');
      p.className = 'sidebar-empty';
      p.textContent = 'No runs yet. Open a scenario and press Run.';
      host.appendChild(p);
    }
    paintCount(ordered.length);
  }

  // ---------------------------------------------------------------- count

  var countEl = document.getElementById('runs-count');
  var clearBtn = document.getElementById('runs-clear');

  function paintCount(n) {
    if (countEl) countEl.textContent = n ? String(n) : '';
    if (clearBtn) clearBtn.hidden = !n;
  }

  // ------------------------------------------------------------- removal

  function forget(row) {
    var id = row.dataset.runId;
    if (!id) return;
    var go = function () {
      // Remove it locally first: the list is reconciled from the server anyway,
      // and waiting for a round trip to acknowledge a click reads as a miss.
      row.remove();
      paintCount(host.querySelectorAll('[data-run-id]').length);
      window.DL.postJSON('/api/runs/' + encodeURIComponent(id) + '/forget', {})
        .then(refresh)
        .catch(refresh);
    };

    // A finished run is one line in a log and costs nothing to lose. A live one
    // is a running container's worth of state, so that gets a question.
    if (!row.dataset.live) { go(); return; }
    window.DL.confirm({
      title: 'Close and remove this run?',
      body: 'It is still open. Removing it closes the connections and drops its ' +
        'scratch database.',
      confirm: 'Close and remove',
      cancel: 'Keep it',
      danger: true
    }).then(function (ok) { if (ok) go(); });
  }

  host.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-forget]');
    if (!btn) return;
    // The button sits inside the row, next to the link that fills it.
    e.preventDefault();
    e.stopPropagation();
    forget(btn.closest('[data-run-id]'));
  });

  if (clearBtn) {
    clearBtn.addEventListener('click', function () {
      window.DL.confirm({
        title: 'Clear the run log?',
        body: 'Every finished run is removed. Runs that are still open are kept.',
        confirm: 'Clear it',
        cancel: 'Cancel',
        danger: true
      }).then(function (ok) {
        if (!ok) return;
        window.DL.postJSON('/api/runs/clear', {}).then(refresh);
      });
    });
  }

  // Refreshes are coalesced: stepping a run emits an event per step, and one
  // fetch per burst is plenty.
  function refresh() {
    clearTimeout(pending);
    pending = setTimeout(function () {
      fetch('/api/runs')
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (res) { if (res && res.ok) reconcile(res.runs || []); })
        .catch(function () {});
    }, 250);
  }

  // ------------------------------------------------------------ analyses

  var jobsHost = document.getElementById('sidebar-analyses-list');
  var jobsWrap = document.getElementById('sidebar-analyses');
  var jobsPending = 0;

  function jobHTML(j) {
    var meta = j.status === 'running' ? (j.progress || 'running…') : j.status;
    return '<span class="job-kind">' + esc(j.kind === 'isolation-matrix' ? 'matrix' : 'shrink') + '</span>' +
      '<span class="job-body">' +
      '<span class="job-name">' + esc(j.name || j.scenario_id) + '</span>' +
      '<span class="job-meta">' + esc(meta) + '</span>' +
      '</span>';
  }

  function reconcileJobs(jobs) {
    if (!jobsHost) return;
    jobsWrap.hidden = !jobs.length;

    var existing = {};
    jobsHost.querySelectorAll('[data-job-id]').forEach(function (el) {
      existing[el.dataset.jobId] = el;
    });

    var ordered = jobs.map(function (j) {
      var el = existing[j.id];
      if (el) {
        delete existing[j.id];
        var cls = 'job-chip status-' + j.status;
        if (el.className !== cls) el.className = cls;
        var next = jobHTML(j);
        if (el.dataset.sig !== next) { el.innerHTML = next; el.dataset.sig = next; }
        return el;
      }
      el = document.createElement('a');
      el.className = 'job-chip status-' + j.status;
      el.dataset.jobId = j.id;
      el.href = '/analysis/' + j.id;
      el.innerHTML = jobHTML(j);
      el.dataset.sig = el.innerHTML;
      return el;
    });

    Object.keys(existing).forEach(function (id) { existing[id].remove(); });
    ordered.forEach(function (el) { jobsHost.appendChild(el); });
  }

  function refreshJobs() {
    clearTimeout(jobsPending);
    jobsPending = setTimeout(function () {
      fetch('/api/jobs')
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (res) { if (res && res.ok) reconcileJobs(res.jobs || []); })
        .catch(function () {});
    }, 250);
  }

  window.addEventListener('dl-activity', function (e) {
    var kind = e.detail && e.detail.kind;
    if (!kind) return;
    if (kind.indexOf('run.') === 0) refresh();
    if (kind.indexOf('analysis.') === 0) refreshJobs();
  });

  // An analysis reports progress without emitting an event per attempt, so it
  // is polled while one is running.
  setInterval(function () {
    if (jobsHost && jobsHost.querySelector('.job-chip.status-running')) refreshJobs();
  }, 2000);
  refreshJobs();

  // A live run's step counter advances without an activity event of its own on
  // every tick, so a slow poll keeps the counts honest while anything is live.
  setInterval(function () {
    if (host.querySelector('.run-chip.is-live')) refresh();
  }, 4000);

  // ------------------------------------------------------ scroll position
  //
  // Every page is a full navigation, so the sidebar is rebuilt from scratch and
  // lands back at the top. After scrolling down a long run log to reach an old
  // run, that throws away exactly the position you just worked for. The offset
  // is saved on the way out and restored on the way in.
  //
  // sessionStorage rather than localStorage: the scroll position is about the
  // journey through this tab, not a preference to carry into tomorrow.
  var SCROLL_KEY = 'dl-sidebar-scroll';
  // #sidebar-history is the element with overflow-y: auto — the sidebar itself
  // is a flex column that does not scroll.
  var scroller = host;

  function saveScroll() {
    try { sessionStorage.setItem(SCROLL_KEY, String(scroller.scrollTop)); } catch (e) {}
  }

  // Capture on the click that navigates, not on scroll: writing on every scroll
  // event would be noise, and a click is the only moment the value matters.
  document.addEventListener('click', function (e) {
    if (e.target.closest('.sidebar a[href]')) saveScroll();
  }, true);
  window.addEventListener('pagehide', saveScroll);

  (function restoreScroll() {
    var saved;
    try { saved = sessionStorage.getItem(SCROLL_KEY); } catch (e) { return; }
    if (!saved) return;
    var top = Number(saved);
    if (!top || isNaN(top)) return;
    // The list is server-rendered, so it is already the right height here. The
    // rAF is for the case where a stylesheet has not settled yet.
    scroller.scrollTop = top;
    requestAnimationFrame(function () {
      if (scroller.scrollTop !== top) scroller.scrollTop = top;
    });
  })();

  // Reconciling can change the list's height, which would otherwise nudge the
  // reader's position. Nothing to do about that beyond not making it worse:
  // entries are moved rather than rebuilt, so the scroll offset stays valid.
})();
