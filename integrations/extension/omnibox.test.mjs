// Unit tests for the omnibox integration in background.js, run with
// `node --test "integrations/extension/*.test.mjs"`. Loads the real
// service-worker source in a vm sandbox with a chrome.* stub — no browser
// needed. Not part of the shipped extension package.
// Governing: SPEC-0019 REQ "Extension Omnibox Integration"
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./background.js', import.meta.url), 'utf8');

// Debounce interval declared in background.js (const, so not reachable on the
// vm context) — waits must exceed it. The spec floor is 150 ms.
const DEBOUNCE_MS = 200;
const settleDebounce = () => new Promise((r) => setTimeout(r, DEBOUNCE_MS + 100));

// Load background.js into a fresh sandbox. `storage` seeds chrome.storage.local;
// `fetchImpl` stubs global fetch; `omnibox: false` simulates a browser without
// the omnibox API (Safari).
function loadBackground({ storage = {}, fetchImpl, omnibox = true } = {}) {
  const captured = {
    listeners: {},
    tabUpdates: [],
    tabCreates: [],
    suggestCalls: [],
    defaultSuggestions: [],
    fetches: [],
    storage,
  };
  const chrome = {
    storage: {
      local: {
        get: async (defaults) => ({ ...defaults, ...storage }),
        set: async (obj) => { Object.assign(storage, obj); },
      },
    },
    declarativeNetRequest: {
      getDynamicRules: async () => [],
      updateDynamicRules: async () => {},
    },
    runtime: {
      onInstalled: { addListener: (fn) => { captured.listeners.onInstalled = fn; } },
      onStartup: { addListener: (fn) => { captured.listeners.onStartup = fn; } },
      onMessage: { addListener: (fn) => { captured.listeners.onMessage = fn; } },
      getURL: (p) => `chrome-extension://test/${p}`,
    },
    alarms: {
      get: async () => undefined,
      create: () => {},
      onAlarm: { addListener: (fn) => { captured.listeners.onAlarm = fn; } },
    },
    webNavigation: {
      onBeforeNavigate: { addListener: (fn) => { captured.listeners.onBeforeNavigate = fn; } },
    },
    tabs: {
      // The omnibox path calls update({url}) (active tab); search interception
      // calls update(tabId, {url}) — capture both signatures.
      update: (...args) => {
        // Spread into fresh test-realm objects: vm-realm objects have a
        // different Object.prototype and would fail deepEqual.
        captured.tabUpdates.push(typeof args[0] === 'object' ? { ...args[0] } : { tabId: args[0], ...args[1] });
        return Promise.resolve({});
      },
      create: (props) => {
        captured.tabCreates.push({ ...props });
        return Promise.resolve({});
      },
    },
    action: { setIcon: async () => {} },
  };
  if (omnibox) {
    chrome.omnibox = {
      onInputChanged: { addListener: (fn) => { captured.listeners.onInputChanged = fn; } },
      onInputEntered: { addListener: (fn) => { captured.listeners.onInputEntered = fn; } },
      setDefaultSuggestion: (s) => { captured.defaultSuggestions.push(s); },
    };
  }
  const wrappedFetch = async (url, opts) => {
    captured.fetches.push({ url: String(url), opts });
    if (!fetchImpl) throw new Error('network disabled in tests');
    return fetchImpl(url, opts);
  };
  const context = vm.createContext({
    chrome,
    URL,
    AbortSignal,
    fetch: wrappedFetch,
    console,
    setTimeout,
    clearTimeout,
  });
  vm.runInContext(src, context, { filename: 'background.js' });
  return { context, captured };
}

const STORAGE = {
  baseURL: 'https://go.stump.rocks',
  keywords: ['go.stump.rocks', 'go'],
  apiKey: 'jl_pat_secret',
};

// fetch stub for GET /api/v1/links/suggest returning the given suggestions.
const suggestFetch = (suggestions) => async (url) => {
  const u = String(url);
  if (u.includes('/api/v1/links/suggest')) {
    return { ok: true, json: async () => ({ suggestions }) };
  }
  throw new Error(`unexpected fetch ${u}`);
};

// Type into the omnibox and collect what suggest() receives.
function typeOmnibox(captured, text) {
  captured.listeners.onInputChanged(text, (results) => {
    captured.suggestCalls.push(results);
  });
}

// --- SPEC-0019 REQ "Extension Omnibox Integration" scenarios -----------------

