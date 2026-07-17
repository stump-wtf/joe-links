// Unit tests for popup.js keyword-template matching, run with
// `node --test "integrations/extension/*.test.mjs"` (the quoted glob works on
// all Node versions; a bare directory argument breaks on Node >= 21).
// Loads the real popup source in a vm sandbox with minimal document/chrome
// stubs. Not part of the shipped package.
// Governing: SPEC-0008 REQ "Browser Action — Create Link"
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./popup.js', import.meta.url), 'utf8');

// Load popup.js into a fresh sandbox. `storage` seeds chrome.storage.local;
// `tabs` seeds chrome.tabs.query. Captures document listeners (so tests can
// fire DOMContentLoaded) and elements by id (so tests can assert DOM writes).
function loadPopup({ storage = {}, tabs = [] } = {}) {
  const elements = new Map();
  function el(id) {
    if (!elements.has(id)) {
      elements.set(id, {
        addEventListener: () => {},
        style: {},
        classList: { add: () => {}, remove: () => {} },
        value: '',
        textContent: '',
        disabled: false,
        hidden: false,
        open: false,
        focus: () => {},
        appendChild: () => {},
        replaceChildren: () => {},
        remove: () => {},
      });
    }
    return elements.get(id);
  }
  const listeners = {};
  const context = vm.createContext({
    document: {
      addEventListener: (name, fn) => { listeners[name] = fn; },
      getElementById: (id) => el(id),
      querySelectorAll: () => [],
      createElement: () => el(`created-${elements.size}`),
    },
    chrome: {
      storage: { local: { get: async (defaults) => ({ ...defaults, ...storage }) } },
      tabs: { query: async () => tabs, create: () => {} },
      runtime: { openOptionsPage: () => {} },
      scripting: { executeScript: async () => [] },
    },
    URL,
    AbortSignal,
    fetch: async () => { throw new Error('network disabled in tests'); },
    navigator: { clipboard: { writeText: async () => {} } },
    DOMParser: class { parseFromString() { return { documentElement: el('svg') }; } },
    setTimeout,
    console,
  });
  vm.runInContext(src, context, { filename: 'popup.js' });
  return { context, listeners, el };
}

const match = (tabURL, tpl) => loadPopup().context.matchKeywordTemplate(tabURL, tpl);

// Templates are validated server-side to contain {slug} (internal/handler/keywords.go),
// so matching must split on {slug} — the old code split on '$' and never matched.

test('extracts the slug from a {slug} template', () => {
  assert.equal(
    match('https://jira.example.com/browse/PROJ-123', 'https://jira.example.com/browse/{slug}'),
    'PROJ-123',
  );
});

test('strips query string and fragment from the extracted slug', () => {
  assert.equal(
    match(
      'https://jira.example.com/browse/PROJ-123?focusedId=1#comment',
      'https://jira.example.com/browse/{slug}',
    ),
    'PROJ-123',
  );
});

test('returns null when the tab URL does not match the template prefix', () => {
  assert.equal(
    match('https://github.com/joestump', 'https://jira.example.com/browse/{slug}'),
    null,
  );
});

test('matches a template with a path suffix after {slug} and strips it', () => {
  assert.equal(
    match('https://example.com/x/PROJ/view', 'https://example.com/x/{slug}/view'),
    'PROJ',
  );
  assert.equal(
    match('https://example.com/x/PROJ/other', 'https://example.com/x/{slug}/view'),
    null,
  );
});

test('returns null for a template without a {slug} placeholder', () => {
  assert.equal(match('https://example.com/foo', 'https://example.com/$id'), null);
  assert.equal(match('https://example.com/foo', ''), null);
  assert.equal(match('https://example.com/foo', undefined), null);
});

test('a literal $ in the template no longer mis-splits the match', () => {
  assert.equal(
    match('https://example.com/price$/ABC', 'https://example.com/price$/{slug}'),
    'ABC',
  );
});

test('returns null when the slug remainder is empty', () => {
  assert.equal(
    match('https://jira.example.com/browse/', 'https://jira.example.com/browse/{slug}'),
    null,
  );
});

// --- #210: slug prefix honors the server-configured short keyword -----------
// Governing: SPEC-0008 REQ "Keyword Host Discovery"

test('slug prefix uses the stored short keyword from GET /api/v1/config', async () => {
  const { listeners, el } = loadPopup({
    storage: { baseURL: 'https://links.example.com', shortKeyword: 'go' },
  });
  await listeners.DOMContentLoaded();
  assert.equal(el('slug-prefix').textContent, 'go/');
});

test('slug prefix falls back to the hostname first label on older servers', async () => {
  const { listeners, el } = loadPopup({
    storage: { baseURL: 'https://links.example.com' },
  });
  await listeners.DOMContentLoaded();
  assert.equal(el('slug-prefix').textContent, 'links/');
});

test('slug prefix lowercases an uppercase configured short keyword', async () => {
  // Interception only ever matches lowercase, so the advertised prefix must
  // match what actually works — mirrors resolveServerKeyword in background.js.
  const { listeners, el } = loadPopup({
    storage: { baseURL: 'https://links.example.com', shortKeyword: 'Go' },
  });
  await listeners.DOMContentLoaded();
  assert.equal(el('slug-prefix').textContent, 'go/');
});
