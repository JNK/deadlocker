/* Syntax highlighting for InnoDB's deadlock report.

   `SHOW ENGINE INNODB STATUS` prints the most useful diagnostic MySQL has and
   the least readable: two transactions, their held and waited-for locks, and a
   hex dump of every locked record, all as one undifferentiated wall of text.
   Nearly everything anyone actually needs is on about six of those lines.

   So this classifies each line and lets the CSS bring the six forward and push
   the hex dump back. It is deliberately line-oriented and forgiving: an
   unrecognised line renders as plain text rather than breaking the view, which
   matters because this format is not documented and does change. */

(function () {
  'use strict';

  var esc = window.DL.escapeHTML;

  // Section markers: "*** (1) TRANSACTION:", "*** (2) HOLDS THE LOCK(S):",
  // "*** WE ROLL BACK TRANSACTION (2)".
  var RE_SECTION = /^\*\*\* (.+)$/;
  var RE_ROLLBACK = /WE ROLL BACK TRANSACTION/;
  var RE_WAITING = /WAITING FOR THIS LOCK/;
  var RE_HOLDS = /HOLDS THE LOCK/;

  var RE_TRX = /^TRANSACTION (\d+), (.+)$/;
  var RE_HEXDUMP = /^\s*\d+: len \d+;/;
  var RE_RECORD = /^Record lock, heap no|^PHYSICAL RECORD/;
  var RE_LOCKS = /^(RECORD LOCKS|TABLE LOCK)\b/;
  var RE_THREAD = /^MySQL thread id /;
  var RE_LOCKWAIT = /^(LOCK WAIT|mysql tables in use)/;
  var RE_RULE = /^-{3,}$/;
  var RE_HEADING = /^LATEST DETECTED DEADLOCK$/;

  // A statement line: the report prints the offending SQL on its own line after
  // the thread line, with no marker of any kind.
  var RE_SQL = /^(SELECT|INSERT|UPDATE|DELETE|REPLACE|SET|LOCK|BEGIN|COMMIT|ROLLBACK)\b/i;

  // Lock modes, longest first so "lock_mode X locks gap before rec" is not
  // matched as the shorter "lock_mode X" with a trailing tail.
  var LOCK_PHRASES = [
    'lock_mode X locks gap before rec insert intention',
    'lock_mode X locks rec but not gap',
    'lock_mode S locks rec but not gap',
    'lock_mode X locks gap before rec',
    'lock_mode S locks gap before rec',
    'lock mode S locks gap before rec',
    'lock_mode X insert intention',
    'lock_mode X',
    'lock_mode S',
    'lock mode S',
    'lock mode X',
    'lock mode IX',
    'lock mode IS'
  ];

  function markLock(html) {
    for (var i = 0; i < LOCK_PHRASES.length; i++) {
      var p = LOCK_PHRASES[i];
      var at = html.indexOf(p);
      if (at < 0) continue;
      return html.slice(0, at) + '<span class="dl-mode">' + p + '</span>' +
        markTail(html.slice(at + p.length));
    }
    return markTail(html);
  }

  // "waiting" at the end of a lock line is the single most important word in
  // the whole report: it is the difference between a lock held and a lock
  // wanted.
  function markTail(html) {
    return html.replace(/\bwaiting\b/, '<span class="dl-waiting">waiting</span>');
  }

  // Table and index names, so the eye can find which object is involved.
  function markObjects(html) {
    return html
      .replace(/index (\S+) of table/, 'index <span class="dl-ident">$1</span> of table')
      .replace(/table (&#39;|`)?([\w$]+)(&#39;|`)?\.(&#39;|`)?([\w$]+)/,
        'table <span class="dl-ident">$1$2$3.$4$5</span>');
  }

  function classify(line) {
    if (RE_HEADING.test(line)) return 'dl-heading';
    if (RE_RULE.test(line)) return 'dl-rule';
    if (RE_SECTION.test(line)) {
      if (RE_ROLLBACK.test(line)) return 'dl-section dl-rollback';
      if (RE_WAITING.test(line)) return 'dl-section dl-section-waiting';
      if (RE_HOLDS.test(line)) return 'dl-section dl-section-holds';
      return 'dl-section';
    }
    if (RE_HEXDUMP.test(line)) return 'dl-hex';
    if (RE_RECORD.test(line)) return 'dl-record';
    if (RE_LOCKS.test(line)) return 'dl-locks';
    if (RE_TRX.test(line)) return 'dl-trx';
    if (RE_THREAD.test(line)) return 'dl-thread';
    if (RE_LOCKWAIT.test(line)) return 'dl-meta';
    if (RE_SQL.test(line)) return 'dl-sql';
    return '';
  }

  function renderLine(line) {
    var cls = classify(line);
    var html = esc(line);

    if (cls === 'dl-locks') html = markObjects(markLock(html));
    else if (cls === 'dl-trx') {
      html = html.replace(/^TRANSACTION (\d+)/, 'TRANSACTION <span class="dl-ident">$1</span>');
    } else if (cls === 'dl-sql' && window.DL.highlightSQL) {
      html = window.DL.highlightSQL(line);
    }

    return '<div class="dl-line' + (cls ? ' ' + cls : '') + '">' + (html || '&nbsp;') + '</div>';
  }

  function highlightDeadlock(text) {
    return String(text || '').split('\n').map(renderLine).join('');
  }

  window.DL = window.DL || {};
  window.DL.highlightDeadlock = highlightDeadlock;
})();
