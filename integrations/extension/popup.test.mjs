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

function loadPopup() {
  const noopElement = { addEventListener: () => {}, style: {}, value: '' };
  const context = vm.createContext({
    document: {
      addEventListener: () => {},
      getElementById: () => noopElement,
      querySelectorAll: () => [],
      createElement: () => noopElement,
    },
    chrome: {},
    URL,
    console,
  });
  vm.runInContext(src, context, { filename: 'popup.js' });
  return context;
}

const match = (tabURL, tpl) => loadPopup().matchKeywordTemplate(tabURL, tpl);

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
