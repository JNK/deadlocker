/* YAML syntax highlighting and a schema-aware completion menu.

   Hand-rolled on purpose: a real editor component (CodeMirror, Monaco) would
   mean a bundler and an npm tree, and this tool's whole shape is a single Go
   binary with no build step. The technique is the classic one — a highlighted
   <pre> sitting exactly behind a transparent <textarea>, with scroll kept in
   sync. Since the font is monospace, the caret position for the completion
   popup is arithmetic rather than DOM measurement. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  // ------------------------------------------------------------ highlighting

  var SQL_KEYWORDS = new RegExp(
    '\\b(' + [
      'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'REPLACE', 'FROM', 'WHERE', 'INTO',
      'VALUES', 'SET', 'AND', 'OR', 'NOT', 'NULL', 'JOIN', 'LEFT', 'RIGHT',
      'INNER', 'OUTER', 'ON', 'ORDER', 'GROUP', 'HAVING', 'BY', 'LIMIT', 'OFFSET',
      'FOR', 'SHARE', 'LOCK', 'IN', 'MODE', 'BEGIN', 'COMMIT', 'ROLLBACK',
      'START', 'TRANSACTION', 'SAVEPOINT', 'CREATE', 'TABLE', 'PRIMARY', 'KEY',
      'UNIQUE', 'FOREIGN', 'REFERENCES', 'CONSTRAINT', 'ENGINE', 'DEFAULT',
      'AUTO_INCREMENT', 'UNSIGNED', 'INT', 'INTEGER', 'BIGINT', 'SMALLINT',
      'TINYINT', 'VARCHAR', 'CHAR', 'BINARY', 'VARBINARY', 'TEXT', 'BLOB',
      'DATETIME', 'TIMESTAMP', 'DATE', 'TIME', 'DECIMAL', 'DOUBLE', 'FLOAT',
      'BETWEEN', 'EXISTS', 'AS', 'DROP', 'ALTER', 'ADD', 'INDEX', 'SHOW',
      'EXPLAIN', 'USE', 'DESC', 'ASC', 'IS', 'LIKE', 'COUNT', 'SUM', 'MIN',
      'MAX', 'AVG', 'DISTINCT', 'UNION', 'CASE', 'WHEN', 'THEN', 'ELSE', 'END',
      'SLEEP', 'SESSION', 'GLOBAL', 'ISOLATION', 'LEVEL', 'REPEATABLE', 'READ',
      'COMMITTED', 'UNCOMMITTED', 'SERIALIZABLE', 'UPDATE', 'DUPLICATE',
      'IGNORE', 'IF', 'CASCADE', 'RESTRICT', 'ENUM', 'BOOLEAN', 'JSON'
    ].join('|') + ')\\b', 'gi');

  function span(cls, text) {
    return '<span class="y-' + cls + '">' + esc(text) + '</span>';
  }

  function indentOf(line) {
    var i = 0;
    while (i < line.length && line[i] === ' ') i++;
    return i;
  }

  // highlightSQL marks keywords, string literals, numbers and comments inside a
  // stretch of SQL. Strings are extracted first so a keyword inside a literal is
  // left alone.
  function highlightSQL(text) {
    var literals = [];
    var masked = text.replace(/'([^'\\]|\\.)*'|"([^"\\]|\\.)*"/g, function (m) {
      literals.push(m);
      return '';
    });
    var out = esc(masked)
      .replace(SQL_KEYWORDS, function (m) { return '<span class="y-kw">' + m + '</span>'; })
      .replace(/\b(\d+)\b/g, '<span class="y-num">$1</span>');
    literals.forEach(function (lit) {
      out = out.replace('', '<span class="y-str">' + esc(lit) + '</span>');
    });
    return out;
  }

  function looksLikeSQL(s) {
    return /^\s*(SELECT|INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER|BEGIN|START|COMMIT|ROLLBACK|SET|SHOW|EXPLAIN|USE|LOCK|TRUNCATE|SAVEPOINT)\b/i.test(s);
  }

  function highlightValue(v) {
    if (v === '') return '';
    // Keep surrounding whitespace outside the token span so a span never
    // stretches across padding that belongs to the layout.
    var m = v.match(/^(\s*)([^]*?)(\s*)$/);
    var lead = m[1], t = m[2], trail = m[3];
    if (t === '') return esc(v);

    var inner;
    if (/^[|>][-+\d]*$/.test(t)) inner = span('blk', t);
    else if (/^(["']).*\1$/.test(t)) inner = span('str', t);
    else if (/^-?\d+(\.\d+)?$/.test(t)) inner = span('num', t);
    else if (/^(true|false|null|yes|no|on|off|~)$/i.test(t)) inner = span('bool', t);
    else if (/^[\[{].*[\]}]$/.test(t)) inner = span('str', t);
    else if (looksLikeSQL(t)) inner = highlightSQL(t);
    else inner = esc(t);

    return esc(lead) + inner + esc(trail);
  }

  // highlightYAML renders source as HTML. It is line-based and tracks block
  // scalars, so the SQL inside `sql: |` is highlighted as SQL rather than being
  // mistaken for more YAML.
  function highlightYAML(src) {
    var lines = String(src == null ? '' : src).split('\n');
    var blockIndent = -1; // indent of the key that opened a block scalar
    var out = [];

    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];

      if (blockIndent >= 0) {
        if (line.trim() === '' || indentOf(line) > blockIndent) {
          out.push(highlightSQL(line));
          continue;
        }
        blockIndent = -1;
      }

      if (line.trim() === '') { out.push(''); continue; }

      if (/^\s*#/.test(line)) { out.push(span('comment', line)); continue; }

      // A list item whose value is a block scalar indicator: "- |"
      var lb = line.match(/^(\s*)(-\s*)([|>][-+\d]*)\s*$/);
      if (lb) {
        blockIndent = indentOf(line);
        out.push(esc(lb[1]) + span('punct', lb[2]) + span('blk', lb[3]));
        continue;
      }

      // key: value, optionally preceded by a list dash
      var m = line.match(/^(\s*)(-\s+)?([A-Za-z_][\w.\- ]*?)(\s*:)(\s?)([^]*)$/);
      if (m) {
        var trailing = '';
        var value = m[6];
        // Split off a trailing comment, but not a '#' inside a quoted string.
        var ci = commentIndex(value);
        if (ci >= 0) {
          trailing = span('comment', value.slice(ci));
          value = value.slice(0, ci);
        }
        if (/^[|>][-+\d]*\s*$/.test(value.trim())) blockIndent = indentOf(line);
        out.push(
          esc(m[1]) +
          (m[2] ? span('punct', m[2]) : '') +
          span('key', m[3]) + span('punct', m[4]) + esc(m[5]) +
          highlightValue(value) + trailing);
        continue;
      }

      // A bare list item: "- SELECT ..." or "- some value"
      var li = line.match(/^(\s*)(-\s+)([^]*)$/);
      if (li) {
        out.push(esc(li[1]) + span('punct', li[2]) + highlightValue(li[3]));
        continue;
      }

      out.push(highlightValue(line));
    }
    return out.join('\n');
  }

  // commentIndex finds an unquoted '#' that starts a trailing comment.
  function commentIndex(s) {
    var quote = null;
    for (var i = 0; i < s.length; i++) {
      var c = s[i];
      if (quote) {
        if (c === quote) quote = null;
        continue;
      }
      if (c === '"' || c === "'") { quote = c; continue; }
      if (c === '#' && (i === 0 || /\s/.test(s[i - 1]))) return i;
    }
    return -1;
  }

  // ------------------------------------------------------------- completions

  var TOP_KEYS = [
    { text: 'name', detail: 'human readable title' },
    { text: 'category', detail: 'sidebar grouping; defaults to the folder' },
    { text: 'description', detail: 'markdown prose shown on the case page' },
    { text: 'tags', detail: 'list of labels' },
    { text: 'mysql', detail: 'server and session configuration' },
    { text: 'schema', detail: 'DDL applied before the run' },
    { text: 'seed', detail: 'rows inserted before the run' },
    { text: 'actors', detail: 'the simulated clients' },
    { text: 'steps', detail: 'the statement sequence' }
  ];

  var MYSQL_KEYS = [
    { text: 'image', detail: 'container image, e.g. mysql:8.4' },
    { text: 'isolation', detail: 'transaction isolation level' },
    { text: 'lock_wait_timeout', detail: 'innodb_lock_wait_timeout, seconds' },
    { text: 'deadlock_detect', detail: 'global innodb_deadlock_detect' },
    { text: 'prepared', detail: 'use prepared statements (binary protocol)' },
    { text: 'vars', detail: 'extra SET SESSION variables' }
  ];

  var ACTOR_KEYS = [
    { text: 'id', detail: 'short identifier used by steps' },
    { text: 'name', detail: 'display name' },
    { text: 'accent', detail: 'lane colour' }
  ];

  var STEP_KEYS = [
    { text: 'actor', detail: 'which actor runs this' },
    { text: 'label', detail: 'short title in the timeline' },
    { text: 'sql', detail: 'the statement' },
    { text: 'args', detail: 'bound parameters' },
    { text: 'note', detail: 'explanation shown with the step' },
    { text: 'expect', detail: 'ok | blocks | error | deadlock | timeout' }
  ];

  var VALUE_SETS = {
    expect: [
      { text: 'ok', detail: 'completes without error' },
      { text: 'blocks', detail: 'hits a lock wait' },
      { text: 'error', detail: 'any failure' },
      { text: 'deadlock', detail: 'errno 1213, chosen as victim' },
      { text: 'timeout', detail: 'errno 1205, lock wait timeout' }
    ],
    isolation: [
      { text: 'REPEATABLE READ', detail: 'default; gap locks are in play' },
      { text: 'READ COMMITTED', detail: 'essentially no gap locks' },
      { text: 'SERIALIZABLE', detail: 'plain SELECT becomes a locking read' },
      { text: 'READ UNCOMMITTED', detail: 'dirty reads' }
    ],
    accent: [
      { text: 'blue' }, { text: 'amber' }, { text: 'violet' },
      { text: 'teal' }, { text: 'rose' }
    ],
    image: [
      { text: 'mysql:8.4', detail: 'current LTS' },
      { text: 'mysql:8.0', detail: 'most widely deployed' },
      { text: 'mysql:9.4', detail: 'innovation release' }
    ],
    deadlock_detect: [{ text: 'true' }, { text: 'false' }],
    prepared: [{ text: 'true', detail: 'binary protocol' }, { text: 'false' }]
  };

  // actorIDs scrapes the ids declared in the document so `actor:` can complete
  // against what the scenario actually defines.
  function actorIDs(src) {
    var ids = [];
    var inActors = false;
    src.split('\n').forEach(function (line) {
      if (/^actors\s*:/.test(line)) { inActors = true; return; }
      if (/^[A-Za-z_]/.test(line)) { inActors = false; return; }
      if (!inActors) return;
      var m = line.match(/^\s*-?\s*id\s*:\s*(\S+)/);
      if (m) ids.push({ text: m[1].replace(/['"]/g, ''), detail: 'actor' });
    });
    return ids;
  }

  // sectionAt walks backwards to find which top-level block the cursor is in.
  function sectionAt(lines, lineNo) {
    for (var i = lineNo; i >= 0; i--) {
      var m = lines[i].match(/^([A-Za-z_][\w]*)\s*:/);
      if (m) return m[1];
    }
    return '';
  }

  function completionsFor(src, caret) {
    var before = src.slice(0, caret);
    var lines = before.split('\n');
    var lineNo = lines.length - 1;
    var line = lines[lineNo];
    var allLines = src.split('\n');

    // Completing a value: "<key>: <partial>"
    var vm = line.match(/^\s*-?\s*([A-Za-z_][\w]*)\s*:\s*(.*)$/);
    if (vm && vm[2] !== '' || (vm && /:\s$/.test(line))) {
      var key = vm[1];
      var partial = vm[2];
      var set = null;
      if (key === 'actor') set = actorIDs(src);
      else if (VALUE_SETS[key]) set = VALUE_SETS[key];
      if (set) {
        return {
          items: filterItems(set, partial),
          replaceFrom: caret - partial.length,
          kind: 'value'
        };
      }
      return null;
    }

    // Completing a key.
    var km = line.match(/^(\s*)(-\s+)?([A-Za-z_][\w]*)?$/);
    if (!km) return null;
    var prefix = km[3] || '';
    var indent = km[1].length;
    var section = sectionAt(allLines, lineNo - 1);

    var pool;
    if (indent === 0) pool = TOP_KEYS;
    else if (section === 'mysql') pool = MYSQL_KEYS;
    else if (section === 'actors') pool = ACTOR_KEYS;
    else if (section === 'steps') pool = STEP_KEYS;
    else pool = TOP_KEYS;

    return {
      items: filterItems(pool, prefix),
      replaceFrom: caret - prefix.length,
      kind: 'key'
    };
  }

  function filterItems(items, prefix) {
    var p = (prefix || '').trim().toLowerCase();
    if (!p) return items.slice(0, 12);
    return items.filter(function (it) {
      return it.text.toLowerCase().indexOf(p) === 0;
    }).slice(0, 12);
  }

  // --------------------------------------------------------------- attaching

  function attachEditor(textarea) {
    var wrap = document.createElement('div');
    wrap.className = 'editor-wrap';
    textarea.parentNode.insertBefore(wrap, textarea);

    var pre = document.createElement('pre');
    pre.className = 'editor-highlight';
    pre.setAttribute('aria-hidden', 'true');
    var code = document.createElement('code');
    pre.appendChild(code);

    wrap.appendChild(pre);
    wrap.appendChild(textarea);

    var menu = document.createElement('div');
    menu.className = 'ac-menu';
    menu.hidden = true;
    wrap.appendChild(menu);

    var state = { items: [], active: 0, replaceFrom: 0, open: false };

    function paint() {
      // The trailing newline keeps the last line measurable and stops the
      // highlight layer from collapsing shorter than the textarea.
      code.innerHTML = highlightYAML(textarea.value) + '\n';
      pre.scrollTop = textarea.scrollTop;
      pre.scrollLeft = textarea.scrollLeft;
    }

    function metrics() {
      var cs = getComputedStyle(textarea);
      var probe = document.createElement('span');
      probe.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;font:' + cs.font;
      probe.textContent = '0000000000';
      document.body.appendChild(probe);
      var charW = probe.getBoundingClientRect().width / 10;
      document.body.removeChild(probe);
      return {
        charW: charW,
        lineH: parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.6,
        padL: parseFloat(cs.paddingLeft),
        padT: parseFloat(cs.paddingTop)
      };
    }

    function placeMenu() {
      var before = textarea.value.slice(0, textarea.selectionStart);
      var lines = before.split('\n');
      var row = lines.length - 1;
      var col = lines[row].length;
      var m = metrics();
      menu.style.left = Math.max(4, m.padL + col * m.charW - textarea.scrollLeft) + 'px';
      menu.style.top = (m.padT + (row + 1) * m.lineH - textarea.scrollTop + 4) + 'px';
    }

    function renderMenu() {
      menu.innerHTML = state.items.map(function (it, i) {
        return '<div class="ac-item' + (i === state.active ? ' is-active' : '') + '" data-i="' + i + '">' +
          '<span class="ac-text">' + esc(it.text) + '</span>' +
          (it.detail ? '<span class="ac-detail">' + esc(it.detail) + '</span>' : '') +
          '</div>';
      }).join('');
    }

    function openMenu() {
      var res = completionsFor(textarea.value, textarea.selectionStart);
      if (!res || !res.items.length) { closeMenu(); return; }
      state.items = res.items;
      state.replaceFrom = res.replaceFrom;
      state.kind = res.kind;
      state.active = 0;
      state.open = true;
      renderMenu();
      placeMenu();
      menu.hidden = false;
    }

    function closeMenu() {
      state.open = false;
      menu.hidden = true;
    }

    function accept() {
      var it = state.items[state.active];
      if (!it) return;
      var caret = textarea.selectionStart;
      var insert = it.text + (state.kind === 'key' ? ': ' : '');
      textarea.value = textarea.value.slice(0, state.replaceFrom) + insert + textarea.value.slice(caret);
      var pos = state.replaceFrom + insert.length;
      textarea.selectionStart = textarea.selectionEnd = pos;
      closeMenu();
      paint();
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    }

    textarea.addEventListener('input', function () {
      paint();
      if (state.open) openMenu();
    });
    textarea.addEventListener('scroll', function () {
      pre.scrollTop = textarea.scrollTop;
      pre.scrollLeft = textarea.scrollLeft;
      if (state.open) placeMenu();
    });
    textarea.addEventListener('blur', function () {
      // Delay so a click on the menu still registers.
      setTimeout(closeMenu, 120);
    });

    // A click anywhere else dismisses the menu. Blur alone does not cover it:
    // clicking a non-focusable area leaves the textarea focused and the popup
    // stranded on screen.
    document.addEventListener('mousedown', function (e) {
      if (!state.open) return;
      if (menu.contains(e.target) || e.target === textarea) return;
      closeMenu();
    }, true);
    window.addEventListener('resize', closeMenu);

    menu.addEventListener('mousedown', function (e) {
      var item = e.target.closest('.ac-item');
      if (!item) return;
      e.preventDefault();
      state.active = Number(item.dataset.i);
      accept();
    });

    textarea.addEventListener('keydown', function (e) {
      if (state.open) {
        if (e.key === 'ArrowDown') {
          e.preventDefault();
          state.active = (state.active + 1) % state.items.length;
          renderMenu();
          return;
        }
        if (e.key === 'ArrowUp') {
          e.preventDefault();
          state.active = (state.active - 1 + state.items.length) % state.items.length;
          renderMenu();
          return;
        }
        if (e.key === 'Enter' || e.key === 'Tab') {
          e.preventDefault();
          accept();
          return;
        }
        if (e.key === 'Escape') {
          e.preventDefault();
          closeMenu();
          return;
        }
      }

      // Ctrl/Cmd+Space opens completions explicitly.
      if (e.key === ' ' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        openMenu();
        return;
      }

      if (e.key === 'Tab') {
        e.preventDefault();
        insertAtCaret(textarea, '  ');
        paint();
        return;
      }

      // Enter keeps the current indentation, and adds a level after a key that
      // opens a block.
      if (e.key === 'Enter') {
        var caret = textarea.selectionStart;
        var lineStart = textarea.value.lastIndexOf('\n', caret - 1) + 1;
        var current = textarea.value.slice(lineStart, caret);
        var ind = (current.match(/^\s*/) || [''])[0];
        if (/:\s*[|>][-+\d]*\s*$/.test(current) || /:\s*$/.test(current)) ind += '  ';
        else if (/^\s*-\s/.test(current)) ind += '  ';
        e.preventDefault();
        insertAtCaret(textarea, '\n' + ind);
        paint();
        return;
      }

      // Typing a letter at the start of a token offers completions.
      if (/^[a-zA-Z_]$/.test(e.key) && !e.ctrlKey && !e.metaKey && !e.altKey) {
        setTimeout(openMenu, 0);
      }
    });

    paint();
    return { repaint: paint };
  }

  function insertAtCaret(textarea, text) {
    var start = textarea.selectionStart;
    var end = textarea.selectionEnd;
    textarea.value = textarea.value.slice(0, start) + text + textarea.value.slice(end);
    textarea.selectionStart = textarea.selectionEnd = start + text.length;
  }

  // Read-only highlighted blocks.
  function highlightViews(root) {
    (root || document).querySelectorAll('[data-yaml-view]').forEach(function (el) {
      el.innerHTML = highlightYAML(el.textContent);
    });
  }

  window.DL.highlightYAML = highlightYAML;
  window.DL.attachEditor = attachEditor;
  window.DL.highlightYAMLViews = highlightViews;
  // The deadlock report contains the offending statements, and they should read
  // the same there as they do in a scenario's source.
  window.DL.highlightSQL = highlightSQL;
  // Exposed so the completion rules can be exercised without a browser.
  window.DL.yamlCompletions = completionsFor;

  document.addEventListener('DOMContentLoaded', function () { highlightViews(document); });
  if (document.readyState !== 'loading') highlightViews(document);
})();
