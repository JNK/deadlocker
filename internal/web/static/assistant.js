/* The two assistant surfaces.

   - Builder: a modal sheet. Conversation on the left; on the right the
     scenario's steps as they are built, with the YAML source as a toggle. A
     test run stays inside the sheet and drives the same step list live.
   - Discuss bubble: a small docked panel on a scenario or run page.

   Both share window.DL.chat for streaming, block rendering and queueing. */

(function () {
  'use strict';

  var C = window.DL.chat;
  var esc = window.DL.escapeHTML;

  // --------------------------------------------------------- steps rendering

  // renderSteps draws the scenario as a compact step list. runState, when
  // present, overlays the live status of each step from an active run.
  function renderSteps(host, scenario, runState) {
    if (!scenario || !scenario.valid) {
      host.innerHTML = '<div class="steps-empty">' +
        (scenario && scenario.error
          ? '<strong>The draft does not parse yet.</strong><span>' + esc(scenario.error) + '</span>'
          : 'No draft yet. Describe what you want and the steps will appear here.') +
        '</div>';
      return;
    }

    var statuses = {};
    if (runState && runState.steps) {
      runState.steps.forEach(function (s) { statuses[s.index] = s; });
    }

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
      var cls = 'steps-item accent-' + esc(s.accent || 'blue') + ' status-' + esc(status);
      html += '<li class="' + cls + '" data-step="' + s.index + '">' +
        '<div class="steps-item-head">' +
        '<span class="steps-num">' + s.index + '</span>' +
        '<span class="steps-actor-tag">' + esc(s.actor) + '</span>' +
        '<span class="steps-label">' + esc(s.label || '') + '</span>';
      if (s.expect) {
        html += '<span class="steps-expect">expects ' + esc(s.expect) + '</span>';
      }
      if (live) {
        html += '<span class="steps-status">' + esc(status) + '</span>';
      }
      html += '</div>';
      html += '<code class="steps-sql">' + esc(oneLine(s.sql)) + '</code>';
      if (live && live.error) {
        html += '<div class="steps-error">' + esc(live.error) + '</div>';
      } else if (live && live.blocked_by && live.blocked_by.length) {
        html += '<div class="steps-blocked">waiting on ' + esc(live.blocked_by.join(', ')) + '</div>';
      } else if (s.note) {
        html += '<div class="steps-note">' + esc(s.note) + '</div>';
      }
      html += '</li>';
    });
    html += '</ol>';
    host.innerHTML = html;
  }

  function oneLine(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }

  // ------------------------------------------------------- builder (modal)

  var Builder = (function () {
    var el = {};
    var st = {
      open: false, session: null, scenarioID: '', savedDraft: '',
      turn: null, scenario: null, run: null, runTimer: 0, runSource: null,
      view: 'steps'
    };

    function q(id) { return document.getElementById(id); }

    function cache() {
      el.sheet = q('builder-sheet');
      if (!el.sheet) return false;
      el.backdrop = q('builder-backdrop');
      el.log = new C.Log(q('builder-log'), q('builder-empty'));
      el.input = q('builder-input');
      el.send = q('builder-send');
      el.status = q('builder-status');
      el.close = q('builder-close');
      el.model = q('builder-model');
      el.scenarioChip = q('builder-scenario');
      el.draft = q('builder-draft');
      el.draftState = q('builder-draft-state');
      el.draftApply = q('builder-draft-apply');
      el.run = q('builder-run');
      el.save = q('builder-save');
      el.suggestions = q('builder-suggestions');
      el.steps = q('builder-steps');
      el.sourcePane = q('builder-source-pane');
      el.stepsPane = q('builder-steps-pane');
      el.viewToggle = q('builder-view-toggle');
      el.runBar = q('builder-run-bar');
      el.runStatus = q('builder-run-status');
      el.runProgress = q('builder-run-progress');
      el.runClock = q('builder-run-clock');
      el.runStep = q('builder-run-step');
      el.runPlay = q('builder-run-play');
      el.runClose = q('builder-run-close');
      el.runOpen = q('builder-run-open');
      return true;
    }

    function setView(view) {
      st.view = view;
      el.stepsPane.hidden = view !== 'steps';
      el.sourcePane.hidden = view !== 'source';
      el.viewToggle.querySelectorAll('[data-view]').forEach(function (b) {
        b.classList.toggle('is-active', b.dataset.view === view);
      });
    }

    function open(opts) {
      opts = opts || {};
      st.scenarioID = opts.scenarioID || '';
      st.pendingPrompt = opts.prompt || '';
      st.session = null;
      st.open = true;
      detachRun();

      el.scenarioChip.hidden = !st.scenarioID;
      el.scenarioChip.textContent = st.scenarioID ? 'editing ' + st.scenarioID : '';
      el.log.clear();
      el.sheet.hidden = false;
      el.backdrop.hidden = false;
      document.body.classList.add('sheet-open');
      setView('steps');
      renderSteps(el.steps, null, null);

      C.status().then(function (s) {
        el.model.textContent = s.model || 'no model selected';
        if (!s.ready) {
          el.log.note('error', 'The assistant is not configured. Open Settings, point it at an ' +
            'OpenAI-compatible endpoint and choose a model. You can still edit the draft by hand.');
        }
      });

      C.createSession('build', st.scenarioID, '').then(function (res) {
        if (!res.ok) { el.log.note('error', res.error); return; }
        st.session = res.session;
        if (res.draft) {
          el.draft.value = res.draft;
          st.savedDraft = res.draft;
          st.scenario = res.scenario;
          renderSteps(el.steps, res.scenario, null);
          paint(false);
        }
      });

      C.loadSuggestions(el.suggestions, 'build', function (text) {
        el.input.value = text;
        el.composer.submit();
      });

      if (st.pendingPrompt) el.input.value = st.pendingPrompt;
      setTimeout(function () { el.input.focus(); }, 40);
    }

    function close(force) {
      if (!st.open) return;
      if (!force && !confirmClose()) return;
      if (st.turn) { st.turn.abort(); st.turn = null; }
      detachRun();
      el.sheet.hidden = true;
      el.backdrop.hidden = true;
      document.body.classList.remove('sheet-open');
      st.open = false;
    }

    function confirmClose() {
      if (el.composer && el.composer.isBusy()) {
        return window.confirm('The assistant is still replying. Close the builder anyway?');
      }
      if (isDraftDirty()) {
        return window.confirm('This draft has not been saved to the library. Close and lose it?');
      }
      return true;
    }

    function isDraftDirty() {
      var current = el.draft.value.trim();
      if (!current) return false;
      return current !== (st.savedDraft || '').trim();
    }

    function paint(dirty) {
      el.draftState.textContent = dirty ? 'edited — apply to share' : 'in sync';
      el.draftState.className = 'draft-state' + (dirty ? ' is-dirty' : '');
    }

    // ------------------------------------------------------------ live run

    // attachRun subscribes to a run's event stream so the step list updates as
    // the run progresses, without navigating away from the builder.
    function attachRun(runID) {
      detachRun();
      st.run = { id: runID, started: Date.now(), state: null };
      el.runBar.hidden = false;
      el.runOpen.href = '/run/' + runID;
      setView('steps');
      updateRunBar();

      st.runTimer = setInterval(updateRunBar, 250);

      var source = new EventSource('/run/' + runID + '/events');
      st.runSource = source;

      source.addEventListener('state', function (e) {
        var ev = safeParse(e.data);
        if (ev && ev.state) {
          st.run.state = ev.state;
          updateRunBar();
        }
      });
      source.addEventListener('step', function (e) {
        var ev = safeParse(e.data);
        if (!ev || !ev.step) return;
        if (!st.run.steps) st.run.steps = {};
        st.run.steps[ev.step.index] = ev.step;
        renderSteps(el.steps, st.scenario, { steps: Object.values(st.run.steps) });
      });
      source.addEventListener('closed', detachRun);
    }

    function detachRun() {
      if (st.runSource) { st.runSource.close(); st.runSource = null; }
      if (st.runTimer) { clearInterval(st.runTimer); st.runTimer = 0; }
      if (el.runBar) el.runBar.hidden = true;
      st.run = null;
    }

    function updateRunBar() {
      if (!st.run) return;
      var state = st.run.state;
      var status = state ? state.status : 'preparing';
      el.runStatus.textContent = status;
      el.runStatus.className = 'run-bar-status status-' + status;
      el.runProgress.textContent = state ? state.cursor + '/' + state.total : '—';
      // The stopwatch runs until the scenario is finished, then freezes.
      if (status === 'finished' || status === 'closed') {
        if (st.runTimer) { clearInterval(st.runTimer); st.runTimer = 0; }
      }
      el.runClock.textContent = C.fmtDuration(Date.now() - st.run.started);
    }

    function safeParse(s) {
      try { return JSON.parse(s); } catch (e) { return null; }
    }

    // ------------------------------------------------------------- actions

    function applyDraft() {
      if (!st.session) return Promise.resolve();
      return window.DL.postJSON('/api/chat/' + st.session + '/draft', { yaml: el.draft.value })
        .then(function (res) {
          if (!res.ok) { el.log.note('error', 'Draft rejected: ' + res.error); return false; }
          st.scenario = res.scenario;
          renderSteps(el.steps, res.scenario, st.run && st.run.steps
            ? { steps: Object.values(st.run.steps) } : null);
          paint(false);
          return true;
        });
    }

    function saveDraft() {
      if (!st.session) return;
      applyDraft().then(function (ok) {
        if (!ok) return;
        return window.DL.postJSON('/api/chat/' + st.session + '/save', {}).then(function (res) {
          if (!res.ok) { el.log.note('error', 'Not saved: ' + res.error); return; }
          st.savedDraft = el.draft.value;
          paint(false);
          el.log.note('ok', 'Saved to cases/' + res.path);
          (res.warnings || []).forEach(function (w) { el.log.note('warn', w); });
          window.dispatchEvent(new CustomEvent('dl-scenario-saved'));
        });
      });
    }

    // The test run stays in the sheet: it drives the step list beside the
    // conversation instead of opening a tab and losing the context.
    function testRun() {
      applyDraft().then(function (ok) {
        if (!ok) return;
        el.log.note('info', 'Starting a test run of the draft…');
        return window.DL.postJSON('/run', { source: el.draft.value }).then(function (r) {
          if (!r.ok) { el.log.note('error', r.error); return; }
          attachRun(r.run_id);
          el.log.note('ok', 'Run ' + r.run_id + ' is ready — step it from the panel on the right.');
        });
      });
    }

    function runAction(path) {
      if (!st.run) return;
      window.DL.postJSON('/run/' + st.run.id + '/' + path).then(function (res) {
        if (res && res.ok === false && res.error) el.log.note('warn', res.error);
      });
    }

    function init() {
      if (!cache()) return;

      el.composer = new C.Composer(el.input, el.send, el.status, el.log, {
        ready: function () { return !!st.session; },
        onSend: function (text) {
          st.turn = C.runTurn(st.session, text, el.log, {
            draft: function (data) {
              el.draft.value = data.yaml || '';
              st.savedDraft = st.savedDraft || '';
              st.scenario = data.scenario;
              renderSteps(el.steps, data.scenario, st.run && st.run.steps
                ? { steps: Object.values(st.run.steps) } : null);
              paint(isDraftDirty());
            },
            run: function (data) { attachRun(data.run_id); },
            saved: function (data) {
              st.savedDraft = el.draft.value;
              paint(false);
              el.log.note('ok', 'Saved to cases/' + data.text);
              window.dispatchEvent(new CustomEvent('dl-scenario-saved'));
            }
          });
          return st.turn.promise
            .catch(function (err) {
              if (err && err.name === 'AbortError') return;
              el.log.note('error', String(err && err.message ? err.message : err));
            })
            .finally(function () { st.turn = null; });
        }
      });

      el.close.addEventListener('click', function () { close(false); });
      el.draftApply.addEventListener('click', applyDraft);
      el.save.addEventListener('click', saveDraft);
      el.run.addEventListener('click', testRun);
      el.draft.addEventListener('input', function () { paint(isDraftDirty()); });

      el.viewToggle.querySelectorAll('[data-view]').forEach(function (b) {
        b.addEventListener('click', function () { setView(b.dataset.view); });
      });

      el.runStep.addEventListener('click', function () { runAction('step'); });
      el.runPlay.addEventListener('click', function () { runAction('play'); });
      el.runClose.addEventListener('click', function () {
        if (!st.run) return;
        window.DL.postJSON('/run/' + st.run.id + '/close').then(detachRun);
      });

      el.backdrop.addEventListener('click', function (e) { e.preventDefault(); });

      document.addEventListener('keydown', function (e) {
        if (e.key !== 'Escape' || !st.open) return;
        e.preventDefault();
        e.stopPropagation();
        el.status.textContent = 'Escape is disabled here — use ✕ to close';
        setTimeout(function () { el.composer.paint(); }, 2200);
      }, true);

      window.addEventListener('beforeunload', function (e) {
        if (!st.open) return;
        if ((el.composer && el.composer.isBusy()) || isDraftDirty()) {
          e.preventDefault();
          e.returnValue = '';
        }
      });

      document.querySelectorAll('[data-builder]').forEach(function (btn) {
        btn.addEventListener('click', function () {
          open({
            scenarioID: btn.dataset.scenario || '',
            prompt: btn.dataset.prompt || ''
          });
        });
      });
    }

    return { init: init, open: open };
  })();

  // ------------------------------------------------------ discuss (bubble)

  var Bubble = (function () {
    var el = {};
    var st = { open: false, session: null, scenarioID: '', runID: '', turn: null };

    function q(id) { return document.getElementById(id); }

    function cache() {
      el.panel = q('bubble');
      if (!el.panel) return false;
      el.launcher = q('bubble-launcher');
      el.log = new C.Log(q('bubble-log'), q('bubble-empty'));
      el.input = q('bubble-input');
      el.send = q('bubble-send');
      el.status = q('bubble-status');
      el.min = q('bubble-min');
      el.model = q('bubble-model');
      el.suggestions = q('bubble-suggestions');
      return true;
    }

    function show() {
      el.panel.hidden = false;
      el.launcher.hidden = true;
      st.open = true;
      if (!st.session) start();
      setTimeout(function () { el.input.focus(); }, 30);
    }

    function hide() {
      el.panel.hidden = true;
      el.launcher.hidden = false;
      st.open = false;
    }

    function start() {
      C.status().then(function (s) {
        el.model.textContent = s.model || 'no model';
        if (!s.ready) {
          el.log.note('error', 'Not configured yet — set an endpoint and model in Settings.');
        }
      });
      C.createSession('discuss', st.scenarioID, st.runID).then(function (res) {
        if (!res.ok) { el.log.note('error', res.error); return; }
        st.session = res.session;
      });
    }

    function init() {
      if (!cache()) return;
      var host = document.querySelector('[data-discuss]');
      if (!host) return;
      st.scenarioID = host.dataset.scenario || '';
      st.runID = host.dataset.run || '';

      el.composer = new C.Composer(el.input, el.send, el.status, el.log, {
        idleText: 'Enter to send',
        ready: function () { return !!st.session; },
        onSend: function (text) {
          st.turn = C.runTurn(st.session, text, el.log, {
            run: function (data) {
              el.log.note('info', 'Started run ' + data.run_id);
            }
          });
          return st.turn.promise
            .catch(function (err) {
              if (err && err.name === 'AbortError') return;
              el.log.note('error', String(err && err.message ? err.message : err));
            })
            .finally(function () { st.turn = null; });
        }
      });

      el.launcher.hidden = false;
      el.launcher.addEventListener('click', show);
      el.min.addEventListener('click', hide);

      C.loadSuggestions(el.suggestions, 'discuss', function (text) {
        el.input.value = text;
        el.composer.submit();
      });

      document.addEventListener('keydown', function (e) {
        if (e.key !== 'Escape' || !st.open) return;
        if (document.body.classList.contains('sheet-open')) return;
        e.preventDefault();
        e.stopPropagation();
        hide();
      });
    }

    return { init: init, show: show };
  })();

  function boot() {
    Builder.init();
    Bubble.init();
  }

  window.DL = window.DL || {};
  window.DL.openBuilder = Builder.open;
  window.DL.openDiscuss = Bubble.show;

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
