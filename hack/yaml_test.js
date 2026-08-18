/* Exercises the YAML highlighter and completion logic outside a browser.

   Run: node hack/yaml_test.js

   yaml.js only touches the DOM when it attaches an editor, so a handful of
   stubs is enough to load it and call the pure parts. */

const fs = require('fs');
const path = require('path');
const assert = require('assert');

// --- minimal browser stubs -------------------------------------------------
global.window = {
  DL: {
    escapeHTML: function (s) {
      return String(s == null ? '' : s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
  }
};
global.document = {
  readyState: 'loading',
  addEventListener: function () {},
  querySelectorAll: function () { return []; },
  createElement: function () { return { style: {}, classList: { toggle() {} }, appendChild() {} }; }
};

const src = fs.readFileSync(path.join(__dirname, '..', 'internal/web/static/yaml.js'), 'utf8');
// eslint-disable-next-line no-eval
eval(src);

const H = window.DL.highlightYAML;

let failures = 0;
function check(name, fn) {
  try {
    fn();
    console.log('  ok   ' + name);
  } catch (e) {
    failures++;
    console.log('  FAIL ' + name + '\n       ' + e.message);
  }
}

console.log('highlightYAML');

check('keys are marked', () => {
  assert.match(H('name: hello'), /<span class="y-key">name<\/span>/);
});

check('comments are marked', () => {
  assert.match(H('# a comment'), /<span class="y-comment"># a comment<\/span>/);
});

check('trailing comments are split off the value', () => {
  const out = H('lock_wait_timeout: 10  # seconds');
  assert.match(out, /<span class="y-num">10<\/span>/);
  assert.match(out, /<span class="y-comment"># seconds<\/span>/);
});

check('a hash inside a quoted string is not a comment', () => {
  const out = H('name: "a # b"');
  assert.ok(!/y-comment/.test(out), 'should not have produced a comment span');
});

check('booleans and numbers are distinguished', () => {
  assert.match(H('deadlock_detect: false'), /<span class="y-bool">false<\/span>/);
  assert.match(H('lock_wait_timeout: 300'), /<span class="y-num">300<\/span>/);
});

check('block scalar content is highlighted as SQL, not YAML', () => {
  const out = H('  - |\n    CREATE TABLE t (id INT)\n');
  assert.match(out, /<span class="y-kw">CREATE<\/span>/);
  assert.ok(!/y-key/.test(out), 'SQL inside a block scalar must not be parsed as YAML keys');
});

check('a block scalar ends when indentation drops back', () => {
  const out = H('sql: |\n  SELECT 1\nexpect: ok');
  assert.match(out, /<span class="y-key">expect<\/span>/);
});

check('SQL keywords in an inline value are highlighted', () => {
  assert.match(H('    sql: SELECT * FROM t'), /<span class="y-kw">SELECT<\/span>/);
});

check('prose values are not SQL-highlighted', () => {
  const out = H('    note: This will set a value and end the transaction');
  assert.ok(!/y-kw/.test(out), 'prose should not be treated as SQL');
});

check('a keyword inside a SQL string literal is left alone', () => {
  const out = H("  - INSERT INTO t VALUES ('select from where')");
  assert.match(out, /<span class="y-str">&#39;select from where&#39;<\/span>/);
});

check('HTML in the source is escaped', () => {
  const out = H('name: <script>alert(1)</script>');
  assert.ok(!/<script>/.test(out), 'raw script tag leaked into the output');
});

check('line count is preserved', () => {
  const input = 'a: 1\n\nb: 2\n# c\nd: |\n  SELECT 1\n';
  assert.strictEqual(H(input).split('\n').length, input.split('\n').length);
});

check('a real scenario round-trips without throwing', () => {
  const casePath = path.join(__dirname, '..', 'cases/02-gap-locks/uuidv7-missing-row-gap-lock.yaml');
  const yaml = fs.readFileSync(casePath, 'utf8');
  const out = H(yaml);
  assert.strictEqual(out.split('\n').length, yaml.split('\n').length);
  assert.match(out, /y-key/);
  assert.match(out, /y-kw/);
});

console.log('\ncompletions');

const C = window.DL.yamlCompletions;
const texts = (res) => (res ? res.items.map((i) => i.text) : null);

check('top level offers document keys', () => {
  const src = 'na';
  assert.ok(texts(C(src, src.length)).includes('name'));
});

check('keys under mysql: are scoped to that block', () => {
  const src = 'mysql:\n  iso';
  assert.deepStrictEqual(texts(C(src, src.length)), ['isolation']);
});

check('keys under steps: are step keys', () => {
  const src = 'steps:\n  - ac';
  assert.deepStrictEqual(texts(C(src, src.length)), ['actor']);
});

check('expect: offers the five outcomes', () => {
  const src = 'steps:\n  - expect: ';
  assert.deepStrictEqual(texts(C(src, src.length)),
    ['ok', 'blocks', 'error', 'deadlock', 'timeout']);
});

check('expect: filters by what is typed', () => {
  const src = 'steps:\n  - expect: dea';
  assert.deepStrictEqual(texts(C(src, src.length)), ['deadlock']);
});

check('isolation: offers the four levels', () => {
  const src = 'mysql:\n  isolation: ';
  assert.strictEqual(texts(C(src, src.length)).length, 4);
});

check('actor: completes against the actors the document declares', () => {
  const src = 'actors:\n  - id: alpha\n  - id: beta\nsteps:\n  - actor: ';
  assert.deepStrictEqual(texts(C(src, src.length)), ['alpha', 'beta']);
});

check('an unknown key offers no value completions', () => {
  const src = 'name: ';
  assert.strictEqual(C(src, src.length), null);
});


// --- step locations -------------------------------------------------------
// The editor uses these to point at the step the caret is in, so the two halves
// of the page are never describing different things.

const DOC = [
  'name: Example',              // 0
  'actors:',                    // 1
  '  - id: a',                  // 2
  'steps:',                     // 3
  '  - actor: a',               // 4
  '    sql: BEGIN',             // 5
  '',                           // 6
  '  - actor: a',               // 7
  '    sql: |',                 // 8
  '      SELECT 1',             // 9
  '    args:',                  // 10
  '      - 1',                  // 11
  '      - 2',                  // 12
  '  - actor: b',               // 13
  '    sql: COMMIT'             // 14
].join('\n');

check('steps are located by line', function () {
  const at = window.DL.yamlStepAtLine;
  assert.strictEqual(at(DOC, 0), 0, 'name is not in a step');
  assert.strictEqual(at(DOC, 3), 0, 'the steps: key itself is not in a step');
  assert.strictEqual(at(DOC, 4), 1);
  assert.strictEqual(at(DOC, 5), 1);
  assert.strictEqual(at(DOC, 7), 2);
  assert.strictEqual(at(DOC, 9), 2, 'a block scalar body belongs to its step');
  assert.strictEqual(at(DOC, 13), 3);
  assert.strictEqual(at(DOC, 14), 3);
});

check('a nested list is not mistaken for a step', function () {
  // Lines 11 and 12 are args entries, indented deeper than the step dashes.
  assert.strictEqual(window.DL.yamlStepAtLine(DOC, 11), 2);
  assert.strictEqual(window.DL.yamlStepAtLine(DOC, 12), 2);
  assert.strictEqual(window.DL.yamlStepRanges(DOC).length, 3);
});

check('a top-level key after the steps closes the block', function () {
  const doc = 'steps:\n  - actor: a\n    sql: BEGIN\ntags: [x]\nname: after';
  assert.strictEqual(window.DL.yamlStepAtLine(doc, 1), 1);
  assert.strictEqual(window.DL.yamlStepAtLine(doc, 3), 0, 'tags is not a step');
  assert.strictEqual(window.DL.yamlStepAtLine(doc, 4), 0, 'name is not a step');
});

check('an indented steps: does not open a block', function () {
  const doc = 'mysql:\n  steps: 3\nname: x';
  assert.strictEqual(window.DL.yamlStepRanges(doc).length, 0);
});

check('an offset maps to the same step as its line', function () {
  const at = window.DL.yamlStepAtOffset;
  assert.strictEqual(at(DOC, 0), 0);
  assert.strictEqual(at(DOC, DOC.indexOf('sql: BEGIN')), 1);
  assert.strictEqual(at(DOC, DOC.indexOf('sql: COMMIT')), 3);
});

check('a document with no steps has none', function () {
  assert.strictEqual(window.DL.yamlStepRanges('name: x').length, 0);
  assert.strictEqual(window.DL.yamlStepAtLine('', 0), 0);
  assert.strictEqual(window.DL.yamlStepAtOffset(null, 0), 0);
});


console.log(failures ? '\n' + failures + ' failure(s)' : '\nall checks passed');
process.exit(failures ? 1 : 0);