test('Scenario: Omnibox Suggestions Appear', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: suggestFetch([
      { slug: 'jira', title: 'Jira board' },
      { slug: 'jitsi', title: '' },
    ]),
  });
  typeOmnibox(captured, 'ji');
  await settleDebounce();

  assert.equal(captured.fetches.length, 1);
  assert.equal(
    captured.fetches[0].url,
    'https://go.stump.rocks/api/v1/links/suggest?q=ji',
  );
  // The stored PAT rides along as a Bearer token (SPEC-0008 REQ "API Key Authentication").
  assert.equal(captured.fetches[0].opts.headers['Authorization'], 'Bearer jl_pat_secret');

  assert.equal(captured.suggestCalls.length, 1);
  const results = captured.suggestCalls[0];
  assert.equal(results.length, 2);
  // Slug + title shown, prefixed with the server's short keyword.
  assert.deepEqual(
    { content: results[0].content, description: results[0].description },
    { content: 'jira', description: 'go/jira — Jira board' },
  );
  // Title absent → slug only.
  assert.deepEqual(
    { content: results[1].content, description: results[1].description },
    { content: 'jitsi', description: 'go/jitsi' },
  );
});

test('Scenario: Suggestion Selection Navigates via Resolver', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  // Selecting the suggestion for slug `jira` delivers its content to
  // onInputEntered — the tab must go to the resolver URL, never the raw
  // destination, so the server performs the redirect.
  await captured.listeners.onInputEntered('jira', 'currentTab');
  assert.deepEqual(captured.tabUpdates, [{ url: 'https://go.stump.rocks/jira' }]);
  assert.deepEqual(captured.tabCreates, []);
});

test('Scenario: Debounce Coalesces Keystrokes', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: suggestFetch([]),
  });
  // Five characters in quick succession…
  for (const text of ['j', 'ji', 'jir', 'jira', 'jira-']) {
    typeOmnibox(captured, text);
  }
  // …issue no request until input settles…
  assert.equal(captured.fetches.length, 0);
  await settleDebounce();
  // …then at most one, for the final text.
  assert.equal(captured.fetches.length, 1);
  assert.match(captured.fetches[0].url, /\?q=jira-$/);
});

test('Scenario: No PAT Configured', async () => {
  const { captured } = loadBackground({
    storage: { baseURL: 'https://go.stump.rocks' }, // no apiKey saved
  });
  const registrationDefaults = captured.defaultSuggestions.length;
  assert.ok(registrationDefaults >= 1); // a default suggestion exists from startup

  typeOmnibox(captured, 'jira');
  await settleDebounce();

  // No request hits the suggest endpoint and only a default suggestion is shown.
  assert.equal(captured.fetches.length, 0);
  assert.equal(captured.suggestCalls.length, 0);
  assert.equal(captured.defaultSuggestions.length, registrationDefaults + 1);
  assert.match(captured.defaultSuggestions.at(-1).description, /API key/i);

  // Pressing Enter still navigates the typed text to {baseURL}/{text}.
  await captured.listeners.onInputEntered('jira', 'currentTab');
  assert.deepEqual(captured.tabUpdates, [{ url: 'https://go.stump.rocks/jira' }]);
});

test('Scenario: Browser Without Omnibox API', async () => {
  // Safari's Xcode project references this same background script and has no
  // omnibox API — startup must not throw and search interception keeps working.
  let loaded;
  assert.doesNotThrow(() => { loaded = loadBackground({ storage: { ...STORAGE }, omnibox: false }); });
  const { captured } = loaded;
  assert.equal(captured.listeners.onInputChanged, undefined);
  await captured.listeners.onBeforeNavigate({
    frameId: 0,
    tabId: 7,
    url: `https://www.google.com/search?q=${encodeURIComponent('go/slack')}`,
  });
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://go.stump.rocks/slack' }]);
});

// --- Supporting behavior ------------------------------------------------------

test('at most 5 suggestions are surfaced', () => {
  const { context } = loadBackground();
  const entries = Array.from({ length: 8 }, (_, i) => ({ slug: `s${i}`, title: '' }));
  assert.equal(context.mapOmniboxSuggestions(entries, 'go').length, 5);
  // Non-array and malformed entries are tolerated.
  assert.deepEqual(Array.from(context.mapOmniboxSuggestions(null, 'go')), []);
  assert.deepEqual(Array.from(context.mapOmniboxSuggestions([{ title: 'no slug' }, null], 'go')), []);
});

