/* The two assistant surfaces.

   - Builder: a modal sheet. Conversation on the left; on the right the
     scenario's steps as they are built, with the YAML source as a toggle. The
     step list and the run controls come from the shared run pane, which the
     playground uses too.
   - Discuss bubble: a small docked panel on a scenario or run page.

   Both share window.DL.chat for streaming, block rendering and queueing. */

(function () {
  'use strict';

  var C = window.DL.chat;

  // ------------------------------------------------------- builder (modal)

  var Builder = (function () {
    var el = {};
    var st = {
      open: false, session: null, scenarioID: '', savedDraft: '',
      turn: null, pane: null, editor: null, viaRoute: false, pendingPrompt: ''
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
      el.save = q('builder-save');
      el.suggestions = q('builder-suggestions');
      el.sourcePane = q('builder-source-pane');
      el.stepsPane = q('builder-steps-pane');
      el.viewToggle = q('builder-view-toggle');
      return true;
    }

    function setView(view) {
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
      // Opened by navigating to /builder, as opposed to from a button on a page
      // that is still sitting behind the sheet.
      st.viaRoute = !!opts.viaRoute;
      st.session = null;
      st.open = true;
      st.pane.detach();
      st.pane.setScenario(null);

      el.scenarioChip.hidden = !st.scenarioID;
      el.scenarioChip.textContent = st.scenarioID ? 'editing ' + st.scenarioID : '';
      el.log.clear();
      el.sheet.hidden = false;
      el.backdrop.hidden = false;
      document.body.classList.add('sheet-open');
      setView('steps');

      C.status().then(function (s) {
        el.model.textContent = s.model || 'no model selected';
        if (!s.ready) {
          el.log.note('error', 'The assistant is not configured. Open Settings, point it at an ' +
            'OpenAI-compatible endpoint and choose a model. You can still edit the draft by hand.');
        }
      });

      C.openSession('build', st.scenarioID, '').then(function (res) {
        if (!res || !res.ok) { el.log.note('error', (res && res.error) || 'could not start a session'); return; }
        st.session = res.session;
        if (res.draft) {
          setDraftValue(res.draft);
          st.savedDraft = res.resumed ? st.savedDraft : res.draft;
          st.pane.setScenario(res.scenario);
          paint(res.resumed ? isDraftDirty() : false);
        }
        if (res.resumed) {
          C.replay(el.log, res.transcript);
          el.log.note('info', 'Picked up where you left off.');
          // A run started before the reload is still there; show it again.
          if (res.run_id) st.pane.attach(res.run_id);
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
      st.pane.detach();
      el.sheet.hidden = true;
      el.backdrop.hidden = true;
      document.body.classList.remove('sheet-open');
      st.open = false;

      // Arrived here by URL: closing should leave the URL too, so the address
      // bar and the view agree.
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

    function paint(dirty) {
      el.draftState.textContent = dirty ? 'edited — apply to share' : 'in sync';
      el.draftState.className = 'draft-state' + (dirty ? ' is-dirty' : '');
    }

    // setDraftValue writes the textarea and refreshes the highlight layer,
    // which only repaints on input events of its own.
    function setDraftValue(yaml) {
      el.draft.value = yaml;
      if (st.editor) st.editor.repaint();
    }

    function applyDraft() {
      if (!st.session) return Promise.resolve(false);
      return window.DL.postJSON('/api/chat/' + st.session + '/draft', { yaml: el.draft.value })
        .then(function (res) {
          if (!res.ok) { el.log.note('error', 'Draft rejected: ' + res.error); return false; }
          st.pane.setScenario(res.scenario);
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

    function init() {
      if (!cache()) return;

      st.pane = window.DL.createRunPane({
        stepsEl: q('builder-steps'),
        bar: q('builder-run-bar'),
        statusEl: q('builder-run-status'),
        progressEl: q('builder-run-progress'),
        clockEl: q('builder-run-clock'),
        stepBtn: q('builder-run-step'),
        playBtn: q('builder-run-play'),
        closeBtn: q('builder-run-close'),
        openLink: q('builder-run-open'),
        runBtn: q('builder-run'),
        stopBtn: q('builder-stop'),
        onNote: function (kind, text) { el.log.note(kind, text); },
        // While the assistant drives a run, the only control offered is Stop:
        // the two must not step the same run at once.
        isBusy: function () { return !!(el.composer && el.composer.isBusy()); }
      });

      // Test run applies the draft first, so what runs is what is on screen.
      q('builder-run').addEventListener('click', function () {
        applyDraft().then(function (ok) {
          if (ok) st.pane.start(el.draft.value);
        });
      });

      el.composer = new C.Composer(el.input, el.send, el.status, el.log, {
        ready: function () { return !!st.session; },
        onSend: function (text) {
          st.turn = C.runTurn(st.session, text, el.log, {
            draft: function (data) {
              setDraftValue(data.yaml || '');
              st.pane.setScenario(data.scenario);
              paint(isDraftDirty());
            },
            run: function (data) { st.pane.attach(data.run_id); },
            saved: function (data) {
              st.savedDraft = el.draft.value;
              paint(false);
              el.log.note('ok', 'Saved to cases/' + data.text);
              window.dispatchEvent(new CustomEvent('dl-scenario-saved'));
            }
          });
          st.pane.paintControls();
          return st.turn.promise
            .catch(function (err) {
              if (err && err.name === 'AbortError') return;
              el.log.note('error', String(err && err.message ? err.message : err));
            })
            .finally(function () {
              st.turn = null;
              st.pane.paintControls();
            });
        }
      });

      el.close.addEventListener('click', function () { close(false); });
      el.draftApply.addEventListener('click', applyDraft);
      el.save.addEventListener('click', saveDraft);
      el.draft.addEventListener('input', function () { paint(isDraftDirty()); });

      // The same editor the playground uses: highlighting and completion.
      if (window.DL.attachEditor) st.editor = window.DL.attachEditor(el.draft);

      el.viewToggle.querySelectorAll('[data-view]').forEach(function (b) {
        b.addEventListener('click', function () { setView(b.dataset.view); });
      });

      el.backdrop.addEventListener('click', function (e) { e.preventDefault(); });

      document.addEventListener('keydown', function (e) {
        if (e.key !== 'Escape' || !st.open) return;
        // The completion menu and any open dialog get Escape first.
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
          open({ scenarioID: btn.dataset.scenario || '', prompt: btn.dataset.prompt || '' });
        });
      });

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
      C.openSession('discuss', st.scenarioID, st.runID).then(function (res) {
        if (!res || !res.ok) { el.log.note('error', (res && res.error) || 'could not start a session'); return; }
        st.session = res.session;
        if (res.resumed) C.replay(el.log, res.transcript);
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
            run: function (data) { el.log.note('info', 'Started run ' + data.run_id); }
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

      // Escape closes the bubble: nothing here is expensive to lose.
      document.addEventListener('keydown', function (e) {
        if (e.key !== 'Escape' || !st.open) return;
        if (document.body.classList.contains('sheet-open')) return;
        if (window.DL.dialogOpen && window.DL.dialogOpen()) return;
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
