/* Exercises the InnoDB deadlock report highlighter outside a browser.

   Run: node hack/deadlock_test.js

   The input is a real report captured from MySQL 8.4. The format is undocumented
   and does change between versions, so the important property is not that every
   line is classified but that nothing is *lost*: an unrecognised line must still
   render, and the text must survive the round trip unchanged. */

const fs = require('fs');
const path = require('path');
const assert = require('assert');

global.window = {
  DL: {
    escapeHTML: function (s) {
      return String(s == null ? '' : s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
  }
};

const src = fs.readFileSync(path.join(__dirname, '..', 'internal/web/static/deadlock.js'), 'utf8');
// eslint-disable-next-line no-eval
eval(src);
const hl = window.DL.highlightDeadlock;

const REPORT = [
  'LATEST DETECTED DEADLOCK',
  '------------------------',
  '2026-08-18 00:10:59 281471939899136',
  '*** (1) TRANSACTION:',
  'TRANSACTION 1834, ACTIVE 2 sec starting index read',
  'mysql tables in use 1, locked 1',
  'LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s), undo log entries 1',
  'MySQL thread id 13, OS thread handle 281472430030592, query id 64 192.168.215.1 root updating',
  'UPDATE accounts SET cents = cents + 100 WHERE id = 20',
  '',
  '*** (1) HOLDS THE LOCK(S):',
  'RECORD LOCKS space id 3 page no 4 n bits 72 index PRIMARY of table `dl_x`.`accounts` trx id 1834 lock_mode X locks rec but not gap',
  'Record lock, heap no 2 PHYSICAL RECORD: n_fields 5; compact format; info bits 0',
  ' 0: len 4; hex 8000000a; asc     ;;',
  '*** (1) WAITING FOR THIS LOCK TO BE GRANTED:',
  'RECORD LOCKS space id 3 page no 4 n bits 72 index PRIMARY of table `dl_x`.`accounts` trx id 1834 lock_mode X locks rec but not gap waiting',
  '*** WE ROLL BACK TRANSACTION (2)'
].join('\n');

let passed = 0, failed = 0;
function check(name, fn) {
  try { fn(); console.log('  ok   ' + name); passed++; }
  catch (e) { console.log('  FAIL ' + name + '\n       ' + e.message); failed++; }
}

// stripTags recovers the plain text, to prove nothing was dropped.
function text(html) {
  return html
    .replace(/<div class="dl-line[^"]*">/g, '')
    .replace(/<\/div>/g, '\n')
    .replace(/<[^>]+>/g, '')
    .replace(/&nbsp;/g, '')
    .replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"').replace(/&#39;/g, "'")
    .replace(/\n$/, '');
}

console.log('deadlock report highlighter');

check('every line survives the round trip', function () {
  assert.strictEqual(text(hl(REPORT)), REPORT);
});

check('one div per line', function () {
  const n = (hl(REPORT).match(/class="dl-line/g) || []).length;
  assert.strictEqual(n, REPORT.split('\n').length);
});

check('section markers are marked', function () {
  assert.ok(hl('*** (1) TRANSACTION:').includes('dl-section'));
});

check('holds and waiting sections are distinguished', function () {
  assert.ok(hl('*** (1) HOLDS THE LOCK(S):').includes('dl-section-holds'));
  assert.ok(hl('*** (1) WAITING FOR THIS LOCK TO BE GRANTED:').includes('dl-section-waiting'));
});

check('the rollback line is called out', function () {
  assert.ok(hl('*** WE ROLL BACK TRANSACTION (2)').includes('dl-rollback'));
});

check('the hex dump is classified separately', function () {
  assert.ok(hl(' 0: len 4; hex 8000000a; asc     ;;').includes('dl-hex'));
});

check('the lock mode is highlighted', function () {
  const out = hl('RECORD LOCKS space id 3 index PRIMARY of table `a`.`b` trx id 1 lock_mode X locks rec but not gap');
  assert.ok(out.includes('<span class="dl-mode">lock_mode X locks rec but not gap</span>'),
    'expected the full lock phrase, got: ' + out);
});

check('the longest lock phrase wins over its prefix', function () {
  const out = hl('RECORD LOCKS space id 3 index PRIMARY of table `a`.`b` trx id 1 lock_mode X locks gap before rec insert intention waiting');
  assert.ok(out.includes('lock_mode X locks gap before rec insert intention</span>'),
    'expected the insert intention phrase, got: ' + out);
});

check('"waiting" is marked, and only on lock lines', function () {
  assert.ok(hl('RECORD LOCKS space id 3 index PRIMARY of table `a`.`b` lock_mode X waiting').includes('dl-waiting'));
  assert.ok(!hl('MySQL thread id 13, OS thread handle 1, query id 2 root updating').includes('dl-waiting'));
});

check('table and index names are marked', function () {
  const out = hl('RECORD LOCKS space id 3 index PRIMARY of table `dl_x`.`accounts` trx id 1 lock_mode X');
  assert.ok(out.includes('>PRIMARY<'), 'index name should be marked: ' + out);
  assert.ok(out.includes('dl-ident'), 'table name should be marked: ' + out);
});

check('the offending statement is treated as SQL', function () {
  assert.ok(hl('UPDATE accounts SET cents = 1 WHERE id = 20').includes('dl-sql'));
});

check('an unrecognised line still renders', function () {
  const out = hl('something entirely new in a future MySQL');
  assert.ok(out.includes('something entirely new'));
  assert.ok(out.includes('dl-line'));
});

check('HTML in the report cannot escape', function () {
  const out = hl('UPDATE t SET name = "<script>alert(1)</script>"');
  assert.ok(!out.includes('<script>'), 'raw script tag leaked: ' + out);
});

check('an empty report is not an error', function () {
  assert.strictEqual(typeof hl(''), 'string');
  assert.strictEqual(typeof hl(null), 'string');
});

console.log('');
if (failed) {
  console.log(failed + ' check(s) failed');
  process.exit(1);
}
console.log('all ' + passed + ' deadlock report checks passed');
