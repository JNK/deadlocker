/* Live behaviour for the run page.
   The page is server-rendered; this script subscribes to the run's event
   stream and patches the parts that change. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var fmtTime = window.DL.formatTime;
  var postJSON = window.DL.postJSON;

  var root = document.getElementById('run-root');
  if (!root) return;
  var runID = root.dataset.run;

  var state = {
    run: JSON.parse(document.getElementById('run-bootstrap').textContent),
    steps: JSON.parse(document.getElementById('steps-bootstrap').textContent) || [],
    locks: null,
    lockDelta: null,
    wire: [],
    docker: [],
    activity: [],
    selected: null,
    lastSeq: 0
  };

  var stepsByIndex = {};
  state.steps.forEach(function (s) { stepsByIndex[s.index] = s; });

  // ------------------------------------------------------------ rendering

  function statusOf(step) { return step.status || 'pending'; }

  function renderStepCard(step) {
    var card = document.querySelector('[data-step-card="' + step.index + '"]');
    if (!card) return;

    card.className = card.className
      .replace(/\bstatus-[\w-]+/g, '')
      .replace(/\s+/g, ' ')
      .trim() + ' status-' + statusOf(step);
    if (state.selected === step.index) card.classList.add('is-selected');

    var badge = card.querySelector('[data-field="status"]');
    if (badge) badge.textContent = statusOf(step);

    var foot = card.querySelector('[data-field="foot"]');
    if (!foot) return;
    var bits = [];

    if (step.status === 'done' || step.status === 'error') {
      bits.push('<span class="mini mini-mono">' + step.duration_ms + ' ms</span>');
    }
    if (step.status === 'done') {
      if (step.row_count) {
        bits.push('<span class="mini mini-ok">' + step.row_count + ' row' + (step.row_count === 1 ? '' : 's') + '</span>');
      } else if (step.rows_affected) {
        bits.push('<span class="mini mini-ok">' + step.rows_affected + ' affected</span>');
      } else {
        bits.push('<span class="mini mini-ok">ok</span>');
      }
    }
    if (step.status === 'blocked') {
      var by = (step.blocked_by || []).map(actorName).join(', ');
      bits.push('<span class="mini mini-warn">waiting' + (by ? ' on ' + esc(by) : '') + '</span>');
      // How long this statement has before innodb_lock_wait_timeout ends it.
      var deadline = timeoutDeadline(step);
      if (deadline) {
        bits.push('<span class="mini mini-countdown" data-countdown="' + deadline + '"></span>');
      }
    } else if (step.was_blocked) {
      bits.push('<span class="mini mini-warn" title="This statement waited on a lock before it finished">was blocked</span>');
    }
    if (step.status === 'error' && step.error) {
      var cls = step.error.kind === 'deadlock' ? 'mini-danger'
        : step.error.kind === 'timeout' ? 'mini-warn' : 'mini-danger';
      bits.push('<span class="mini ' + cls + '">' + esc(errorLabel(step.error)) + '</span>');
    }
    if (step.verdict === 'match') {
      bits.push('<span class="mini mini-ok" title="Matches the scenario\'s expectation">✓ as expected</span>');
    } else if (step.verdict === 'mismatch') {
      bits.push('<span class="mini mini-danger" title="' + esc(step.verdict_note || '') + '">✕ ' + esc(step.verdict_note || 'unexpected') + '</span>');
    }
    foot.innerHTML = bits.join('');
  }

  // timeoutDeadline is when innodb_lock_wait_timeout will fire for a statement
  // that is currently waiting, in epoch milliseconds.
  function timeoutDeadline(step) {
    var secs = state.run && state.run.lock_wait_timeout;
    if (!secs || !step.submitted_at) return 0;
    var started = new Date(step.submitted_at).getTime();
    if (isNaN(started)) return 0;
    return started + secs * 1000;
  }

  // A single ticker drives every countdown on the page.
  setInterval(function () {
    document.querySelectorAll('[data-countdown]').forEach(function (el) {
      var left = Number(el.dataset.countdown) - Date.now();
      if (left <= 0) {
        el.textContent = 'timeout due';
        el.classList.add('is-expired');
        return;
      }
      el.textContent = formatCountdown(left) + ' to timeout';
      el.classList.toggle('is-urgent', left < 10000);
    });
  }, 500);

  function formatCountdown(ms) {
    var s = Math.ceil(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
  }

  function errorLabel(err) {
    if (err.kind === 'deadlock') return 'deadlock victim (1213)';
    if (err.kind === 'timeout') return 'lock wait timeout (1205)';
    return 'error ' + err.code;
  }

  function renderAllSteps() {
    state.steps.forEach(renderStepCard);
  }

  function selectStep(index, reveal) {
    state.selected = index;
    document.querySelectorAll('.step-card').forEach(function (c) {
      c.classList.toggle('is-selected', Number(c.dataset.stepCard) === index);
    });
    renderStepDetail();
    var card = document.querySelector('[data-step-card="' + index + '"]');
    if (card) revealCard(card, reveal);
  }

  // revealCard scrolls a step into view leaving breathing room underneath, so a
  // freshly submitted step never sits flush against the bottom of the lane area
  // with the next one hidden behind the dock.
  var REVEAL_MARGIN = 96;

  function revealCard(card, generous) {
    var wrap = document.getElementById('lanes-wrap');
    var wrapBox = wrap.getBoundingClientRect();
    var cardBox = card.getBoundingClientRect();
    var margin = generous ? REVEAL_MARGIN : 16;

    var below = cardBox.bottom + margin - wrapBox.bottom;
    var above = wrapBox.top - cardBox.top + 16;

    if (below > 0) {
      wrap.scrollTo({ top: wrap.scrollTop + below, behavior: 'smooth' });
    } else if (above > 0) {
      wrap.scrollTo({ top: wrap.scrollTop - above, behavior: 'smooth' });
    }
  }

  function renderStepDetail() {
    var empty = document.getElementById('step-empty');
    var host = document.getElementById('step-detail');
    var step = stepsByIndex[state.selected];
    if (!step) {
      empty.hidden = false;
      host.hidden = true;
      return;
    }
    empty.hidden = true;
    host.hidden = false;

    var html = '';
    html += '<div class="detail-head">';
    html += '<span class="step-status">' + esc(statusOf(step)) + '</span>';
    html += '<h3>Step ' + step.index + ' · ' + esc(step.label) + '</h3>';
    html += '<span class="chip">' + esc(step.actor_name) + '</span>';
    if (step.expect) html += '<span class="chip chip-expect">expects ' + esc(step.expect) + '</span>';
    html += '</div>';

    html += '<pre class="sql">' + esc(step.sql) + '</pre>';
    if (step.note) html += '<div class="callout">' + esc(step.note) + '</div>';

    if (step.verdict === 'mismatch') {
      html += '<div class="callout callout-danger"><h4>Expectation not met</h4>' +
        esc(step.verdict_note) + '</div>';
    } else if (step.verdict === 'match') {
      html += '<div class="callout callout-ok"><h4>Expectation met</h4>Observed <code>' +
        esc(step.actual) + '</code>, as the scenario predicted.</div>';
    }

    if (step.status === 'blocked') {
      var dl = timeoutDeadline(step);
      if (dl) {
        html += '<div class="callout callout-warn"><h4>Lock wait timeout</h4>' +
          'This statement will fail with error 1205 in ' +
          '<strong data-countdown="' + dl + '"></strong> unless the lock is released first. ' +
          'innodb_lock_wait_timeout is ' + esc(String(state.run.lock_wait_timeout)) + 's for this run.' +
          '</div>';
      }
      html += '<div class="callout callout-warn"><h4>Blocked</h4>' +
        (step.wait_explain
          ? esc(step.wait_explain)
          : 'The statement has not returned yet. It is waiting on a lock held by another transaction.') +
        '</div>';
    }

    if (step.error) {
      var kindClass = step.error.kind === 'timeout' ? 'callout-warn' : 'callout-danger';
      html += '<div class="callout ' + kindClass + '"><h4>' + esc(errorLabel(step.error)) + '</h4>' +
        '<code>' + esc(step.error.message) + '</code>';
      if (step.error.sql_state) html += '<div class="muted">SQLSTATE ' + esc(step.error.sql_state) + '</div>';
      if (step.error.hint) html += '<p>' + esc(step.error.hint) + '</p>';
      html += '</div>';
    }

    html += '<div class="detail-grid">';
    html += stat('Status', statusOf(step));
    if (step.duration_ms != null && step.status !== 'pending') html += stat('Duration', step.duration_ms + ' ms');
    if (step.row_count) html += stat('Rows returned', step.row_count);
    if (step.rows_affected) html += stat('Rows affected', step.rows_affected);
    if (step.last_insert_id) html += stat('Last insert id', step.last_insert_id);
    html += '</div>';

    if (step.plan && step.plan.length) {
      html += '<div class="panel-subhead">Query plan</div>';
      html += '<div class="table-wrap"><table class="plan-table"><thead><tr>' +
        '<th>table</th><th>access</th><th>key</th><th>rows</th><th>extra</th>' +
        '</tr></thead><tbody>';
      step.plan.forEach(function (r) {
        html += '<tr>' +
          '<td>' + esc(r.table || '—') + '</td>' +
          '<td><span class="plan-type ' + planClass(r.type) + '">' + esc(r.type || '?') + '</span></td>' +
          '<td>' + esc(r.key && r.key !== 'NULL' ? r.key : '—') + '</td>' +
          '<td>' + esc(String(r.rows)) + '</td>' +
          '<td>' + esc(r.extra || '') + '</td>' +
          '</tr>';
      });
      html += '</tbody></table></div>';
      step.plan.forEach(function (r) {
        if (r.explain) html += '<div class="plan-explain">' + esc(r.explain) + '</div>';
      });
    }

    if (step.columns && step.columns.length && step.rows) {
      html += '<div class="table-wrap"><table class="data"><thead><tr>';
      step.columns.forEach(function (c) { html += '<th>' + esc(c) + '</th>'; });
      html += '</tr></thead><tbody>';
      step.rows.forEach(function (row) {
        html += '<tr>';
        row.forEach(function (v) { html += '<td class="mono">' + esc(v) + '</td>'; });
        html += '</tr>';
      });
      html += '</tbody></table></div>';
      if (step.rows_truncated) {
        html += '<p class="muted">Showing the first ' + step.rows.length + ' of ' + step.row_count + ' rows.</p>';
      }
      if (!step.rows.length) html += '<p class="muted">The statement returned no rows.</p>';
    }

    host.innerHTML = html;
  }

  // planClass colours the access path: a full scan is the finding, a unique
  // lookup is the benign case.
  function planClass(type) {
    switch (String(type || '').toLowerCase()) {
      case 'all': case 'index': return 'is-scan';
      case 'range': case 'ref': case 'ref_or_null': case 'index_merge': return 'is-range';
      case 'eq_ref': case 'const': case 'system': return 'is-unique';
      default: return '';
    }
  }

  function stat(label, value) {
    return '<div class="stat"><dt>' + esc(label) + '</dt><dd>' + esc(value) + '</dd></div>';
  }

  // ---------------------------------------------------------------- locks

  function lockModeClass(lock) {
    var m = (lock.lock_mode || '').toUpperCase();
    if ((lock.lock_type || '').toUpperCase() === 'TABLE') return 'is-table';
    if (m.indexOf('INSERT_INTENTION') >= 0) return 'is-insert-intention';
    if (m.indexOf('GAP') >= 0) return 'is-gap';
    if (m.indexOf('REC_NOT_GAP') >= 0) return 'is-record';
    return 'is-nextkey';
  }

  // renderWaitGraph draws the wait-for graph: one node per actor, one arrow per
  // wait edge. A cycle in this graph is precisely what InnoDB's deadlock
  // detector looks for, so when one closes it is worth showing loudly.
  function renderWaitGraph(snap) {
    var host = document.getElementById('wait-graph');
    var waits = (snap && snap.waits) || [];
    if (!waits.length) {
      host.innerHTML = '';
      host.classList.remove('has-cycle');
      return;
    }

    var actors = (state.run.actors || []);
    var involved = [];
    var seen = {};
    // Keep declaration order so the graph does not reshuffle between snapshots.
    actors.forEach(function (a) {
      var used = waits.some(function (w) {
        return w.waiting_actor === a.id || w.blocking_actor === a.id;
      });
      if (used && !seen[a.id]) { seen[a.id] = true; involved.push(a); }
    });
    if (involved.length < 2) { host.innerHTML = ''; return; }

    var cycle = hasCycle(waits);
    host.classList.toggle('has-cycle', cycle);

    var nodeW = 132, nodeH = 38, gapX = 96;
    var padX = 18, padY = cycle ? 62 : 54;
    var width = padX * 2 + involved.length * nodeW + (involved.length - 1) * gapX;
    var height = padY + nodeH + 26;
    var index = {};
    involved.forEach(function (a, i) { index[a.id] = i; });

    function cx(i) { return padX + i * (nodeW + gapX) + nodeW / 2; }

    var svg = '<svg viewBox="0 0 ' + width + ' ' + height + '" width="' + width + '" height="' + height + '" role="img">';
    svg += '<defs>' +
      '<marker id="wf-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">' +
      '<path d="M0 0 L10 5 L0 10 z" fill="currentColor"/></marker></defs>';

    // Edges first so nodes paint over the arrow ends.
    waits.forEach(function (w, k) {
      var from = index[w.waiting_actor], to = index[w.blocking_actor];
      if (from === undefined || to === undefined || from === to) return;
      var x1 = cx(from), x2 = cx(to);
      var forward = x2 > x1;
      // Arcs above for one direction and below for the other keeps the two
      // edges of a deadlock cycle from overlapping.
      var y = padY;
      var lift = forward ? -30 : 30;
      var mx = (x1 + x2) / 2;
      var edgeCls = 'wf-edge' + (cycle ? ' is-cycle' : '');
      var sx = x1 + (forward ? nodeW / 2 - 4 : -nodeW / 2 + 4);
      var ex = x2 + (forward ? -nodeW / 2 + 4 : nodeW / 2 - 4);
      svg += '<path class="' + edgeCls + '" d="M' + sx + ' ' + y + ' Q' + mx + ' ' + (y + lift) + ' ' + ex + ' ' + y + '" ' +
        'fill="none" marker-end="url(#wf-arrow)"/>';

      var mode = w.waiting_lock ? w.waiting_lock.lock_mode : 'waiting';
      svg += '<text class="wf-edge-label" x="' + mx + '" y="' + (y + lift * 0.72) + '" text-anchor="middle">' +
        esc(mode) + '</text>';
    });

    involved.forEach(function (a, i) {
      var x = padX + i * (nodeW + gapX);
      svg += '<g class="wf-node accent-' + esc(a.accent) + '">' +
        '<rect x="' + x + '" y="' + (padY - nodeH / 2) + '" width="' + nodeW + '" height="' + nodeH + '" rx="9"/>' +
        '<text x="' + (x + nodeW / 2) + '" y="' + (padY + 1) + '" text-anchor="middle">' + esc(a.name) + '</text>' +
        '</g>';
    });

    svg += '</svg>';

    var caption = cycle
      ? '<div class="wf-caption is-cycle">Cycle detected — each transaction is waiting for a lock the other holds. This is what InnoDB rolls one of them back for.</div>'
      : '<div class="wf-caption">Arrows point from the waiting transaction to the one holding the lock.</div>';

    host.innerHTML = '<div class="wf-canvas">' + svg + '</div>' + caption;
  }

  // hasCycle walks the wait-for edges looking for a loop.
  function hasCycle(waits) {
    var next = {};
    waits.forEach(function (w) {
      if (!w.waiting_actor || !w.blocking_actor) return;
      (next[w.waiting_actor] = next[w.waiting_actor] || []).push(w.blocking_actor);
    });
    var state = {}; // 1 = on the current path, 2 = fully explored
    var found = false;

    function visit(node) {
      if (found) return;
      if (state[node] === 1) { found = true; return; }
      if (state[node] === 2) return;
      state[node] = 1;
      (next[node] || []).forEach(visit);
      state[node] = 2;
    }
    Object.keys(next).forEach(visit);
    return found;
  }

  // ------------------------------------------------------------ lock diff
  //
  // A snapshot answers "what is held now". The question a reader actually has
  // while stepping is "what did that statement just do", and answering it from
  // two tables side by side is work the machine should do.
  //
  // Locks are identified by actor, table, index, mode and key rather than by
  // lock_id, which is not stable across snapshots.
  function lockKey(l) {
    return [l.actor, l.table, l.index, l.lock_mode, l.lock_data].join('\u0001');
  }

  function diffLocks(before, after) {
    var was = {}, now = {};
    (before && before.locks || []).forEach(function (l) { was[lockKey(l)] = l; });
    (after && after.locks || []).forEach(function (l) { now[lockKey(l)] = l; });

    var added = [], released = [], changed = [];
    Object.keys(now).forEach(function (k) {
      if (!was[k]) added.push(now[k]);
      else if (was[k].lock_status !== now[k].lock_status) {
        changed.push({ from: was[k], to: now[k] });
      }
    });
    Object.keys(was).forEach(function (k) {
      if (!now[k]) released.push(was[k]);
    });
    return { added: added, released: released, changed: changed };
  }

  function lockLine(l) {
    return '<span class="lock-mode ' + lockModeClass(l) + '">' + esc(l.lock_mode) + '</span> ' +
      esc(actorName(l.actor)) + ' on <code>' + esc(l.table) +
      (l.index ? '.' + esc(l.index) : '') + '</code>' +
      (l.lock_data && l.lock_data !== '—' ? ' at <code>' + esc(l.lock_data) + '</code>' : '');
  }

  function renderLockDelta() {
    var host = document.getElementById('locks-delta');
    if (!host) return;
    var d = state.lockDelta;
    if (!d || (!d.added.length && !d.released.length && !d.changed.length)) {
      host.innerHTML = '';
      return;
    }
    var rows = '';
    d.added.forEach(function (l) {
      rows += '<div class="delta-row is-added"><span class="delta-mark">+</span>' + lockLine(l) + '</div>';
    });
    d.changed.forEach(function (c) {
      rows += '<div class="delta-row is-changed"><span class="delta-mark">~</span>' +
        lockLine(c.to) + ' — ' + esc(c.from.lock_status) + ' → ' + esc(c.to.lock_status) + '</div>';
    });
    d.released.forEach(function (l) {
      rows += '<div class="delta-row is-released"><span class="delta-mark">−</span>' + lockLine(l) + '</div>';
    });
    host.innerHTML = '<div class="panel-subhead">What changed since the previous snapshot</div>' +
      '<div class="delta-list">' + rows + '</div>';
  }

  // setSnapshot records a new lock snapshot and the delta from the last one.
  function setSnapshot(snap) {
    if (!snap) return;
    state.lockDelta = state.locks ? diffLocks(state.locks, snap) : null;
    state.locks = snap;
  }

  function renderLocks() {
    var snap = state.locks;
    var timeEl = document.getElementById('locks-time');
    var waitsEl = document.getElementById('locks-waits');
    var tableEl = document.getElementById('locks-table');
    if (!snap) return;

    timeEl.textContent = 'snapshot at ' + fmtTime(snap.at);
    renderWaitGraph(snap);
    renderLockDelta();
    renderTransactions(snap);

    var waits = snap.waits || [];
    if (!waits.length) {
      waitsEl.innerHTML = '';
    } else {
      waitsEl.innerHTML = waits.map(function (w) {
        var detail = '';
        if (w.waiting_lock) {
          detail += 'requesting <span class="lock-mode ' + lockModeClass(w.waiting_lock) + '">' +
            esc(w.waiting_lock.lock_mode) + '</span> on <code>' + esc(w.waiting_lock.table) + '.' +
            esc(w.waiting_lock.index) + '</code>' +
            (w.waiting_lock.lock_data ? ' at <code>' + esc(w.waiting_lock.lock_data) + '</code>' : '');
        }
        if (w.blocking_lock) {
          detail += '<br>blocked by <span class="lock-mode ' + lockModeClass(w.blocking_lock) + '">' +
            esc(w.blocking_lock.lock_mode) + '</span>' +
            (w.blocking_lock.lock_data ? ' at <code>' + esc(w.blocking_lock.lock_data) + '</code>' : '');
        }
        return '<div class="wait-card">' +
          '<span class="wait-actor">' + esc(actorName(w.waiting_actor)) + '</span>' +
          '<span class="wait-arrow">waits for →</span>' +
          '<span class="wait-actor">' + esc(actorName(w.blocking_actor)) + '</span>' +
          '<span class="wait-detail">' + detail + '</span>' +
          '</div>';
      }).join('');
    }

    var mdlEl = document.getElementById('locks-mdl');
    var mdl = snap.metadata_locks || [];
    if (!mdl.length) {
      mdlEl.innerHTML = '';
    } else {
      mdlEl.innerHTML = '<div class="panel-subhead">Metadata locks</div>' +
        '<div class="table-wrap">' + mdl.map(function (m) {
          return '<div class="mdl-row' + (m.status === 'PENDING' ? ' is-pending' : '') + '">' +
            '<span>' + esc(actorName(m.actor)) + '</span>' +
            '<span class="mdl-type">' + esc(m.lock_type) + '</span>' +
            '<span class="mdl-explain">' + esc(m.explain || '') + '</span>' +
            '<span class="mono">' + esc(m.name || m.object_type) + ' · ' + esc(m.status) + '</span>' +
            '</div>';
        }).join('') + '</div>';
    }

    var showGranted = document.getElementById('locks-granted').checked;
    var locks = (snap.locks || []).filter(function (l) {
      return showGranted || l.lock_status !== 'GRANTED';
    });

    if (!locks.length) {
      tableEl.innerHTML = '<div class="dock-empty">No row locks are held right now.' +
        (mdl.length ? ' The metadata locks above are what is blocking.' : '') + '</div>';
      setCount('locks', (snap.locks || []).length);
      return;
    }

    var html = '<div class="table-wrap"><table class="data"><thead><tr>' +
      '<th>Actor</th><th>Table</th><th>Index</th><th>Mode</th><th>Status</th><th>Key</th><th>What it means</th>' +
      '</tr></thead><tbody>';
    locks.forEach(function (l) {
      html += '<tr' + (l.lock_status === 'WAITING' ? ' class="is-waiting"' : '') + '>' +
        '<td>' + esc(actorName(l.actor)) + '</td>' +
        '<td class="mono">' + esc(l.table) + '</td>' +
        '<td class="mono">' + esc(l.index || '—') + '</td>' +
        '<td><span class="lock-mode ' + lockModeClass(l) + '">' + esc(l.lock_mode) + '</span></td>' +
        '<td class="mono">' + esc(l.lock_status) + '</td>' +
        '<td class="mono">' + esc(l.lock_data || '—') + '</td>' +
        '<td>' + esc(l.explain) + '</td>' +
        '</tr>';
    });
    html += '</tbody></table></div>';
    tableEl.innerHTML = html;
    setCount('locks', (snap.locks || []).length);
  }

  // --------------------------------------------------- transaction inspector
  //
  // The weights InnoDB uses to pick a deadlock victim are readable, and without
  // them "which transaction gets rolled back" is folklore. Roughly, the cheaper
  // transaction to undo loses — so rows modified is the number to watch.
  function renderTransactions(snap) {
    var host = document.getElementById('locks-trx');
    if (!host) return;
    var trx = (snap && snap.transactions) || [];
    if (!trx.length) { host.innerHTML = ''; return; }

    var html = '<div class="panel-subhead">Transactions</div>' +
      '<div class="table-wrap"><table class="data"><thead><tr>' +
      '<th>Actor</th><th>State</th><th>Isolation</th>' +
      '<th title="performance_schema reports this as trx_rows_locked">Rows locked</th>' +
      '<th title="Roughly what decides the deadlock victim: the cheaper transaction to undo loses">Rows modified</th>' +
      '<th>Waiting since</th></tr></thead><tbody>';
    trx.forEach(function (t) {
      html += '<tr' + (t.wait_started ? ' class="is-waiting"' : '') + '>' +
        '<td>' + esc(actorName(t.actor)) + '</td>' +
        '<td class="mono">' + esc(t.state || '—') + '</td>' +
        '<td class="mono">' + esc(t.isolation_level || '—') + '</td>' +
        '<td class="mono">' + t.rows_locked + '</td>' +
        '<td class="mono">' + t.rows_modified + '</td>' +
        '<td class="mono">' + esc(t.wait_started || '—') + '</td>' +
        '</tr>';
    });
    host.innerHTML = html + '</tbody></table></div>';
  }

  function actorName(id) {
    if (!id) return 'another session';
    var a = (state.run.actors || []).filter(function (x) { return x.id === id; })[0];
    return a ? a.name : id;
  }

  // ----------------------------------------------------------------- wire

  var wireList = document.getElementById('wire-list');
  var MAX_WIRE_ROWS = 2500;

  function wirePasses(ev) {
    var actor = document.getElementById('wire-actor').value;
    var step = document.getElementById('wire-step').value;
    if (actor && ev.actor !== actor) return false;
    if (step !== '' && String(ev.step_index) !== step) return false;
    return true;
  }

  function wireRowHTML(ev) {
    var showHex = document.getElementById('wire-hex').checked;
    var cls = 'wire-row dir-' + (ev.direction || '');
    if (ev.kind === 'ERR' || ev.kind === 'ProxyError') cls += ' is-err';
    var arrow = ev.direction === 'c2s' ? '→' : ev.direction === 's2c' ? '←' : '·';

    var html = '<div class="' + cls + '">';
    html += '<span class="wire-time">' + fmtTime(ev.at) + '</span>';
    html += '<span class="wire-arrow">' + arrow + '</span>';
    html += '<span class="wire-kind">' + esc(ev.kind) + '</span>';
    html += '<span class="wire-summary">' + esc(ev.summary || '');
    if (ev.protocol === 'binary') {
      html += ' <span class="wire-flags wire-proto">binary</span>';
    }
    if (ev.status_flags && ev.status_flags.length) {
      html += ' <span class="wire-flags">[' + esc(ev.status_flags.join(' ')) + ']</span>';
    }
    if (ev.info) html += ' <span class="wire-flags">' + esc(ev.info) + '</span>';
    if (ev.decode_err) html += ' <span class="wire-flags">decode: ' + esc(ev.decode_err) + '</span>';
    html += '</span>';
    html += '<span class="wire-bytes">' + (ev.bytes || 0) + ' B</span>';
    if (showHex && ev.hex) {
      html += '<div class="wire-hex">' + esc(formatHex(ev.hex)) + (ev.truncated ? '\n… truncated' : '') + '</div>';
    }
    html += '</div>';
    return html;
  }

  function formatHex(hex) {
    var out = [];
    for (var i = 0; i < hex.length; i += 2) out.push(hex.substr(i, 2));
    var lines = [];
    for (var j = 0; j < out.length; j += 16) lines.push(out.slice(j, j + 16).join(' '));
    return lines.join('\n');
  }

  function renderWireAll() {
    var rows = state.wire.filter(wirePasses).slice(-MAX_WIRE_ROWS);
    wireList.innerHTML = rows.length
      ? rows.map(wireRowHTML).join('')
      : '<div class="dock-empty">No packets captured for this filter yet.</div>';
    wireList.scrollTop = wireList.scrollHeight;
  }

  function appendWire(ev) {
    state.wire.push(ev);
    if (state.wire.length > 20000) state.wire.splice(0, 5000);
    setCount('wire', state.wire.length);
    if (!wirePasses(ev)) return;
    if (wireList.firstElementChild && wireList.firstElementChild.classList.contains('dock-empty')) {
      wireList.innerHTML = '';
    }
    var atBottom = wireList.scrollHeight - wireList.scrollTop - wireList.clientHeight < 60;
    wireList.insertAdjacentHTML('beforeend', wireRowHTML(ev));
    while (wireList.childElementCount > MAX_WIRE_ROWS) wireList.removeChild(wireList.firstElementChild);
    if (atBottom) wireList.scrollTop = wireList.scrollHeight;
  }

  // ------------------------------------------------------ logs & activity

  var dockerEl = document.getElementById('docker-log');
  var activityEl = document.getElementById('activity-log');

  function appendDocker(line) {
    state.docker.push(line);
    setCount('docker', state.docker.length);
    if (document.getElementById('docker-deadlock-only').checked && !line.deadlock) return;
    var follow = document.getElementById('docker-follow').checked;
    dockerEl.insertAdjacentHTML('beforeend',
      '<div class="log-line stream-' + esc(line.stream) + (line.deadlock ? ' is-deadlock' : '') + '">' +
      '<span class="log-time">' + fmtTime(line.at) + '</span>' +
      '<span class="log-text">' + esc(line.text) + '</span></div>');
    while (dockerEl.childElementCount > 4000) dockerEl.removeChild(dockerEl.firstElementChild);
    if (follow) dockerEl.scrollTop = dockerEl.scrollHeight;
  }

  function renderDockerAll() {
    var onlyDeadlock = document.getElementById('docker-deadlock-only').checked;
    var lines = state.docker.filter(function (l) { return !onlyDeadlock || l.deadlock; });
    dockerEl.innerHTML = lines.length ? lines.map(function (line) {
      return '<div class="log-line stream-' + esc(line.stream) + (line.deadlock ? ' is-deadlock' : '') + '">' +
        '<span class="log-time">' + fmtTime(line.at) + '</span>' +
        '<span class="log-text">' + esc(line.text) + '</span></div>';
    }).join('') : '<div class="dock-empty">No container output captured yet.</div>';
    dockerEl.scrollTop = dockerEl.scrollHeight;
  }

  function appendActivity(ev) {
    state.activity.push(ev);
    activityEl.insertAdjacentHTML('beforeend',
      '<div class="log-line level-' + esc(ev.level || 'info') + '">' +
      '<span class="log-time">' + fmtTime(ev.at) + '</span>' +
      '<span class="log-text">' + esc(ev.message) + '</span></div>');
    activityEl.scrollTop = activityEl.scrollHeight;
  }

  // ---------------------------------------------------------- run state

  // wireArchivedOnly leaves a finished run browsable: step selection, the dock
  // and its tabs all work; nothing that would mutate the run exists.
  function wireArchivedOnly() {
    document.querySelectorAll('[data-step-card]').forEach(function (card) {
      card.addEventListener('click', function () { selectStep(Number(card.dataset.stepCard)); });
    });
    document.querySelectorAll('.dock-tab').forEach(function (t) {
      t.addEventListener('click', function () { activateTab(t.dataset.tab); });
    });
    document.getElementById('locks-granted').addEventListener('change', renderLocks);
    setupDock();
    renderAllSteps();
    renderRunState();
    if (window.DL_ARCHIVED_LOCKS) {
      state.locks = window.DL_ARCHIVED_LOCKS;
      renderLocks();
    }
    document.querySelector('[data-lanes-end]').textContent =
      'This run is closed and its scratch database was dropped. Showing the recorded result.';
    connState.textContent = 'finished';
  }

  function renderRunState() {
    var st = state.run;
    var badge = document.querySelector('[data-run-status]');
    if (badge) {
      badge.textContent = st.status;
      badge.setAttribute('data-run-status', st.status);
    }
    var cursor = document.querySelector('[data-cursor]');
    if (cursor) cursor.textContent = st.cursor;

    (st.actors || []).forEach(function (a) {
      var trx = document.querySelector('[data-actor-trx="' + a.id + '"]');
      if (trx) trx.hidden = !a.in_trx;
    });

    var stepBtn = document.getElementById('btn-step');
    var playBtn = document.getElementById('btn-play');
    var done = st.cursor >= st.total;
    if (stepBtn && playBtn) {
      stepBtn.disabled = done || st.status === 'closed' || st.status === 'failed';
      playBtn.disabled = stepBtn.disabled;
      document.querySelector('[data-lanes-end]').textContent =
        done ? 'End of scenario — every step has been submitted.' : 'Press Step to submit step ' + (st.cursor + 1) + '.';
    }

    if (st.deadlock_report) {
      document.getElementById('deadlock-report').innerHTML =
        '<div class="dl-report">' + window.DL.highlightDeadlock(st.deadlock_report) + '</div>';
      var dot = document.querySelector('[data-dot="deadlock"]');
      if (dot) dot.hidden = false;
    }
  }

  function setCount(name, n) {
    var el = document.querySelector('[data-count="' + name + '"]');
    if (el) el.textContent = n ? '(' + n + ')' : '';
  }

  // ------------------------------------------------------------ event feed

  var connState = document.getElementById('conn-state');
  var source = null;

  function connect() {
    if (source) source.close();
    source = new EventSource('/run/' + runID + '/events?since=' + state.lastSeq);

    source.addEventListener('open', function () {
      connState.textContent = 'live';
      connState.className = 'conn-state is-live';
    });
    source.addEventListener('error', function () {
      connState.textContent = 'reconnecting…';
      connState.className = 'conn-state is-down';
    });
    source.addEventListener('ready', function () {
      connState.textContent = 'live';
      connState.className = 'conn-state is-live';
    });
    source.addEventListener('closed', function () {
      connState.textContent = 'run closed';
      connState.className = 'conn-state is-down';
      source.close();
    });

    ['state', 'step', 'wire', 'locks', 'docker', 'log'].forEach(function (type) {
      source.addEventListener(type, function (e) {
        var ev;
        try { ev = JSON.parse(e.data); } catch (err) { return; }
        if (ev.seq) state.lastSeq = Math.max(state.lastSeq, ev.seq);
        handle(ev);
      });
    });
  }

  function handle(ev) {
    switch (ev.type) {
      case 'state':
        state.run = ev.state;
        renderRunState();
        break;
      case 'step':
        stepsByIndex[ev.step.index] = ev.step;
        for (var i = 0; i < state.steps.length; i++) {
          if (state.steps[i].index === ev.step.index) state.steps[i] = ev.step;
        }
        renderStepCard(ev.step);
        if (state.selected === ev.step.index) renderStepDetail();
        break;
      case 'wire':
        appendWire(ev.wire);
        break;
      case 'locks':
        setSnapshot(ev.locks);
        renderLocks();
        break;
      case 'docker':
        appendDocker(ev.docker);
        break;
      case 'log':
        appendActivity(ev);
        break;
    }
  }

  // ------------------------------------------------------------- controls

  var busy = false;

  function toast(message, kind) {
    var el = document.getElementById('toast');
    el.textContent = message;
    el.className = 'toast' + (kind ? ' is-' + kind : '');
    el.hidden = false;
    clearTimeout(el._timer);
    el._timer = setTimeout(function () { el.hidden = true; }, 6000);
  }

  // nextPendingStep is the step the next press of Step will submit.
  function nextPendingStep() {
    var cursor = (state.run && state.run.cursor) || 0;
    return stepsByIndex[cursor + 1] || null;
  }

  function doStep() {
    if (busy) return;

    // The guess has to be taken before the statement is submitted, or there is
    // nothing to guess about.
    var pending = predict.on ? nextPendingStep() : null;
    var ask = pending ? askPrediction(pending) : Promise.resolve('');

    ask.then(function (guess) {
      if (busy) return;
      busy = true;
      var btn = document.getElementById('btn-step');
      btn.disabled = true;
      postJSON('/run/' + runID + '/step')
        .then(function (res) {
          if (!res.ok) {
            if (res.done) toast('Every step has been submitted.', 'warn');
            else toast(res.error, res.blocked_actor ? 'warn' : 'error');
            return;
          }
          if (res.step) {
            // A newly submitted step gets the generous reveal margin.
            selectStep(res.step.index, true);
          }
          if (!guess || !res.step) return;
          // A step that blocks has no final outcome yet; wait for the settle
          // window to pass so the verdict is judged on what happened, not on
          // where it was when the request returned.
          var index = res.step.index;
          setTimeout(function () {
            var settled = stepsByIndex[index];
            if (settled) scorePrediction(guess, settled);
          }, (window.DL_SETTLE_MS || 400) + 350);
        })
        .catch(function (err) { toast(String(err), 'error'); })
        .finally(function () {
          busy = false;
          renderRunState();
        });
    });
  }

  function doPlay() {
    if (busy) return;
    busy = true;
    document.getElementById('btn-play').disabled = true;
    postJSON('/run/' + runID + '/play')
      .then(function (res) {
        if (!res.ok) toast(res.error, 'error');
        else if (res.stopped) toast(res.stopped, 'warn');
      })
      .catch(function (err) { toast(String(err), 'error'); })
      .finally(function () { busy = false; renderRunState(); });
  }

  // ------------------------------------------------------------- predict
  //
  // The scenario declares what each step should do and the run records what it
  // did. Predict mode simply withholds the first and asks for a guess before
  // revealing the second — the data is already there, this is a switch over it.
  var predict = {
    on: false,
    right: 0,
    wrong: 0,
    // pending resolves once the reader has answered, so doStep can await it.
    ask: null
  };

  var predictToggle = document.getElementById('predict-mode');
  if (predictToggle) {
    try { predictToggle.checked = localStorage.getItem('dl-predict') === '1'; } catch (e) {}
    predict.on = predictToggle.checked;
    document.body.classList.toggle('is-predicting', predict.on);
    predictToggle.addEventListener('change', function () {
      predict.on = predictToggle.checked;
      document.body.classList.toggle('is-predicting', predict.on);
      try { localStorage.setItem('dl-predict', predict.on ? '1' : '0'); } catch (e) {}
      renderAllSteps();
    });
  }

  var PREDICT_CHOICES = [
    { value: 'ok', label: 'completes' },
    { value: 'blocks', label: 'blocks on a lock' },
    { value: 'deadlock', label: 'deadlock (1213)' },
    { value: 'timeout', label: 'lock wait timeout (1205)' },
    { value: 'error', label: 'some other error' }
  ];

  // askPrediction resolves to the chosen outcome, or '' if the reader skipped.
  function askPrediction(step) {
    var body = '<div class="predict-sql"><code>' + esc(oneLineSQL(step.sql)) + '</code></div>' +
      '<div class="predict-choices">' +
      PREDICT_CHOICES.map(function (c) {
        return '<button class="btn btn-sm" type="button" data-guess="' + c.value + '">' +
          esc(c.label) + '</button>';
      }).join('') + '</div>';

    return window.DL.confirm({
      title: 'Step ' + step.index + ' · ' + (step.actor_name || step.actor) + ' — what happens?',
      bodyHTML: body,
      confirm: '',
      cancel: 'Skip',
      onBody: function (el, close) {
        el.querySelectorAll('[data-guess]').forEach(function (b) {
          b.addEventListener('click', function () { close(b.dataset.guess); });
        });
      }
    });
  }

  function oneLineSQL(sql) {
    return String(sql || '').replace(/\s+/g, ' ').trim();
  }

  // scorePrediction compares the guess with what the step actually did, once it
  // has settled. A blocked step is judged on whether it ever waited, matching
  // how the scenario's own expectations are evaluated.
  function scorePrediction(guess, step) {
    if (!guess) return;
    var actual = step.actual || '';
    var right = guess === actual ||
      (guess === 'blocks' && step.was_blocked) ||
      (guess === 'error' && (actual === 'deadlock' || actual === 'timeout' || actual === 'error'));
    if (right) predict.right++; else predict.wrong++;

    toast(right
      ? 'Right — it ' + describeOutcome(actual) + '. ' + predictScore()
      : 'Not quite: you said ' + guess + ', it ' + describeOutcome(actual) + '. ' + predictScore(),
      right ? 'ok' : 'warn');
  }

  function describeOutcome(actual) {
    switch (actual) {
      case 'ok': return 'completed';
      case 'blocks': return 'blocked';
      case 'deadlock': return 'was chosen as the deadlock victim';
      case 'timeout': return 'hit the lock wait timeout';
      case 'error': return 'failed';
      default: return 'did something unrecorded';
    }
  }

  function predictScore() {
    var total = predict.right + predict.wrong;
    return predict.right + '/' + total + ' so far.';
  }

  var stepBtnEl = document.getElementById('btn-step');
  if (!stepBtnEl) {
    // Archived run: no controls to wire, and no keyboard stepping either.
    wireArchivedOnly();
    return;
  }

  stepBtnEl.addEventListener('click', doStep);
  document.getElementById('btn-play').addEventListener('click', doPlay);
  document.getElementById('btn-snapshot').addEventListener('click', function () {
    postJSON('/run/' + runID + '/snapshot').then(function (res) {
      if (res.ok) { setSnapshot(res.locks); renderLocks(); activateTab('locks'); }
    });
  });
  document.getElementById('btn-close-run').addEventListener('click', function () {
    window.DL.confirm({
      title: 'Close this run?',
      body: 'Its connections are dropped and the scratch database is deleted. ' +
        'The run stays in the history.',
      confirm: 'Close the run',
      cancel: 'Keep it open',
      danger: true
    }).then(function (ok) {
      if (!ok) return;
      postJSON('/run/' + runID + '/close').then(function () { window.location.href = '/'; });
    });
  });

  var moreBtn = document.getElementById('btn-more');
  var moreMenu = document.getElementById('more-menu');
  moreBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    moreMenu.hidden = !moreMenu.hidden;
  });
  document.addEventListener('click', function () { moreMenu.hidden = true; });
  moreMenu.addEventListener('click', function (e) { e.stopPropagation(); });

  document.querySelectorAll('[data-step-card]').forEach(function (card) {
    card.addEventListener('click', function () { selectStep(Number(card.dataset.stepCard)); });
  });

  function activateTab(name) {
    document.querySelectorAll('.dock-tab').forEach(function (t) {
      t.classList.toggle('is-active', t.dataset.tab === name);
    });
    document.querySelectorAll('.dock-panel').forEach(function (p) {
      p.classList.toggle('is-active', p.dataset.panel === name);
    });
    if (name === 'wire') renderWireAll();
    if (name === 'docker') renderDockerAll();
  }
  document.querySelectorAll('.dock-tab').forEach(function (t) {
    t.addEventListener('click', function () { activateTab(t.dataset.tab); });
  });

  ['wire-actor', 'wire-step', 'wire-hex'].forEach(function (id) {
    document.getElementById(id).addEventListener('change', renderWireAll);
  });
  document.getElementById('wire-clear').addEventListener('click', function () {
    state.wire = [];
    setCount('wire', 0);
    renderWireAll();
  });
  document.getElementById('locks-granted').addEventListener('change', renderLocks);
  document.getElementById('docker-deadlock-only').addEventListener('change', renderDockerAll);

  // arrowsNavigate decides whether an arrow key moves between steps or is left
  // to the browser to scroll with.
  //
  // The Step pane shows whichever step is selected, so arrows navigating it is
  // exactly what you want. The other panes — locks, packets, container log —
  // are long lists you read by scrolling, and hijacking the arrow keys there
  // would make them unreadable. j/k always navigate, whichever pane is open.
  function arrowsNavigate(target) {
    var dock = document.getElementById('dock');
    if (!dock || !dock.contains(target)) return true;
    var active = dock.querySelector('.dock-panel.is-active');
    return !!active && active.dataset.panel === 'step';
  }

  document.addEventListener('keydown', function (e) {
    if (window.DL.isTypingTarget(e.target)) return;
    if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); doStep(); return; }

    var next = e.key === 'ArrowDown' || e.key === 'j';
    var prev = e.key === 'ArrowUp' || e.key === 'k';
    if (!next && !prev) return;
    if (e.key.indexOf('Arrow') === 0 && !arrowsNavigate(e.target)) return;

    e.preventDefault();
    if (next) selectStep(Math.min((state.selected || 0) + 1, state.steps.length));
    else selectStep(Math.max((state.selected || 1) - 1, 1));
  });

  // The dock is focusable so clicking into the Step pane and then pressing an
  // arrow key navigates, rather than doing nothing until you click a card.
  var dockEl = document.getElementById('dock');
  if (dockEl && !dockEl.hasAttribute('tabindex')) dockEl.setAttribute('tabindex', '-1');

  // ------------------------------------------------------- resizable drawer

  function setupDockResize() {
    var dock = document.getElementById('dock');
    var handle = document.getElementById('dock-resize');
    var split = document.querySelector('.run-split');
    if (!dock || !handle || !split) return;

    var DEFAULT = '44vh';
    var MIN = 120;

    try {
      var saved = localStorage.getItem('dl-dock-height');
      if (saved) split.style.setProperty('--dock-h', saved);
    } catch (e) {}

    var dragging = false;

    function heightFromEvent(e) {
      var rect = split.getBoundingClientRect();
      // Leave room for at least a couple of lane rows above the dock.
      var max = rect.height - 140;
      return Math.max(MIN, Math.min(max, rect.bottom - e.clientY));
    }

    handle.addEventListener('pointerdown', function (e) {
      dragging = true;
      handle.setPointerCapture(e.pointerId);
      document.body.classList.add('is-resizing');
      e.preventDefault();
    });

    handle.addEventListener('pointermove', function (e) {
      if (!dragging) return;
      split.style.setProperty('--dock-h', heightFromEvent(e) + 'px');
    });

    function endDrag(e) {
      if (!dragging) return;
      dragging = false;
      document.body.classList.remove('is-resizing');
      if (e && e.pointerId !== undefined && handle.hasPointerCapture(e.pointerId)) {
        handle.releasePointerCapture(e.pointerId);
      }
      try {
        localStorage.setItem('dl-dock-height', split.style.getPropertyValue('--dock-h'));
      } catch (err) {}
    }

    handle.addEventListener('pointerup', endDrag);
    handle.addEventListener('pointercancel', endDrag);

    handle.addEventListener('dblclick', function () {
      split.style.setProperty('--dock-h', DEFAULT);
      try { localStorage.removeItem('dl-dock-height'); } catch (e) {}
    });
  }
  // ------------------------------------------------------ collapsible drawer
  //
  // The dock is where you read what happened; the lanes are where you watch it
  // happen. On a laptop you cannot have much of both, so it collapses to its
  // tab bar and gives the whole window back to the timeline.
  //
  // The state is remembered because it is a working preference, not a per-run
  // decision: someone who wants the lanes full-height wants that every time.
  function setupDockCollapse() {
    var dock = document.getElementById('dock');
    var split = document.querySelector('.run-split');
    var btn = document.getElementById('dock-collapse');
    if (!dock || !split || !btn) return;

    var KEY = 'dl-dock-collapsed';

    function paint(collapsed) {
      split.classList.toggle('dock-collapsed', collapsed);
      btn.setAttribute('aria-expanded', String(!collapsed));
      btn.title = collapsed ? 'Expand the drawer' : 'Collapse the drawer';
    }

    function set(collapsed) {
      paint(collapsed);
      try { localStorage.setItem(KEY, collapsed ? '1' : '0'); } catch (e) {}
    }

    var stored = '0';
    try { stored = localStorage.getItem(KEY) || '0'; } catch (e) {}
    paint(stored === '1');

    btn.addEventListener('click', function () {
      set(!split.classList.contains('dock-collapsed'));
    });

    // Clicking a tab while collapsed means "show me that", not "switch a pane I
    // cannot see".
    document.querySelectorAll('.dock-tab').forEach(function (t) {
      t.addEventListener('click', function () {
        if (split.classList.contains('dock-collapsed')) set(false);
      });
    });

    // Selecting a step is a request to read its result, so the drawer comes
    // back for that too.
    document.querySelectorAll('[data-step-card]').forEach(function (card) {
      card.addEventListener('click', function () {
        if (split.classList.contains('dock-collapsed')) set(false);
      });
    });
  }

  // The dock's own chrome -- resize and collapse -- belongs to the drawer rather
  // than to the run controls, so it is wired for a closed run too. It was not,
  // which is why the collapse button did nothing on an archived run: the early
  // return for "no controls to bind" skipped it.
  function setupDock() {
    setupDockResize();
    setupDockCollapse();
  }
  setupDock();

  // ------------------------------------------------------------- start up

  renderAllSteps();
  renderRunState();

  if (window.DL_ARCHIVED) {
    // A finished run has no stream to join. Everything on screen came from its
    // history record, including the locks it ended with.
    connState.textContent = 'finished';
    connState.className = 'conn-state';
    if (window.DL_ARCHIVED_LOCKS) {
      state.locks = window.DL_ARCHIVED_LOCKS;
      renderLocks();
    }
    document.querySelector('[data-lanes-end]').textContent =
      'This run is closed and its scratch database was dropped. Showing the recorded result.';
  } else {
    connect();
  }
})();
