/* A small Markdown renderer for chat messages.

   Deliberately the same subset the Go renderer handles, because assistant
   replies use exactly these: headings, paragraphs, bold, italic, inline code,
   fenced blocks, lists, blockquotes, tables and links.

   It runs on partial text while a reply streams, so it has to tolerate
   half-finished syntax: an unterminated code fence renders as an open block
   rather than swallowing the rest of the message. Everything is escaped before
   any markup is inserted. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;
  var CODE_MARK = '';

  function inline(text) {
    // Pull code spans out first so their contents are never reinterpreted.
    var codes = [];
    var masked = '';
    var inCode = false;
    text = text.split(CODE_MARK).join('');

    for (var i = 0; i < text.length; i++) {
      if (text[i] === '`') {
        if (inCode) { inCode = false; masked += CODE_MARK; }
        else { inCode = true; codes.push(''); }
        continue;
      }
      if (inCode) codes[codes.length - 1] += text[i];
      else masked += text[i];
    }
    if (inCode) {
      // Unterminated while streaming: show it as an open code span.
      masked += CODE_MARK;
    }

    var out = esc(masked)
      .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+|\/[^)\s]*)\)/g,
        '<a href="$2" rel="noopener noreferrer" target="_blank">$1</a>')
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
      .replace(/(^|[\s(])\*([^*\n]+)\*($|[\s).,;:!?])/g, '$1<em>$2</em>$3');

    codes.forEach(function (c) {
      out = out.replace(CODE_MARK, '<code>' + esc(c) + '</code>');
    });
    return out;
  }

  function tableRow(line) {
    var cells = line.trim().replace(/^\||\|$/g, '').split('|');
    return cells.map(function (c) { return c.trim(); });
  }

  function isDivider(line) {
    return /^\s*\|?[\s:-]*-[\s:|-]*\|?\s*$/.test(line) && line.indexOf('-') >= 0;
  }

  function render(src) {
    var lines = String(src == null ? '' : src).replace(/\r\n/g, '\n').split('\n');
    var out = [];
    var para = [];
    var list = null; // 'ul' | 'ol'

    function flushPara() {
      if (!para.length) return;
      out.push('<p>' + inline(para.join(' ')) + '</p>');
      para = [];
    }
    function closeList() {
      if (list) { out.push('</' + list + '>'); list = null; }
    }

    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];

      // Fenced code, tolerant of a missing closing fence mid-stream.
      var fence = line.match(/^\s*```\s*(\w*)\s*$/);
      if (fence) {
        flushPara();
        closeList();
        var body = [];
        i++;
        while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) {
          body.push(lines[i]);
          i++;
        }
        out.push('<pre class="md-code"><code>' + esc(body.join('\n')) + '</code></pre>');
        continue;
      }

      // Tables: a header row followed by a divider.
      if (line.indexOf('|') >= 0 && i + 1 < lines.length && isDivider(lines[i + 1])) {
        flushPara();
        closeList();
        var head = tableRow(line);
        i += 2;
        var rows = [];
        while (i < lines.length && lines[i].indexOf('|') >= 0 && lines[i].trim() !== '') {
          rows.push(tableRow(lines[i]));
          i++;
        }
        i--;
        var html = '<div class="md-table-wrap"><table class="md-table"><thead><tr>';
        head.forEach(function (h) { html += '<th>' + inline(h) + '</th>'; });
        html += '</tr></thead><tbody>';
        rows.forEach(function (r) {
          html += '<tr>';
          r.forEach(function (c) { html += '<td>' + inline(c) + '</td>'; });
          html += '</tr>';
        });
        out.push(html + '</tbody></table></div>');
        continue;
      }

      if (line.trim() === '') { flushPara(); closeList(); continue; }

      var heading = line.match(/^(#{1,6})\s+(.*)$/);
      if (heading) {
        flushPara();
        closeList();
        var level = Math.min(heading[1].length + 2, 6);
        out.push('<h' + level + '>' + inline(heading[2]) + '</h' + level + '>');
        continue;
      }

      var quote = line.match(/^>\s?(.*)$/);
      if (quote) {
        flushPara();
        closeList();
        out.push('<blockquote>' + inline(quote[1]) + '</blockquote>');
        continue;
      }

      var ul = line.match(/^\s*[-*+]\s+(.*)$/);
      if (ul) {
        flushPara();
        if (list !== 'ul') { closeList(); out.push('<ul>'); list = 'ul'; }
        out.push('<li>' + inline(ul[1]) + '</li>');
        continue;
      }

      var ol = line.match(/^\s*\d+[.)]\s+(.*)$/);
      if (ol) {
        flushPara();
        if (list !== 'ol') { closeList(); out.push('<ol>'); list = 'ol'; }
        out.push('<li>' + inline(ol[1]) + '</li>');
        continue;
      }

      closeList();
      para.push(line.trim());
    }

    flushPara();
    closeList();
    return out.join('');
  }

  window.DL = window.DL || {};
  window.DL.renderMarkdown = render;
})();
