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

  document.getElementById('pg-save').addEventListener('click', function () {
    var path = document.getElementById('pg-path').value.trim();
    if (!path) {
      show('error', 'Give the file a name first, for example <code>my-scenarios/experiment.yaml</code>.');
      return;
    }
    // Editing writes back over the same file, so make that explicit once.
    var ask = window.DL_EDITING
      ? window.DL.confirm({
          title: 'Overwrite ' + path + '?',
          body: 'The file on disk is replaced with what is in the editor.',
          confirm: 'Save changes',
          cancel: 'Cancel'
        })
      : Promise.resolve(true);

    ask.then(function (ok) {
      if (!ok) return;
      return save(path);
    });
  });

  function save(path) {
    return post('/playground/save', { path: path, source: source.value })
      .then(function (res) {
        if (res.ok) {
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
  window.DL.attachEditor(source);
})();
