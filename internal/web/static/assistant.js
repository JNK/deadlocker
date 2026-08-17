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
      el.stop = q('builder-stop');
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
      // Opened by navigating to /builder, as opposed to from a button on a
      // page that is still behind the sheet.
      st.viaRoute = !!opts.viaRoute;
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
      paintRunControls();

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
          setDraftValue(res.draft);
          st.savedDraft = res.draft;
          st.scenario = res.scenario;
          paintSteps();
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
      if (force) { finishClose(); return; }

      var ask = closeQuestion();
      if (!ask) { finishClose(); return; }
      window.DL.confirm(ask).then(function (ok) {
        if (ok) finishClose();
      });
    }

    // closeQuestion returns what to ask before closing, or null when there is
    // nothing at stake.
    function closeQuestion() {
      if (el.composer && el.composer.isBusy()) {
        return {
          title: 'The assistant is still replying',
          body: 'Closing now discards the reply in progress.',
          confirm: 'Close anyway',
          cancel: 'Keep waiting',
          danger: true
        };
      }
      if (isDraftDirty()) {
        return {
          title: 'This draft has not been saved',
          body: 'It only exists in this conversation. Closing loses it.',
          confirm: 'Discard the draft',
          cancel: 'Keep editing',
          danger: true
        };
      }
      return null;
    }

    function finishClose() {
      if (st.turn) { st.turn.abort(); st.turn = null; }
      detachRun();
      el.sheet.hidden = true;
      el.backdrop.hidden = true;
      document.body.classList.remove('sheet-open');
      st.open = false;

      // Arrived here by URL: closing should leave the URL too, so the address
      // bar and the view agree. Fall back to the library when there is no
      // history to return to (a fresh tab opened straight on /builder).
      if (st.viaRoute) {
        if (window.history.length > 1) window.history.back();
        else window.location.href = '/';
      }
    }

    function isDraftDirty() {
      var current = el.draft.value.trim();
      if (!current) return false;
      return current !== (st.savedDraft || '').trim();
    }

    // setDraftValue writes the textarea and refreshes the highlight layer,
    // which only repaints on input events of its own.
    function setDraftValue(yaml) {
      el.draft.value = yaml;
      if (st.editor) st.editor.repaint();
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
      var run = { id: runID, started: Date.now(), state: null, steps: {} };
      st.run = run;
      el.runBar.hidden = false;
      el.runOpen.href = '/run/' + runID;
      setView('steps');
      updateRunBar();

      // Events from a superseded run must not write into the current one, so
      // every handler checks it is still the active run before touching state.
      function current() { return st.run === run; }

      function applyState(state) {
        if (!current() || !state) return;
        run.state = state;
        updateRunBar();
      }

      function applyStep(step) {
        if (!current() || !step) return;
        run.steps[step.index] = step;
        paintSteps();
      }

      // Reconcile against the server rather than trusting that every event
      // arrived. This is what makes the counter and the step list correct even
      // if the stream hiccups or the page attached late.
      function reconcile() {
        if (!current()) return;
        fetch('/run/' + runID + '/state')
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (res) {
            if (!res || !res.ok || !current()) return;
            run.state = res.state;
            (res.steps || []).forEach(function (step) { run.steps[step.index] = step; });
            updateRunBar();
            paintSteps();
          })
          .catch(function () { /* the stream is the primary path */ });
      }

      reconcile();
      st.runTimer = setInterval(function () {
        updateRunBar();
        // Cheap local endpoint; a slow tick is enough to self-heal.
        if (run.tick === undefined) run.tick = 0;
        if (++run.tick % 4 === 0) reconcile();
      }, 250);

      var source = new EventSource('/run/' + runID + '/events');
      st.runSource = source;

      source.addEventListener('state', function (e) {
        var ev = safeParse(e.data);
        if (ev) applyState(ev.state);
      });
      source.addEventListener('step', function (e) {
        var ev = safeParse(e.data);
        if (ev) applyStep(ev.step);
      });
      source.addEventListener('closed', function () {
        if (current()) detachRun();
      });
    }

    // paintSteps redraws the step list with whatever live statuses are known.
    function paintSteps() {
      var live = st.run ? { steps: Object.keys(st.run.steps).map(function (k) {
        return st.run.steps[k];
      }) } : null;
      renderSteps(el.steps, st.scenario, live);
    }

    function detachRun() {
      if (st.runSource) { st.runSource.close(); st.runSource = null; }
      if (st.runTimer) { clearInterval(st.runTimer); st.runTimer = 0; }
      if (el.runBar) el.runBar.hidden = true;
      st.run = null;
    }

    // paintRunControls decides which of Test run / Stop is offered. The
    // assistant and the user must not step the same run at once.
    function paintRunControls() {
      var assistantBusy = !!(el.composer && el.composer.isBusy());
      var hasRun = !!st.run;

      el.stop.hidden = !(assistantBusy && hasRun);
      el.run.hidden = assistantBusy && hasRun;
      el.run.disabled = assistantBusy;
      el.run.textContent = hasRun ? 'Run again' : 'Test run';

      // Manual stepping is only safe when nothing else is driving the run.
      if (el.runStep) el.runStep.disabled = assistantBusy;
      if (el.runPlay) el.runPlay.disabled = assistantBusy;
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
      paintRunControls();
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
          paintSteps();
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

    // Test run does the whole thing in one press: start the run and play it to
    // the end, in the sheet, without saying anything to the assistant. It is
    // refused while the assistant is driving a run of its own.
    function testRun() {
      if (el.composer && el.composer.isBusy() && st.run) return;

      applyDraft().then(function (ok) {
        if (!ok) return;
        return window.DL.postJSON('/run', { source: el.draft.value }).then(function (r) {
          if (!r.ok) { el.log.note('error', r.error); return; }
          attachRun(r.run_id);
          paintRunControls();
          // Play to the end. It stops by itself if an actor blocks, which the
          // run bar then shows.
          return window.DL.postJSON('/run/' + r.run_id + '/play').then(function (res) {
            if (res && res.stopped) el.log.note('warn', res.stopped);
          });
        });
      });
    }

    // Stop interrupts a run the assistant is stepping. The interruption reaches
    // the model through its own tool result, so it learns a person stepped in.
    function stopRun() {
      if (!st.run) return;
      window.DL.postJSON('/run/' + st.run.id + '/stop').then(function (res) {
        if (res && res.ok) {
          el.log.note('warn', 'You stopped the run. The assistant will be told on its next step.');
        } else if (res) {
          el.log.note('error', res.error || 'could not stop the run');
        }
        paintRunControls();
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
              setDraftValue(data.yaml || '');
              st.savedDraft = st.savedDraft || '';
              st.scenario = data.scenario;
              paintSteps();
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
          paintRunControls();
          return st.turn.promise
            .catch(function (err) {
              if (err && err.name === 'AbortError') return;
              el.log.note('error', String(err && err.message ? err.message : err));
            })
            .finally(function () {
              st.turn = null;
              paintRunControls();
            });
        }
      });

      el.close.addEventListener('click', function () { close(false); });
      el.draftApply.addEventListener('click', applyDraft);
      el.save.addEventListener('click', saveDraft);
      el.run.addEventListener('click', testRun);
      el.stop.addEventListener('click', stopRun);
      // Syntax highlighting and completions, the same editor the playground
      // uses. attachEditor owns the textarea's key handling from here.
      if (window.DL.attachEditor) {
        st.editor = window.DL.attachEditor(el.draft);
      }
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
        // The YAML completion menu gets Escape first: dismissing it is what the
        // user means there, not closing the whole builder.
        if (el.sheet.querySelector('.ac-menu:not([hidden])')) return;
        if (window.DL.dialogOpen && window.DL.dialogOpen()) return;
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

      // /builder opens the sheet on load. The library renders underneath, so
      // closing it leaves the user on the scenario list.
      if (window.DL_OPEN_BUILDER) {
        var opts = window.DL_OPEN_BUILDER;
        opts.viaRoute = true;
        open(opts);
      }
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
