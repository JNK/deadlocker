/* The two background analyses: an isolation-level sweep and a minimal-repro
   reduction.

   Both start a job on the server, which runs many real scenarios, so the page
   polls. The same renderers serve the Analyse tab on a scenario and the
   standalone /analysis/{id} page. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  function poll(jobID, onUpdate) {
    function tick() {
      fetch('/api/job/' + jobID)
        .then(function (r) { return r.json(); })
        .then(function (res) {
          if (!res.ok) { onUpdate(null, res.error); return; }
          onUpdate(res.job, null);
          if (res.job.status === 'running') setTimeout(tick, 1200);
        })
        .catch(function (err) { onUpdate(null, String(err)); });
    }
    tick();
  }

  // showProgress paints the running state, updating the text in place rather
  // than rebuilding the element.
  //
  // Replacing the node restarts its CSS animation, and with a poll every 1.2s
  // the spinner visibly jerked back to the start rather than turning. A spinner
  // that stutters says "stuck" — the exact opposite of what it is for.
  function showProgress(host, text, jobID) {
    var existing = host.querySelector('.job-progress');
    if (!existing) {
      host.innerHTML = '<div class="job-progress">' +
        '<span class="job-spinner"></span>' +
        '<span class="job-progress-text"></span>' +
        '<button class="btn btn-sm btn-stop" type="button" data-abort hidden>Abort</button>' +
        '</div>';
      existing = host.querySelector('.job-progress');
    }
    existing.querySelector('.job-progress-text').textContent = text || 'working…';

    var abort = existing.querySelector('[data-abort]');
    abort.hidden = !jobID;
    if (jobID && abort.dataset.job !== jobID) {
      abort.dataset.job = jobID;
      abort.addEventListener('click', function () {
        abort.disabled = true;
        abort.textContent = 'aborting…';
        window.DL.postJSON('/api/job/' + encodeURIComponent(jobID) + '/cancel', {});
      });
    }
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

  function permalink(jobID) {
    return '<p class="muted analysis-permalink">Permanent link: ' +
      '<a class="link" href="/analysis/' + esc(jobID) + '">/analysis/' + esc(jobID) + '</a></p>';
  }

  // ------------------------------------------------------ isolation matrix

  function renderMatrix(host, m, jobID) {
    if (!m) { host.innerHTML = ''; return; }
    var html = '<p class="callout callout-accent">' + esc(m.summary) + '</p>';
    html += '<div class="table-wrap"><table class="matrix-table"><thead><tr><th>Step</th>';
    m.columns.forEach(function (c) { html += '<th>' + esc(c.label || c.isolation) + '</th>'; });
    html += '</tr></thead><tbody>';

    (m.step_labels || []).forEach(function (label, i) {
      // Highlight the rows where the swept axis actually changes something.
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
    if (jobID) html += permalink(jobID);
    host.innerHTML = html;
  }

  // --------------------------------------------------- minimal reproduction

  function renderShrink(host, s, jobID) {
    if (!s) { host.innerHTML = ''; return; }

    var reduced = s.minimal_steps < s.original_steps;
    var html = '<p class="callout ' + (reduced ? 'callout-ok' : 'callout-accent') + '">' +
      esc(s.note || '') + '</p>';

    // The original sequence with the drops struck through says more than a list
    // of survivors: you can see what turned out to be incidental.
    if (s.steps && s.steps.length) {
      html += '<div class="shrink-legend">' +
        '<span><span class="shrink-dot is-kept"></span>needed for the ' + esc(s.target) + '</span>' +
        '<span><span class="shrink-dot is-dropped"></span>can be dropped</span>' +
        '</div>';
      html += '<ol class="shrink-steps">';
      s.steps.forEach(function (st) {
        html += '<li class="shrink-step accent-' + esc(st.accent || 'blue') +
          (st.kept ? ' is-kept' : ' is-dropped') + '">' +
          '<span class="shrink-num">' + st.index + '</span>' +
          '<div class="shrink-body">' +
          '<div class="shrink-head">' +
          '<span class="seq-actor"><span class="actor-dot"></span>' + esc(st.actor) + '</span>' +
          '<span class="shrink-label">' + esc(st.label || '') + '</span>' +
          (st.expect ? '<span class="steps-expect">expects ' + esc(st.expect) + '</span>' : '') +
          '<span class="shrink-verdict">' + (st.kept ? 'needed' : 'not needed') + '</span>' +
          '</div>' +
          '<pre class="shrink-sql">' + esc(st.sql) + '</pre>' +
          '</div></li>';
      });
      html += '</ol>';
    }

    if (s.yaml) {
      html += '<div class="shrink-actions">' +
        '<button class="btn btn-sm" data-shrink-copy type="button">Copy the YAML</button>' +
        (jobID ? '<button class="btn btn-sm" data-shrink-apply="new" data-job="' + esc(jobID) +
          '" type="button">Save as a new scenario</button>' : '') +
        (jobID && reduced ? '<button class="btn btn-sm btn-danger" data-shrink-apply="replace" data-job="' +
          esc(jobID) + '" type="button">Replace the original</button>' : '') +
        '</div>';
      html += '<div class="shrink-status pg-status" id="shrink-status" hidden></div>';
      html += '<details class="shrink-yaml"><summary>The minimal scenario as YAML</summary>' +
        '<pre class="yaml code-scroll" data-yaml-view>' + esc(s.yaml) + '</pre></details>';
    }
    if (jobID) html += permalink(jobID);

    host.innerHTML = html;
    if (window.DL.highlightYAMLViews) window.DL.highlightYAMLViews(host);
    wireShrinkActions(host, s);
  }

  function wireShrinkActions(host, s) {
    var status = host.querySelector('#shrink-status');
    function say(kind, text) {
      if (!status) return;
      status.className = 'shrink-status pg-status is-' + kind;
      status.innerHTML = text;
      status.hidden = false;
    }

    var copy = host.querySelector('[data-shrink-copy]');
    if (copy) {
      copy.addEventListener('click', function () {
        navigator.clipboard.writeText(s.yaml).then(function () {
          copy.textContent = 'Copied';
          setTimeout(function () { copy.textContent = 'Copy the YAML'; }, 1600);
        }, function () {
          say('error', 'The clipboard is not available here; expand the YAML below and copy it by hand.');
        });
      });
    }

    host.querySelectorAll('[data-shrink-apply]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var mode = btn.dataset.shrinkApply;
        var dropped = s.original_steps - s.minimal_steps;
        var ask = mode === 'replace'
          ? {
              title: 'Replace the original scenario?',
              body: 'The file is overwritten with the ' + s.minimal_steps + '-step reduction. The ' +
                dropped + ' dropped step(s) are lost unless you have them in version control.',
              confirm: 'Replace it',
              cancel: 'Cancel',
              danger: true
            }
          : {
              title: 'Save as a new scenario?',
              body: 'The reduction is written beside the original, leaving it untouched.',
              confirm: 'Save it',
              cancel: 'Cancel'
            };

        window.DL.confirm(ask).then(function (ok) {
          if (!ok) return;
          btn.disabled = true;
          window.DL.postJSON('/api/analysis/' + btn.dataset.job + '/apply', { mode: mode })
            .then(function (res) {
              btn.disabled = false;
              if (!res.ok) { say('error', esc(res.error)); return; }
              say('ok', 'Written to <code>cases/' + esc(res.path) + '</code>. ' +
                '<a class="link" href="/case/' + esc(res.id) + '">Open it</a>.');
              window.dispatchEvent(new CustomEvent('dl-scenario-saved'));
            });
        });
      });
    });
  }

  // --------------------------------------------------------------- wiring

  function render(host, job, jobID) {
    if (job.kind === 'isolation-matrix' || job.kind === 'version-matrix') renderMatrix(host, job.matrix, jobID);
    else renderShrink(host, job.shrink, jobID);
  }

  // The standalone /analysis/{id} page.
  var page = document.getElementById('analysis-out');
  if (page && page.dataset.job) {
    poll(page.dataset.job, function (job, err) {
      if (err) { page.innerHTML = '<div class="callout callout-danger">' + esc(err) + '</div>'; return; }
      if (job.status === 'running') { showProgress(page, job.progress, job.id); return; }
      if (job.status === 'cancelled') {
        page.innerHTML = '<div class="callout callout-warn">Aborted. ' +
          esc(job.progress || '') + '</div>';
        return;
      }
      if (job.status === 'failed') {
        page.innerHTML = '<div class="callout callout-danger">' + esc(job.error) + '</div>';
        return;
      }
      render(page, job, job.id);
    });
  }

  // The Analyse tab on a scenario.
  document.querySelectorAll('[data-analyse]').forEach(function (btn) {
    var kind = btn.dataset.analyse;
    var host = document.getElementById(kind + '-out') ||
      document.getElementById(kind === 'isolation' ? 'matrix-out' : 'shrink-out');
    if (!host) return;

    btn.addEventListener('click', function () {
      btn.disabled = true;
      showProgress(host, 'starting…');

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
            if (job.status === 'running') { showProgress(host, job.progress, job.id); return; }
            btn.disabled = false;
            if (job.status === 'cancelled') {
              host.innerHTML = '<div class="callout callout-warn">Aborted after ' +
                esc(job.progress || 'starting') + '.</div>';
              return;
            }
            if (job.status === 'failed') {
              host.innerHTML = '<div class="callout callout-danger">' + esc(job.error) + '</div>';
              return;
            }
            render(host, job, job.id);
          });
        })
        .catch(function (err) {
          host.innerHTML = '<div class="callout callout-danger">' + esc(String(err)) + '</div>';
          btn.disabled = false;
        });
    });
  });
})();
