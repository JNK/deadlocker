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

    // Dropping a folder's worth of scenarios at once should work, and each is
    // reported on its own: one bad file must not sink the rest.
    var results = [];
    var chain = Promise.resolve();
    list.forEach(function (file) {
      chain = chain.then(function () {
        return readFile(file)
          .then(function (text) { return window.DL.postJSON('/api/import', text, true); })
          .then(function (res) {
            results.push({ file: file.name, res: res });
          })
          .catch(function (err) {
            results.push({ file: file.name, res: { ok: false, error: String(err) } });
          });
      });
    });
    chain.then(function () { report(results); });
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
