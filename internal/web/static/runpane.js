/* The scenario step pane, shared by the builder sheet and the playground.

   It renders a scenario's steps, overlays the live status of a run onto them,
   and owns the run controls. Both places want exactly this, so it lives once
   here rather than being reimplemented beside each editor. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  function oneLine(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }

  function fmtDuration(ms) {
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    return Math.floor(s / 60) + 'm ' + String(s % 60).padStart(2, '0') + 's';
  }

  // renderSteps draws the scenario, with live statuses overlaid when a run is
  // attached. Steps are matched by index, so the list keeps its shape whether
  // or not anything has run yet.
  function renderSteps(host, scenario, liveSteps, currentStep) {
    if (!scenario || !scenario.valid) {
      host.innerHTML = '<div class="steps-empty">' +
        (scenario && scenario.error
          ? '<strong>The scenario does not parse yet.</strong><span>' + esc(scenario.error) + '</span>'
          : 'Nothing to show yet. The steps appear here as the scenario takes shape.') +
        '</div>';
      return;
    }

    var statuses = {};
    (liveSteps || []).forEach(function (s) { statuses[s.index] = s; });

    var html = '';
    if (scenario.actors && scenario.actors.length) {
      html += '<div class="steps-actors">';
      scenario.actors.forEach(function (a) {
        html += '<span class="steps-actor accent-' + esc(a.accent || 'blue') + '">' +
          '<span class="actor-dot"></span>' + esc(a.name || a.id) + '</span>';
      });
      html += '</div>';
    }

    if (scenario.warnings && scenario.warnings.length) {
      html += '<div class="steps-warnings">';
      scenario.warnings.forEach(function (w) {
        html += '<div class="steps-warning">' + esc(w) + '</div>';
      });
      html += '</div>';
    }

    html += '<ol class="steps-list">';
    (scenario.steps || []).forEach(function (s) {
      var live = statuses[s.index];
      var status = live ? live.status : 'pending';
      html += '<li class="steps-item accent-' + esc(s.accent || 'blue') + ' status-' + esc(status) +
        (s.index === currentStep ? ' is-current' : '') +
        '" data-step="' + s.index + '">' +
        '<div class="steps-item-head">' +
        '<span class="steps-num">' + s.index + '</span>' +
        '<span class="steps-actor-tag">' + esc(s.actor) + '</span>' +
        '<span class="steps-label">' + esc(s.label || '') + '</span>';
      if (s.expect) html += '<span class="steps-expect">expects ' + esc(s.expect) + '</span>';
      if (live) html += '<span class="steps-status">' + esc(status) + '</span>';
      html += '</div><code class="steps-sql">' + esc(oneLine(s.sql)) + '</code>';

      if (live && live.error) {
        html += '<div class="steps-error">' + esc(live.error) + '</div>';
      } else if (live && status === 'blocked') {
        var who = (live.blocked_by || []).join(', ');
        html += '<div class="steps-blocked">waiting' + (who ? ' on ' + esc(who) : '') +
          countdownTag(live) + '</div>';
      } else if (live && live.verdict === 'mismatch') {
        html += '<div class="steps-blocked">' + esc(live.verdict_note || 'unexpected') + '</div>';
      } else if (s.note) {
        html += '<div class="steps-note">' + esc(s.note) + '</div>';
      }
      html += '</li>';
    });
    html += '</ol>';
    host.innerHTML = html;
  }

  // countdownTag renders the time left before innodb_lock_wait_timeout fires.
  function countdownTag(step) {
    if (!step.submitted_at || !currentTimeout) return '';
    var started = new Date(step.submitted_at).getTime();
    if (isNaN(started)) return '';
    return ' <span class="mini mini-countdown" data-countdown="' +
      (started + currentTimeout * 1000) + '"></span>';
  }

  // currentTimeout is the innodb_lock_wait_timeout of the attached run.
  var currentTimeout = 0;

  // One ticker updates every countdown the pane has rendered.
  setInterval(function () {
    document.querySelectorAll('.steps-view [data-countdown]').forEach(function (el) {
      var left = Number(el.dataset.countdown) - Date.now();
      if (left <= 0) {
        el.textContent = 'timeout due';
        el.classList.add('is-expired');
        return;
      }
      var s = Math.ceil(left / 1000);
      el.textContent = (s < 60 ? s + 's' : Math.floor(s / 60) + 'm ' + String(s % 60).padStart(2, '0') + 's') + ' to timeout';
      el.classList.toggle('is-urgent', left < 10000);
    });
  }, 500);

  // create wires a pane to its elements. Everything is optional except stepsEl,
  // so a caller that only wants the step list can leave the controls out.
  function create(opts) {
    var el = opts;
    var scenario = null;
    var run = null;      // { id, started, state, steps, tick }
    var timer = 0;
    var source = null;
    // The step the editor's caret is sitting in, if anything is telling us.
    var current = 0;

    function note(kind, text) {
      if (opts.onNote) opts.onNote(kind, text);
    }

    function externallyBusy() {
      return !!(opts.isBusy && opts.isBusy());
    }

    function paintSteps() {
      var live = run ? Object.keys(run.steps).map(function (k) { return run.steps[k]; }) : null;
      renderSteps(el.stepsEl, scenario, live, current);
    }

    function paintControls() {
      var busy = externallyBusy();
      var has = !!run;

      if (el.stopBtn) el.stopBtn.hidden = !(busy && has);
      if (el.runBtn) {
        el.runBtn.hidden = busy && has;
        el.runBtn.disabled = busy || !(scenario && scenario.valid);
        el.runBtn.textContent = has ? 'Run again' : 'Test run';
      }
      // Manual stepping is only safe when nothing else is driving the run.
      if (el.stepBtn) el.stepBtn.disabled = busy;
      if (el.playBtn) el.playBtn.disabled = busy;
    }

    function paintBar() {
      if (!el.bar || !run) return;
      var state = run.state;
      var status = state ? state.status : 'preparing';

      if (el.statusEl) {
        // While the container comes up, the phase is more use than the word
        // "preparing" — it is the difference between a 400 MB pull and a boot.
        el.statusEl.textContent = status === 'preparing' && state && state.prepare
          ? state.prepare.detail || status
          : status;
        el.statusEl.className = 'run-bar-status status-' + status;
      }
      // A run that failed to start says so once, where the pane can see it.
      if (state && state.status === 'failed' && state.error && !run.reported) {
        run.reported = true;
        note('error', state.error);
      }
      if (el.progressEl) {
        el.progressEl.textContent = state ? state.cursor + '/' + state.total : '—';
      }
      if (state && state.lock_wait_timeout) currentTimeout = state.lock_wait_timeout;
      if (el.clockEl) el.clockEl.textContent = fmtDuration(Date.now() - run.started);

      if (status === 'finished' || status === 'closed') {
        if (timer) { clearInterval(timer); timer = 0; }
      }
      paintControls();
    }

    function parse(text) {
      try { return JSON.parse(text); } catch (e) { return null; }
    }

    // reconcile asks the server for the truth rather than trusting that every
    // event arrived, so a dropped message cannot leave the list or the counter
    // stale.
    function reconcile(active) {
      if (run !== active) return;
      fetch('/run/' + active.id + '/state')
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (res) {
          if (!res || !res.ok || run !== active) return;
          active.state = res.state;
          (res.steps || []).forEach(function (s) { active.steps[s.index] = s; });
          paintBar();
          paintSteps();
        })
        .catch(function () {});
    }

    function attach(runID) {
      detach();
      var active = { id: runID, started: Date.now(), state: null, steps: {}, tick: 0 };
      run = active;

      if (el.bar) el.bar.hidden = false;
      if (el.openLink) el.openLink.href = '/run/' + runID;
      paintBar();
      reconcile(active);

      timer = setInterval(function () {
        paintBar();
        if (++active.tick % 4 === 0) reconcile(active);
      }, 250);

      source = new EventSource('/run/' + runID + '/events');
      source.addEventListener('state', function (e) {
        var ev = parse(e.data);
        if (ev && ev.state && run === active) { active.state = ev.state; paintBar(); }
      });
      source.addEventListener('step', function (e) {
        var ev = parse(e.data);
        if (ev && ev.step && run === active) {
          active.steps[ev.step.index] = ev.step;
          paintSteps();
        }
      });
      source.addEventListener('closed', function () {
        if (run === active) detach();
      });
    }

    function detach() {
      if (source) { source.close(); source = null; }
      if (timer) { clearInterval(timer); timer = 0; }
      if (el.bar) el.bar.hidden = true;
      run = null;
      paintSteps();
      paintControls();
    }

    // start runs the given YAML end to end: one press, no stepping required.
    function start(yaml) {
      return window.DL.postJSON('/run', { source: yaml }).then(function (r) {
        if (!r.ok) { note('error', r.error); return null; }
        attach(r.run_id);
        paintControls();
        return window.DL.postJSON('/run/' + r.run_id + '/play').then(function (res) {
          if (res && res.stopped) note('warn', res.stopped);
          return r.run_id;
        });
      });
    }

    function action(path) {
      if (!run) return Promise.resolve();
      return window.DL.postJSON('/run/' + run.id + '/' + path).then(function (res) {
        if (res && res.ok === false && res.error) note('warn', res.error);
        return res;
      });
    }

    if (el.stepBtn) el.stepBtn.addEventListener('click', function () { action('step'); });
    if (el.playBtn) el.playBtn.addEventListener('click', function () { action('play'); });
    if (el.closeBtn) {
      el.closeBtn.addEventListener('click', function () {
        if (!run) return;
        action('close').then(detach);
      });
    }
    if (el.stopBtn) {
      el.stopBtn.addEventListener('click', function () {
        action('stop').then(function () {
          note('warn', 'You stopped the run. The assistant is told on its next step.');
          paintControls();
        });
      });
    }

    paintSteps();
    paintControls();

    // setCurrentStep points the pane at whichever step is being edited. It
    // repaints only when the answer changes: this is driven by caret movement,
    // which happens on every keystroke.
    function setCurrentStep(index) {
      index = Number(index) || 0;
      if (index === current) return;
      current = index;
      paintSteps();
      var li = el.stepsEl.querySelector('.steps-item.is-current');
      if (li && li.scrollIntoView) {
        li.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      }
    }

    return {
      setScenario: function (view) { scenario = view; paintSteps(); paintControls(); },
      setCurrentStep: setCurrentStep,
      scenario: function () { return scenario; },
      attach: attach,
      detach: detach,
      start: start,
      runID: function () { return run ? run.id : ''; },
      paintControls: paintControls,
      repaint: paintSteps
    };
  }

  window.DL = window.DL || {};
  window.DL.createRunPane = create;
  window.DL.renderSteps = renderSteps;
})();
