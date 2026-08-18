/* Importing scenarios by dropping files on the page.

   A scenario is a single YAML file, which makes it the most shareable thing in
   the app — paste it in a ticket, attach it to a message. Drag and drop is the
   shortest path from "someone sent me this" to "I am watching it deadlock".

   A bundle (.deadlocker.json) is the same thing plus the run history, for when
   the point is "here is what it did on my machine". */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var zone = document.querySelector('[data-drop-import]');
  if (!zone) return;

  var overlay = null;
  // Counted rather than set: dragging over a child fires dragleave on the
  // parent, and a boolean flickers.
  var depth = 0;

  function buildOverlay() {
    if (overlay) return overlay;
    overlay = document.createElement('div');
    overlay.className = 'drop-overlay';
    overlay.innerHTML =
      '<div class="drop-card">' +
      '<svg viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" ' +
      'stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M12 16V4M7 9l5-5 5 5M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/></svg>' +
      '<strong>Drop to import</strong>' +
      '<span>A scenario <code>.yaml</code>, or a <code>.deadlocker.json</code> bundle</span>' +
      '</div>';
    document.body.appendChild(overlay);
    return overlay;
  }

  function showOverlay(on) {
    buildOverlay().classList.toggle('is-on', on);
  }

  // hasFiles keeps the overlay from appearing when dragging text or a link.
  function hasFiles(e) {
    var dt = e.dataTransfer;
    if (!dt) return false;
    if (dt.types) {
      return Array.prototype.indexOf.call(dt.types, 'Files') !== -1;
    }
    return true;
  }

  window.addEventListener('dragenter', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    depth++;
    showOverlay(true);
  });
  window.addEventListener('dragover', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  window.addEventListener('dragleave', function () {
    depth = Math.max(0, depth - 1);
    if (!depth) showOverlay(false);
  });

  window.addEventListener('drop', function (e) {
    if (!hasFiles(e)) return;
    e.preventDefault();
    depth = 0;
    showOverlay(false);
    importFiles(e.dataTransfer.files);
  });

  // The file picker is the same path, for anyone who would rather click.
  var picker = document.getElementById('import-file');
  var pickBtn = document.getElementById('import-open');
  if (pickBtn && picker) {
    pickBtn.addEventListener('click', function () { picker.click(); });
    picker.addEventListener('change', function () {
      importFiles(picker.files);
      picker.value = '';
    });
  }

  function importFiles(files) {
    var list = Array.prototype.slice.call(files || []);
    if (!list.length) return;

    // Read and inspect everything first, so the confirmation can say what is
    // actually in the files. Nothing is written until it is accepted.
    var texts = [];
    var chain = Promise.resolve();
    list.forEach(function (file) {
      chain = chain.then(function () {
        return readFile(file)
          .then(function (text) {
            return window.DL.postJSON('/api/import/inspect', text, true)
              .then(function (info) { texts.push({ file: file.name, text: text, info: info }); });
          })
          .catch(function (err) {
            texts.push({ file: file.name, info: { ok: false, error: String(err) } });
          });
      });
    });

    chain.then(function () {
      return confirmImport(texts).then(function (go) {
        if (!go) return;
        return writeAll(texts.filter(function (t) { return t.info && t.info.ok; }));
      });
    });
  }

  // confirmImport shows what each file holds and waits for a decision.
  function confirmImport(items) {
    var good = items.filter(function (i) { return i.info && i.info.ok; });
    var bad = items.filter(function (i) { return !i.info || !i.info.ok; });

    var body = '';
    good.forEach(function (i) {
      var d = i.info;
      var facts = [
        d.steps + ' step' + (d.steps === 1 ? '' : 's'),
        d.actors + ' actor' + (d.actors === 1 ? '' : 's')
      ];
      if (d.image) facts.push(d.image);
      if (d.isolation) facts.push(d.isolation);
      if (d.docs) facts.push(d.docs + ' doc link' + (d.docs === 1 ? '' : 's'));

      // The counts that make a bundle different from a bare scenario.
      var extras = [];
      if (d.runs) extras.push(d.runs + ' recorded run' + (d.runs === 1 ? '' : 's'));
      if (d.versions) extras.push(d.versions + ' version' + (d.versions === 1 ? '' : 's'));

      body += '<div class="import-card">' +
        '<div class="import-card-head">' +
        '<span class="import-kind">' + esc(d.kind) + '</span>' +
        '<strong>' + esc(d.name) + '</strong></div>' +
        '<div class="import-facts">' + esc(facts.join(' · ')) + '</div>' +
        (d.tags && d.tags.length
          ? '<div class="import-tags">' + d.tags.map(function (t) {
              return '<span class="tag">' + esc(t) + '</span>';
            }).join('') + '</div>'
          : '') +
        (extras.length
          ? '<div class="import-extras">carries ' + esc(extras.join(' and ')) +
            ' — read for context, not written to the library</div>'
          : '') +
        (d.warnings && d.warnings.length
          ? '<div class="import-warn">' + d.warnings.map(esc).join('<br>') + '</div>'
          : '') +
        '<div class="import-dest">will be written to <code>cases/' + esc(d.path) + '</code></div>' +
        '</div>';
    });

    if (bad.length) {
      body += '<ul class="import-list is-bad">' + bad.map(function (i) {
        return '<li><strong>' + esc(i.file) + '</strong><span>' +
          esc((i.info && i.info.error) || 'could not be read') + '</span></li>';
      }).join('') + '</ul>';
    }

    if (!good.length) {
      return window.DL.confirm({
        title: 'Nothing here can be imported',
        bodyHTML: body,
        confirm: 'Close',
        cancel: ''
      }).then(function () { return false; });
    }

    return window.DL.confirm({
      title: good.length === 1 ? 'Import this scenario?' : 'Import ' + good.length + ' scenarios?',
      bodyHTML: body,
      confirm: 'Import',
      cancel: 'Cancel'
    });
  }

  function writeAll(items) {
    var results = [];
    var chain = Promise.resolve();
    items.forEach(function (i) {
      chain = chain.then(function () {
        return window.DL.postJSON('/api/import', i.text, true)
          .then(function (res) { results.push({ file: i.file, res: res }); })
          .catch(function (err) {
            results.push({ file: i.file, res: { ok: false, error: String(err) } });
          });
      });
    });
    return chain.then(function () { report(results); });
  }

  // The empty library offers the built-ins directly; Settings owns it after
  // that, so this button only exists while there is nothing to lose.
  var emptyBtn = document.getElementById('builtins-import-empty');
  if (emptyBtn) {
    emptyBtn.addEventListener('click', function () {
      emptyBtn.disabled = true;
      emptyBtn.textContent = 'Importing…';
      window.DL.postJSON('/api/builtins', {}).then(function (res) {
        if (!res.ok) {
          emptyBtn.disabled = false;
          emptyBtn.textContent = 'Import the built-in scenarios';
          return;
        }
        window.location.reload();
      });
    });
  }

  function readFile(file) {
    if (file.text) return file.text();
    return new Promise(function (resolve, reject) {
      var fr = new FileReader();
      fr.onload = function () { resolve(String(fr.result)); };
      fr.onerror = function () { reject(fr.error); };
      fr.readAsText(file);
    });
  }

  function report(results) {
    var ok = results.filter(function (r) { return r.res && r.res.ok; });
    var bad = results.filter(function (r) { return !r.res || !r.res.ok; });

    var body = '';
    if (ok.length) {
      body += '<ul class="import-list">' + ok.map(function (r) {
        return '<li><strong>' + esc(r.res.name) + '</strong><span>' + esc(r.res.path) + '</span></li>';
      }).join('') + '</ul>';
    }
    if (bad.length) {
      body += '<ul class="import-list is-bad">' + bad.map(function (r) {
        return '<li><strong>' + esc(r.file) + '</strong><span>' +
          esc((r.res && r.res.error) || 'could not be read') + '</span></li>';
      }).join('') + '</ul>';
    }

    var title = !bad.length
      ? (ok.length === 1 ? 'Scenario imported' : ok.length + ' scenarios imported')
      : (!ok.length ? 'Nothing could be imported' : 'Imported with problems');

    window.DL.confirm({
      title: title,
      bodyHTML: body,
      confirm: ok.length ? 'Reload the library' : 'Close',
      cancel: ok.length ? 'Stay here' : ''
    }).then(function (reload) {
      if (reload && ok.length) window.location.reload();
    });
  }
})();
