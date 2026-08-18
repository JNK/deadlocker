/* Exercises the command palette's ranking outside a browser.

   Run: node hack/palette_test.js

   palette.js only touches the DOM when it opens, so a handful of stubs is
   enough to load it and call the pure search function. What is worth testing is
   the ordering: a palette that finds the right scenario in fourth place is a
   palette you stop using. */

const fs = require('fs');
const path = require('path');
const assert = require('assert');

global.window = {
  DL: {
    escapeHTML: function (s) { return String(s == null ? '' : s); }
  }
};
global.document = {
  addEventListener: function () {},
  querySelectorAll: function () { return []; },
  createElement: function () {
    return { style: {}, classList: { toggle() {} }, appendChild() {}, querySelector() { return null; } };
  },
  body: { appendChild: function () {} }
};
global.navigator = { platform: 'MacIntel', userAgent: 'test' };
global.fetch = function () { return Promise.resolve({ ok: false }); };

const src = fs.readFileSync(path.join(__dirname, '..', 'internal/web/static/palette.js'), 'utf8');
// eslint-disable-next-line no-eval
eval(src);

const search = window.DL.paletteSearch;
assert.ok(typeof search === 'function', 'paletteSearch is exported');

const LIB = [
  { kind: 'go', title: 'Settings', subtitle: 'model endpoint', url: '/settings', terms: ['config', 'llm'] },
  { kind: 'go', title: 'Compare runs', subtitle: 'diff two runs', url: '/compare', terms: ['diff'] },
  { kind: 'scenario', title: 'Gap locks are precise', subtitle: 'Gap locks', url: '/case/gap',
    detail: 'A gap lock covers the space between two keys.', terms: ['gap-locks', 'gap-lock'] },
  { kind: 'scenario', title: 'The classic AB-BA deadlock', subtitle: 'Deadlocks', url: '/case/abba',
    detail: 'Two transactions take the same two locks in opposite order.', terms: ['deadlock', 'ab-ba'] },
  { kind: 'scenario', title: 'SELECT FOR UPDATE on a missing UUIDv7 row blocks the next insert',
    subtitle: 'Gap locks', url: '/case/uuid',
    detail: 'The gap lock a missing row takes collides with the next insert.',
    terms: ['uuidv7', 'insert-intention'] },
  { kind: 'run', title: 'The classic AB-BA deadlock', subtitle: 'run · deadlock',
    detail: 'abc123', url: '/run/abc123', terms: ['abc123'] }
];

let passed = 0, failed = 0;
function check(name, fn) {
  try { fn(); console.log('  ok   ' + name); passed++; }
  catch (e) { console.log('  FAIL ' + name + '\n       ' + e.message); failed++; }
}

console.log('command palette');

check('an empty query lists the actions first', function () {
  const r = search(LIB, '');
  assert.strictEqual(r[0].title, 'Settings');
  assert.strictEqual(r[1].title, 'Compare runs');
});

check('a title prefix wins over a mention in the body', function () {
  const r = search(LIB, 'gap');
  assert.strictEqual(r[0].title, 'Gap locks are precise',
    'expected the scenario named after gaps, got ' + r[0].title);
});

check('a word inside the title still matches', function () {
  const r = search(LIB, 'deadlock');
  assert.ok(r.some(function (i) { return i.title === 'The classic AB-BA deadlock'; }));
});

check('a tag matches even when the title does not', function () {
  const r = search(LIB, 'uuidv7');
  assert.strictEqual(r[0].url, '/case/uuid');
});

check('description text is searchable', function () {
  const r = search(LIB, 'opposite order');
  assert.strictEqual(r[0].url, '/case/abba');
});

check('a run id finds the run', function () {
  const r = search(LIB, 'abc123');
  assert.strictEqual(r[0].kind, 'run');
});

check('a subsequence matches but ranks below a real hit', function () {
  const r = search(LIB, 'dlk');
  assert.ok(r.length > 0, 'dlk should find something');
  const exact = search(LIB, 'deadlock');
  assert.strictEqual(exact[0].title, 'The classic AB-BA deadlock');
});

check('nothing matching returns nothing', function () {
  assert.strictEqual(search(LIB, 'zzzzzqqqq').length, 0);
});

check('the shortest title wins when both start with the query', function () {
  const r = search([
    { kind: 'scenario', title: 'Gap locks are precise and long winded', url: '/a' },
    { kind: 'scenario', title: 'Gap locks', url: '/b' }
  ], 'gap');
  assert.strictEqual(r[0].url, '/b');
});

check('case is ignored', function () {
  assert.strictEqual(search(LIB, 'GAP')[0].title, 'Gap locks are precise');
});

console.log('');
if (failed) {
  console.log(failed + ' check(s) failed');
  process.exit(1);
}
console.log('all ' + passed + ' palette checks passed');
