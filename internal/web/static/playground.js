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

  var runBtn = document.getElementById('pg-run');
  runBtn.addEventListener('click', function () {
    runBtn.disabled = true;
    var original = runBtn.innerHTML;
    runBtn.textContent = 'Starting…';
    show('ok', 'Preparing the container and applying the schema…');

    post('/run', { source: source.value })
      .then(function (res) {
        if (res.ok) {
          window.location.href = '/run/' + res.run_id;
          return;
        }
        show('error', '<strong>Could not start.</strong> ' + esc(res.error));
        runBtn.disabled = false;
        runBtn.innerHTML = original;
      })
      .catch(function (err) {
        show('error', esc(String(err)));
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
  var savedSource = source.value;

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
    return post('/playground/save', { path: path, source: source.value })
      .then(function (res) {
        if (res.ok) {
          savedSource = source.value;
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
    runBtn: document.getElementById('pg-run'),
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

  document.getElementById('pg-run').addEventListener('click', function () {
    pane.start(source.value);
  });
})();
