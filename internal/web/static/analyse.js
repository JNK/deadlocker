/* The two background analyses: an isolation-level sweep and a minimal-repro
   reduction. Both start a job on the server, which runs many real scenarios,
   so the page polls rather than waiting on a request. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  function poll(jobID, onUpdate) {
    var stopped = false;
    function tick() {
      if (stopped) return;
      fetch('/api/job/' + jobID)
        .then(function (r) { return r.json(); })
        .then(function (res) {
          if (!res.ok) { onUpdate(null, res.error); stopped = true; return; }
          onUpdate(res.job, null);
          if (res.job.status === 'running') setTimeout(tick, 1200);
        })
        .catch(function (err) { onUpdate(null, String(err)); stopped = true; });
    }
    tick();
  }

  function spinner(text) {
    return '<div class="job-progress"><span class="job-spinner"></span><span>' +
      esc(text || 'working…') + '</span></div>';
  }

  function cellClass(outcome) {
    switch (outcome) {
      case 'ok': return 'cell-ok';
      case 'blocked': return 'cell-blocked';
      case 'deadlock': return 'cell-deadlock';
      case 'timeout': return 'cell-timeout';
      case 'error': return 'cell-error';
      default: return 'cell-not';
    }
  }

  function renderMatrix(host, m) {
    if (!m) { host.innerHTML = ''; return; }
    var html = '<p class="callout callout-accent">' + esc(m.summary) + '</p>';
    html += '<div class="table-wrap"><table class="matrix-table"><thead><tr><th>Step</th>';
    m.columns.forEach(function (c) { html += '<th>' + esc(c.isolation) + '</th>'; });
    html += '</tr></thead><tbody>';

    (m.step_labels || []).forEach(function (label, i) {
      // Highlight the rows where the isolation level actually changes something.
      var seen = {};
      m.columns.forEach(function (c) {
        if (c.cells && c.cells[i]) seen[c.cells[i].outcome] = true;
      });
      var differs = Object.keys(seen).length > 1;

      html += '<tr' + (differs ? ' class="is-differing"' : '') + '>';
      html += '<td class="matrix-step"><code>' + esc(m.step_actors[i]) + '</code> ' + esc(label) + '</td>';
      m.columns.forEach(function (c) {
        var cell = (c.cells || [])[i];
        var outcome = cell ? cell.outcome : '—';
        html += '<td><span class="cell-outcome ' + cellClass(outcome) + '">' + esc(outcome) + '</span></td>';
      });
      html += '</tr>';
    });
    html += '</tbody></table></div>';
    host.innerHTML = html;
  }

  function renderShrink(host, s) {
    if (!s) { host.innerHTML = ''; return; }
    var html = '<p class="callout ' + (s.minimal_steps < s.original_steps ? 'callout-ok' : 'callout-accent') +
      '">' + esc(s.note || '') + '</p>';

    if (s.removed_labels && s.removed_labels.length) {
      html += '<div class="panel-subhead">Not needed for the ' + esc(s.target) + '</div><ul class="shrink-removed">';
      s.removed_labels.forEach(function (l) { html += '<li>' + esc(l) + '</li>'; });
      html += '</ul>';
    }
    if (s.yaml) {
      html += '<div class="panel-subhead">The minimal scenario</div>' +
        '<pre class="yaml code-scroll" data-yaml-view>' + esc(s.yaml) + '</pre>';
    }
    host.innerHTML = html;
    if (window.DL.highlightYAMLViews) window.DL.highlightYAMLViews(host);
  }

  document.querySelectorAll('[data-analyse]').forEach(function (btn) {
    var kind = btn.dataset.analyse;
    var host = document.getElementById(kind === 'isolation' ? 'matrix-out' : 'shrink-out');

    btn.addEventListener('click', function () {
      btn.disabled = true;
      host.innerHTML = spinner('starting…');

      window.DL.postJSON('/api/analyse/' + kind, { scenario_id: btn.dataset.scenario })
        .then(function (res) {
          if (!res.ok) {
            host.innerHTML = '<div class="callout callout-danger">' + esc(res.error) + '</div>';
            btn.disabled = false;
            return;
          }
          poll(res.job_id, function (job, err) {
            if (err) {
              host.innerHTML = '<div class="callout callout-danger">' + esc(err) + '</div>';
              btn.disabled = false;
              return;
            }
            if (job.status === 'running') { host.innerHTML = spinner(job.progress); return; }
            btn.disabled = false;
            if (job.status === 'failed') {
              host.innerHTML = '<div class="callout callout-danger">' + esc(job.error) + '</div>';
              return;
            }
            if (kind === 'isolation') renderMatrix(host, job.matrix);
            else renderShrink(host, job.shrink);
          });
        });
    });
  });
})();
