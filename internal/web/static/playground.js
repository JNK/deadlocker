/* Playground: validate, run and save an ad-hoc scenario. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var source = document.getElementById('pg-source');
  if (!source) return;

  var statusEl = document.getElementById('pg-status');

  function show(kind, html) {
    statusEl.className = 'pg-status is-' + kind;
    statusEl.innerHTML = html;
    statusEl.hidden = false;
  }

  function post(url, body) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) { return r.json(); });
  }

  document.getElementById('pg-validate').addEventListener('click', function () {
    fetch('/playground/validate', {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain' },
      body: source.value
    })
      .then(function (r) { return r.json(); })
      .then(function (res) {
        if (res.ok) {
          show('ok', '<strong>Valid.</strong> “' + esc(res.name) + '” — ' +
            res.actors + ' actors, ' + res.steps + ' steps, image <code>' + esc(res.image) + '</code>.');
        } else {
          show('error', '<strong>Not valid.</strong> ' + esc(res.error));
        }
      })
      .catch(function (err) { show('error', esc(String(err))); });
  });

  // ------------------------------------------------------------- the draft
  //
  // The editor's buffer, kept server-side as you type. It exists for one
  // moment in particular: pressing Run used to navigate to the run page and
  // leave the text behind, so the one thing you always want next -- to go back
  // and change a line -- was the one thing you could not do.
  //
  // Drafts are not versioned. Scenario history starts when a draft becomes a
  // file; while it is still being written, every keystroke as a revision would
  // not be history, it would be noise.
  var draft = window.DL_DRAFT || { id: '', scenarioID: '', path: '' };
  var draftEl = document.getElementById('pg-draft');
  var discardBtn = document.getElementById('pg-draft-discard');
  var draftSaved = draft.id ? source.value : null;
  var draftTimer = 0;
  var draftInFlight = null;

  function paintDraft(text, kind) {
    if (!draftEl) return;
    draftEl.hidden = !draft.id && !text;
    draftEl.textContent = text || '';
    draftEl.className = 'muted pg-draft' + (kind ? ' is-' + kind : '');
    if (discardBtn) discardBtn.hidden = !draft.id;
  }

  // saveDraft writes the buffer and resolves with the draft id. Calling it when
  // nothing has changed is free, which is what lets every path that leaves the
  // page call it without thinking about it.
  function saveDraft() {
    if (draftInFlight) return draftInFlight;
    if (draftSaved === source.value) return Promise.resolve(draft.id);
    // Editing a saved scenario and putting it back exactly as it was is not a
    // pending change; keeping a draft for it would mean offering the file back
    // to itself forever.
    if (window.DL_EDITING && source.value === savedSource) {
      return discardDraft().then(function () { return ''; });
    }
    var body = {
      id: draft.id,
      source: source.value,
      scenario_id: draft.scenarioID || '',
      path: draft.path || ''
    };
    paintDraft('saving draft…');
    draftInFlight = post('/api/drafts', body)
      .then(function (res) {
        draftInFlight = null;
        if (!res.ok) {
          paintDraft('draft not saved: ' + res.error, 'error');
          return draft.id;
        }
        draft.id = res.draft.id;
        draftSaved = body.source;
        rememberInURL();
        paintDraft('draft saved ' + new Date().toLocaleTimeString());
        window.dispatchEvent(new CustomEvent('dl-draft-saved', { detail: res.draft }));
        return draft.id;
      })
      .catch(function (err) {
        draftInFlight = null;
        paintDraft('draft not saved: ' + String(err), 'error');
        return draft.id;
      });
    return draftInFlight;
  }

  // rememberInURL puts the draft in the address bar, so a reload, a bookmark
  // and the browser's own back button all land on the same text.
  function rememberInURL() {
    if (!draft.id || !window.history || !window.history.replaceState) return;
    var url = new URL(window.location.href);
    if (url.searchParams.get('draft') === draft.id) return;
    url.searchParams.set('draft', draft.id);
    url.searchParams.delete('from');
    window.history.replaceState({}, '', url.toString());
  }

  function discardDraft() {
    var id = draft.id;
    if (!id) return Promise.resolve();
    draft.id = '';
    draftSaved = null;
    paintDraft('');
    return post('/api/drafts/' + encodeURIComponent(id) + '/discard', {})
      .then(function () {
        window.dispatchEvent(new CustomEvent('dl-draft-saved'));
      })
      .catch(function () {});
  }

  if (discardBtn) {
    discardBtn.addEventListener('click', function () {
      window.DL.confirm({
        title: 'Discard this draft?',
        body: window.DL_EDITING
          ? 'The unsaved changes go and the editor keeps showing them until you reload, ' +
            'where it will show what is on disk.'
          : 'The text stays in this editor until you leave the page, and then it is gone.',
        confirm: 'Discard it',
        cancel: 'Keep it',
        danger: true
      }).then(function (ok) { if (ok) discardDraft(); });
    });
  }

  source.addEventListener('input', function () {
    clearTimeout(draftTimer);
    draftTimer = setTimeout(saveDraft, 700);
  });
  paintDraft(draft.id ? 'draft saved' : '');

  var runBtn = document.getElementById('pg-run');
  // starting is held while a run is being created, so the validity check below
  // cannot re-enable the button underneath it.
  var starting = false;
  runBtn.addEventListener('click', function () {
    starting = true;
    runBtn.disabled = true;
    var original = runBtn.innerHTML;
    runBtn.textContent = 'Starting…';
    show('ok', 'Preparing the container and applying the schema…');

    // The draft is written before the run starts, so the run page can offer the
    // way back to the exact text it was started from.
    saveDraft()
      .then(function (draftID) {
        return post('/run', { source: source.value, draft_id: draftID || '' });
      })
      .then(function (res) {
        if (res.ok) {
          window.location.href = '/run/' + res.run_id;
          return;
        }
        show('error', '<strong>Could not start.</strong> ' + esc(res.error));
        starting = false;
        runBtn.disabled = false;
        runBtn.innerHTML = original;
      })
      .catch(function (err) {
        show('error', esc(String(err)));
        starting = false;
        runBtn.disabled = false;
        runBtn.innerHTML = original;
      });
  });

  // ------------------------------------------------------------- saving
  //
  // Saving is only offered when there is something to save. That removes the
  // "overwrite?" question entirely: an identical save was the only case where
  // the answer mattered, and every real save is kept as a version anyway.
  var saveBtn = document.getElementById('pg-save');
  var dirtyEl = document.getElementById('pg-dirty');
  // What is on disk. When the editor opened on a draft of unsaved changes, that
  // is not what the textarea holds -- and comparing the buffer with itself
  // reported "no changes" and disabled the Save button on exactly the edits that
  // most needed saving.
  var savedSource = typeof window.DL_FILE_SOURCE === 'string'
    ? window.DL_FILE_SOURCE : source.value;

  function isDirty() { return source.value !== savedSource; }

  function paintDirty() {
    var dirty = isDirty();
    saveBtn.disabled = !dirty;
    if (!dirtyEl) return;
    dirtyEl.textContent = dirty ? 'unsaved changes' : 'no changes';
    dirtyEl.className = dirty ? 'muted is-dirty' : 'muted';
  }
  source.addEventListener('input', paintDirty);
  paintDirty();

  saveBtn.addEventListener('click', function () {
    if (!isDirty()) return;
    // An empty path is fine: the server derives one from the scenario's name.
    save(document.getElementById('pg-path').value.trim());
  });

  // Ctrl/Cmd-S is what everyone reaches for in an editor.
  source.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && (e.key === 's' || e.key === 'S')) {
      e.preventDefault();
      if (isDirty()) save(document.getElementById('pg-path').value.trim());
    }
  });

  function save(path) {
    return post('/playground/save', { path: path, source: source.value, draft_id: draft.id })
      .then(function (res) {
        if (res.ok) {
          savedSource = source.value;
          // Saving is what turns a draft into a file with a history. Keeping
          // both would leave two copies claiming to be the current one.
          draft.id = '';
          draftSaved = null;
          paintDraft('');
          window.dispatchEvent(new CustomEvent('dl-draft-saved'));
          paintDirty();
          // The derived path is worth showing once it exists, so the next save
          // goes to the same file rather than deriving it again.
          var pathEl = document.getElementById('pg-path');
          if (!pathEl.value.trim()) pathEl.value = res.path;
          show('ok', (window.DL_EDITING ? 'Changes saved to ' : 'Saved to ') +
            '<code>cases/' + esc(res.path) + '</code>. ' +
            '<a href="/case/' + esc(res.id) + '">Open it</a>.');
          window.dispatchEvent(new CustomEvent('dl-scenario-saved'));
        } else {
          show('error', '<strong>Not saved.</strong> ' + esc(res.error));
        }
      })
      .catch(function (err) { show('error', esc(String(err))); });
  }

  // Syntax highlighting, indentation and completions. Tab handling lives in
  // the editor itself, alongside the completion menu that also wants the key.
  var editor = window.DL.attachEditor(source);

  // ------------------------------------------------------------ step pane

  // The same pane the builder uses, so a hand-written scenario can be run and
  // watched without leaving the editor.
  var pane = window.DL.createRunPane({
    stepsEl: document.getElementById('pg-steps'),
    bar: document.getElementById('pg-run-bar'),
    statusEl: document.getElementById('pg-run-status'),
    progressEl: document.getElementById('pg-run-progress'),
    clockEl: document.getElementById('pg-run-clock'),
    stepBtn: document.getElementById('pg-run-step'),
    playBtn: document.getElementById('pg-run-play'),
    closeBtn: document.getElementById('pg-run-close'),
    openLink: document.getElementById('pg-run-open'),
    runBtn: document.getElementById('pg-test-run'),
    onNote: function (kind, text) { show(kind === 'error' ? 'error' : 'ok', esc(text)); }
  });

  var validState = document.getElementById('pg-valid');

  // Reparse as you type, debounced, so the steps track the text without a
  // request per keystroke.
  var parseTimer = 0;
  function refresh() {
    clearTimeout(parseTimer);
    parseTimer = setTimeout(function () {
      fetch('/playground/validate', {
        method: 'POST',
        headers: { 'Content-Type': 'text/plain' },
        body: source.value
      })
        .then(function (r) { return r.json(); })
        .then(function (res) {
          if (res.ok && res.scenario) {
            pane.setScenario(res.scenario);
            validState.textContent = res.steps + ' steps · ' + res.actors + ' actors';
            validState.className = 'muted';
          } else {
            pane.setScenario({ valid: false, error: res.error });
            validState.textContent = 'does not parse';
            validState.className = 'muted is-invalid';
          }
          // A scenario that does not parse cannot be run, and offering the
          // button anyway only turns a visible state into an error message.
          if (!starting) {
            runBtn.disabled = !res.ok;
            runBtn.title = res.ok ? '' : 'The scenario does not parse yet';
          }
        })
        .catch(function () {});
    }, 350);
  }

  // ------------------------------------------------- follow the caret
  //
  // The editor and the step view are two halves of the same document, and
  // without this they are only related by the reader holding both in their
  // head. Moving the caret into a step lights that step up beside it.
  function followCaret() {
    pane.setCurrentStep(window.DL.yamlStepAtOffset(source.value, source.selectionStart));
  }
  ['click', 'keyup', 'input', 'focus'].forEach(function (ev) {
    source.addEventListener(ev, followCaret);
  });
  // selectionchange is the only event that fires for a caret moved by the
  // platform itself -- a drag, a context menu, an accessibility action.
  document.addEventListener('selectionchange', function () {
    if (document.activeElement === source) followCaret();
  });
  source.addEventListener('blur', function () {
    // Keep the highlight: the point is to know where you were, and it survives
    // clicking into the pane to read the step you just wrote.
  });

  source.addEventListener('input', refresh);
  refresh();
  followCaret();

  // Test run stays here and steps beside the text. The header's Run opens the
  // full run page; they are two different intentions and now two different
  // buttons, which they were not -- both carried the same id, so pressing Run
  // started a run in the pane and a second one on the page it navigated to.
  document.getElementById('pg-test-run').addEventListener('click', function () {
    saveDraft().then(function (draftID) {
      pane.start(source.value, draftID);
    });
  });

  // Leaving the page any other way -- a link, the back button, closing the tab
  // -- must not cost the buffer either. The debounce is what makes this
  // necessary: without it there is always a window of up to a second where the
  // newest keystrokes exist only here.
  window.addEventListener('pagehide', function () {
    if (draftSaved === source.value || !navigator.sendBeacon) return;
    if (window.DL_EDITING && source.value === savedSource) return;
    // A save already on its way will land on the same draft; beaconing beside
    // it would make a second one holding the same text.
    if (draftInFlight) return;
    try {
      navigator.sendBeacon('/api/drafts', new Blob([JSON.stringify({
        id: draft.id,
        source: source.value,
        scenario_id: draft.scenarioID || '',
        path: draft.path || ''
      })], { type: 'application/json' }));
    } catch (e) {}
  });
})();