test('suggestion descriptions are XML-escaped', () => {
  const { context } = loadBackground();
  const [s] = context.mapOmniboxSuggestions(
    [{ slug: 'ab', title: `Tom & Jerry <"quoted"> 'show'` }],
    'go',
  );
  assert.equal(
    s.description,
    'go/ab — Tom &amp; Jerry &lt;&quot;quoted&quot;&gt; &apos;show&apos;',
  );
});

test('typed text is percent-encoded in the suggest query string', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: suggestFetch([]),
  });
  typeOmnibox(captured, 'ji ra&x');
  await settleDebounce();
  assert.equal(captured.fetches.length, 1);
  assert.match(captured.fetches[0].url, /\?q=ji%20ra%26x$/);
});

test('suggest request failures fail silently and Enter still navigates', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: async () => { throw new TypeError('Failed to fetch'); },
  });
  typeOmnibox(captured, 'jira');
  await settleDebounce();
  assert.deepEqual(Array.from(captured.suggestCalls[0]), []); // no suggestions, no throw
  await captured.listeners.onInputEntered('jira', 'currentTab');
  assert.deepEqual(captured.tabUpdates, [{ url: 'https://go.stump.rocks/jira' }]);
});

test('committing the omnibox cancels a pending debounced suggest request', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: suggestFetch([]),
  });
  // Enter lands within the debounce window: the pending timer must be
  // cancelled so no PAT-authenticated request fires after navigation.
  typeOmnibox(captured, 'jira');
  await captured.listeners.onInputEntered('jira', 'currentTab');
  await settleDebounce();
  assert.equal(captured.fetches.length, 0);
  assert.deepEqual(captured.tabUpdates, [{ url: 'https://go.stump.rocks/jira' }]);
});

test('a 401 from the suggest endpoint yields no suggestions', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: async () => ({ ok: false, status: 401 }),
  });
  typeOmnibox(captured, 'jira');
  await settleDebounce();
  assert.equal(captured.suggestCalls.length, 1);
  assert.deepEqual(Array.from(captured.suggestCalls[0]), []);
});

test('typed text is never interpreted as an absolute URL', async () => {
  const { context } = loadBackground();
  for (const text of ['https://evil.example/x', '//evil.example/x', 'jira?x=1#frag']) {
    const out = context.omniboxResolverURL('https://go.stump.rocks', text);
    // Joined as path content — always same-origin with the base URL.
    assert.equal(new URL(out).origin, 'https://go.stump.rocks');
  }
  // "#" and "?" stay in the path; "/" survives for keyword paths.
  assert.equal(
    context.omniboxResolverURL('https://go.stump.rocks', 'jira/ABC-123?x#y'),
    'https://go.stump.rocks/jira/ABC-123%3Fx%23y',
  );
});

test('a lone surrogate in the typed text never breaks Enter-to-navigate', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  // '\uD800' alone would make encodeURI throw URIError; the resolver URL
  // builder must substitute it and still produce a same-origin URL.
  const out = context.omniboxResolverURL('https://go.stump.rocks', 'ji\uD800ra');
  assert.equal(new URL(out).origin, 'https://go.stump.rocks');
  // The full Enter path navigates rather than throwing.
  await captured.listeners.onInputEntered('ji\uD800ra', 'currentTab');
  assert.equal(captured.tabUpdates.length, 1);
  assert.equal(new URL(captured.tabUpdates[0].url).origin, 'https://go.stump.rocks');
});

test('the suggestion prefix honors the server-configured short keyword', async () => {
  const { captured } = loadBackground({
    storage: {
      baseURL: 'https://links.example.com',
      apiKey: 'jl_pat_secret',
      shortKeyword: 'go',
    },
    fetchImpl: suggestFetch([{ slug: 'jira', title: 'Jira' }]),
  });
  typeOmnibox(captured, 'ji');
  await settleDebounce();
  assert.equal(captured.suggestCalls[0][0].description, 'go/jira — Jira');
});

test('selecting with a new-tab disposition opens a tab at the resolver URL', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await captured.listeners.onInputEntered('jira', 'newForegroundTab');
  await captured.listeners.onInputEntered('wiki', 'newBackgroundTab');
  assert.deepEqual(captured.tabCreates, [
    { url: 'https://go.stump.rocks/jira' },
    { url: 'https://go.stump.rocks/wiki', active: false },
  ]);
  assert.deepEqual(captured.tabUpdates, []);
});
