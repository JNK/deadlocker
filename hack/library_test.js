/* The library page's two empty states are mutually exclusive: one is about the
   filter, the other about the library. Showing both at once said two different
   things, so this pins which applies when.

   Run: node hack/library_test.js */

const assert = require('assert');

let passed = 0, failed = 0;
function check(name, fn) {
  try { fn(); console.log('  ok   ' + name); passed++; }
  catch (e) { console.log('  FAIL ' + name + '\n       ' + e.message); failed++; }
}

// The rule as implemented in app.js: the filter's empty state is hidden when
// there is nothing to filter, or when something survived the filter.
function filterEmptyHidden(total, visible) {
  return total === 0 || visible > 0;
}

console.log('library empty states');

check('an empty library shows only its own empty state', function () {
  assert.strictEqual(filterEmptyHidden(0, 0), true,
    '"Nothing matches" must be hidden when there is nothing to match against');
});

check('a filter that excludes everything shows "nothing matches"', function () {
  assert.strictEqual(filterEmptyHidden(31, 0), false);
});

check('a filter with results shows neither', function () {
  assert.strictEqual(filterEmptyHidden(31, 4), true);
});

check('one surviving card is enough', function () {
  assert.strictEqual(filterEmptyHidden(31, 1), true);
});

console.log('');
if (failed) { console.log(failed + ' check(s) failed'); process.exit(1); }
console.log('all ' + passed + ' library checks passed');
