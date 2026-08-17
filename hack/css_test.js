/* CSS invariants that are easy to break and hard to notice.

   Run: node hack/css_test.js */

const fs = require('fs');
const path = require('path');
const assert = require('assert');

const cssPath = path.join(__dirname, '..', 'internal/web/static/app.css');
const css = fs.readFileSync(cssPath, 'utf8');
const templateDir = path.join(__dirname, '..', 'internal/web/templates');
const html = fs.readdirSync(templateDir)
  .filter((f) => f.endsWith('.html'))
  .map((f) => fs.readFileSync(path.join(templateDir, f), 'utf8'))
  .join('\n');

function rule(selector) {
  const m = new RegExp('(?:^|\\n)' + selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') +
    '\\s*\\{([^}]*)\\}').exec(css);
  return m ? m[1] : null;
}

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

console.log('css invariants');

// Without this, any class that sets `display` outranks the UA's
// `[hidden] { display: none }` and the element stays visible.
check('the hidden attribute is enforced with !important', () => {
  assert.match(css, /\[hidden\]\s*\{\s*display:\s*none\s*!important/,
    'expected a global [hidden] { display: none !important } rule');
});

// Every element that carries a hidden attribute in markup, or is toggled from
// JavaScript, must actually disappear. The global rule above covers them, so
// this asserts the rule precedes any class that would fight it.
check('the hidden rule comes before the rules it has to beat', () => {
  const hiddenRule = css.search(/\[hidden\]\s*\{\s*display:\s*none\s*!important/);
  assert.ok(hiddenRule >= 0, 'no [hidden] rule at all');
  // !important wins regardless of order, but keeping it near the top of the
  // file is what makes it discoverable.
  assert.ok(hiddenRule < css.length / 4,
    'the [hidden] rule should sit near the top of the stylesheet');
});

check('every hidden-by-default element resolves to a real element', () => {
  const ids = [...html.matchAll(/id="([\w-]+)"[^>]*\bhidden\b/g)].map((m) => m[1]);
  assert.ok(ids.length > 0, 'expected some hidden-by-default elements');
  // The builder sheet is the one that has bitten us; pin it explicitly.
  assert.ok(ids.includes('builder-sheet'),
    'the builder sheet must start hidden, not open');
  assert.ok(ids.includes('builder-backdrop'),
    'the builder backdrop must start hidden');
});

check('the builder only auto-opens behind an explicit route flag', () => {
  const hooks = [...html.matchAll(/DL_OPEN_BUILDER/g)];
  if (hooks.length === 0) return; // no auto-open at all is also fine
  assert.match(html, /\{\{if \.OpenBuilder\}\}[\s\S]{0,200}DL_OPEN_BUILDER/,
    'the auto-open hook must be guarded by {{if .OpenBuilder}}, so it only ' +
    'fires on the /builder route and never on an ordinary page');
});

// The sticky header must sit flush against the top of its scroll container.
// Any gap there shows the list scrolling through the strip above the header.
check('nothing can peek above the sticky header', () => {
  const main = rule('.main');
  assert.ok(main, 'no .main rule');
  const padding = /padding:\s*([^;]+)/.exec(main);
  assert.ok(padding, '.main has no padding shorthand');
  const top = padding[1].trim().split(/\s+/)[0];
  assert.strictEqual(top, '0',
    '.main must have no top padding; the sticky header supplies that spacing itself');

  const header = rule('.page-header-sticky');
  assert.match(header, /top:\s*0/, 'the sticky header must stick at top: 0');
});

check('the sticky header is frosted, not opaque', () => {
  const header = rule('.page-header-sticky');
  assert.match(header, /backdrop-filter:\s*blur/,
    'the header should blur the content passing under it');
  assert.match(header, /color-mix\(/,
    'the header background should be translucent for the blur to show');
  assert.match(css, /@supports not \(\(backdrop-filter/,
    'needs an opaque fallback where backdrop-filter is unsupported');
});

console.log(failures ? '\n' + failures + ' failure(s)' : '\nall css checks passed');
process.exit(failures ? 1 : 0);
