/* Settings: edit the LLM configuration, fetch the model list, restore an old
   configuration version. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var statusEl = document.getElementById('cfg-status');
  if (!statusEl) return;

  var clearKey = false;

  function show(kind, html) {
    statusEl.className = 'pg-status is-' + kind;
    statusEl.innerHTML = html;
    statusEl.hidden = false;
  }

  function val(id) { return document.getElementById(id).value.trim(); }

  // An empty optional field means "unset", which the server stores as null and
  // then simply does not send to the model.
  function optNum(id) {
    var raw = document.getElementById(id).value.trim();
    if (raw === '') return null;
    var n = parseFloat(raw);
    return isNaN(n) ? null : n;
  }
  function optInt(id) {
    var n = optNum(id);
    return n === null ? null : Math.round(n);
  }

  document.getElementById('cfg-fetch').addEventListener('click', function () {
    var base = val('cfg-base');
    if (!base) { show('error', 'Set a base URL first.'); return; }
    var hint = document.getElementById('cfg-model-hint');
    hint.textContent = 'fetching…';

    window.DL.postJSON('/api/models', { base_url: base, api_key: val('cfg-key') })
      .then(function (res) {
        if (!res.ok) {
          hint.textContent = 'Reads GET {base}/models from the endpoint above.';
          show('error', '<strong>Could not list models.</strong> ' + esc(res.error));
          return;
        }
        var select = document.getElementById('cfg-model');
        var current = select.value;
        select.innerHTML = res.models.map(function (m) {
          return '<option value="' + esc(m.id) + '">' + esc(m.id) + '</option>';
        }).join('');
        // Keep the current selection if the endpoint still offers it.
        if (current && res.models.some(function (m) { return m.id === current; })) {
          select.value = current;
        }
        hint.textContent = res.models.length + ' model(s) available.';
        show('ok', 'Found ' + res.models.length + ' model(s). Pick one and save.');
      })
      .catch(function (err) { show('error', esc(String(err))); });
  });

  var clearBtn = document.getElementById('cfg-clear-key');
  if (clearBtn) {
    clearBtn.addEventListener('click', function () {
      clearKey = true;
      document.getElementById('cfg-key').placeholder = 'will be cleared on save';
      document.getElementById('cfg-key').value = '';
      show('ok', 'The stored API key will be removed when you save.');
    });
  }

  document.getElementById('cfg-save').addEventListener('click', function () {
    var payload = {
      enabled: document.getElementById('cfg-enabled').checked,
      base_url: val('cfg-base'),
      api_key: val('cfg-key'),
      clear_api_key: clearKey,
      model: val('cfg-model'),
      temperature: optNum('cfg-temp'),
      max_tokens: optInt('cfg-max-tokens'),
      max_steps: optInt('cfg-max-steps'),
      note: ''
    };
    window.DL.postJSON('/api/settings', payload).then(function (res) {
      if (!res.ok) { show('error', '<strong>Not saved.</strong> ' + esc(res.error)); return; }
      clearKey = false;
      var msg = 'Saved as version ' + res.version + '.';
      if (!res.ready) {
        msg += ' The assistant stays hidden until it is enabled with a base URL and a model.';
      }
      show('ok', msg + ' <a href="/settings">Reload</a> to see it in the history.');
    });
  });

  document.querySelectorAll('.cfg-restore').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var version = Number(btn.dataset.version);
      if (!window.confirm('Restore configuration version ' + version + '? ' +
        'It is copied forward as a new version, so nothing is lost.')) return;
      window.DL.postJSON('/api/settings/restore', { version: version }).then(function (res) {
        if (!res.ok) { show('error', esc(res.error)); return; }
        window.location.reload();
      });
    });
  });

  var copy = document.getElementById('copy-mcp');
  if (copy) {
    copy.addEventListener('click', function () {
      var url = document.getElementById('mcp-url').textContent;
      navigator.clipboard.writeText(url).then(function () {
        copy.textContent = 'Copied';
        setTimeout(function () { copy.textContent = 'Copy'; }, 1600);
      });
    });
  }
})();
