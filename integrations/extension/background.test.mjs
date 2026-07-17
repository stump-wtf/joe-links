// Unit tests for background.js, run with `node --test "integrations/extension/*.test.mjs"` (the quoted glob
// works on all Node versions; a bare directory argument breaks on Node >= 21).
// Loads the real service-worker source in a vm sandbox with a chrome.* stub —
// no browser needed. Not part of the shipped extension package.
// Governing: SPEC-0008 REQ "Search Interception and Redirect", REQ "Fallthrough Safety",
//            REQ "Keyword Host Discovery"
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const src = readFileSync(new URL('./background.js', import.meta.url), 'utf8');

// Load background.js into a fresh sandbox. `storage` seeds chrome.storage.local;
// `alarms` seeds chrome.alarms; `fetchImpl` stubs global fetch.
function loadBackground({ storage = {}, alarms = {}, fetchImpl } = {}) {
  const captured = {
    dnrRules: [],
    tabUpdates: [],
    alarmCreates: [],
    listeners: {},
    storage,
    alarms,
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
      updateDynamicRules: async ({ addRules }) => { captured.dnrRules = addRules; },
    },
    runtime: {
      onInstalled: { addListener: (fn) => { captured.listeners.onInstalled = fn; } },
      onStartup: { addListener: (fn) => { captured.listeners.onStartup = fn; } },
      onMessage: { addListener: (fn) => { captured.listeners.onMessage = fn; } },
      getURL: (p) => `chrome-extension://test/${p}`,
    },
    alarms: {
      get: async (name) => alarms[name],
      create: (name, info) => {
        captured.alarmCreates.push({ name, ...info });
        alarms[name] = { name, ...info };
      },
      onAlarm: { addListener: (fn) => { captured.listeners.onAlarm = fn; } },
    },
    webNavigation: {
      onBeforeNavigate: { addListener: (fn) => { captured.listeners.onBeforeNavigate = fn; } },
    },
    tabs: {
      update: (tabId, props) => {
        captured.tabUpdates.push({ tabId, ...props });
        return Promise.resolve({});
      },
      create: () => {},
    },
    action: { setIcon: async () => {} },
  };
  const context = vm.createContext({
    chrome,
    URL,
    AbortSignal,
    fetch: fetchImpl || (async () => { throw new Error('network disabled in tests'); }),
    console,
    setTimeout,
  });
  vm.runInContext(src, context, { filename: 'background.js' });
  return { context, captured };
}

// Let the top-level ensureKeywordRefreshAlarm() promise chain settle.
const settle = () => new Promise((r) => setTimeout(r, 0));

// Apply the captured declarativeNetRequest rules to a URL the way the browser
// would: first matching regexFilter wins, \N backrefs substituted.
function applyDNR(rules, url) {
  for (const rule of rules) {
    const m = url.match(new RegExp(rule.condition.regexFilter));
    if (m) {
      return rule.action.redirect.regexSubstitution.replace(/\\(\d)/g, (_, n) => m[+n] ?? '');
    }
  }
  return null;
}

const STORAGE = { baseURL: 'https://go.stump.rocks', keywords: ['go.stump.rocks', 'go', 'jira'] };

// --- #211: search-engine host coverage -------------------------------------

test('getSearchQuery matches Google ccTLDs, bare bing.com, start.duckduckgo.com', () => {
  const { context } = loadBackground();
  const q = (u) => context.getSearchQuery(new URL(u));
  assert.equal(q('https://www.google.com/search?q=go%2Fslack'), 'go/slack');
  assert.equal(q('https://www.google.co.uk/search?q=x'), 'x');
  assert.equal(q('https://google.de/search?q=x'), 'x');
  assert.equal(q('https://www.google.com.au/search?q=x'), 'x');
  assert.equal(q('https://bing.com/search?q=x'), 'x');
  assert.equal(q('https://www.bing.com/search?q=x'), 'x');
  assert.equal(q('https://start.duckduckgo.com/?q=x'), 'x');
  assert.equal(q('https://duckduckgo.com/?q=x'), 'x');
  assert.equal(q('https://kagi.com/search?q=x'), 'x');
  // Not search engines / not search paths:
  assert.equal(q('https://example.com/search?q=x'), null);
  assert.equal(q('https://www.google.com/maps?q=x'), null);
});

// --- #208: DNR rules must preserve the keyword path segment -----------------

test('DNR rule for a template keyword prepends the keyword path segment', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  await context.updateRedirectRules();
  assert.equal(
    applyDNR(captured.dnrRules, 'http://jira/ABC-123'),
    'https://go.stump.rocks/jira/ABC-123',
  );
});

test('DNR rule for the server alias swaps host without double prefix', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  await context.updateRedirectRules();
  assert.equal(applyDNR(captured.dnrRules, 'http://go/slack'), 'https://go.stump.rocks/slack');
});

test('DNR rules do not match the canonical server hostname', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  await context.updateRedirectRules();
  assert.equal(applyDNR(captured.dnrRules, 'https://go.stump.rocks/whatever'), null);
});

test('DNR redirect for a bare keyword host yields a sane URL', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  await context.updateRedirectRules();
  const out = applyDNR(captured.dnrRules, 'http://jira');
  assert.equal(out, 'https://go.stump.rocks/jira');
  assert.doesNotThrow(() => new URL(out));
});

test('DNR redirect preserves the query string', async () => {
  const { context, captured } = loadBackground({ storage: { ...STORAGE } });
  await context.updateRedirectRules();
  assert.equal(
    applyDNR(captured.dnrRules, 'http://jira/ABC-123?focus=1'),
    'https://go.stump.rocks/jira/ABC-123?focus=1',
  );
});

// --- #209: keyword-refresh alarm resurrection -------------------------------

test('worker start creates the keyword-refresh alarm when absent', async () => {
  const { captured } = loadBackground();
  await settle();
  assert.deepEqual(captured.alarmCreates, [{ name: 'keyword-refresh', periodInMinutes: 60 }]);
});

test('worker start does not reset an existing keyword-refresh alarm', async () => {
  const { captured } = loadBackground({
    alarms: { 'keyword-refresh': { name: 'keyword-refresh', periodInMinutes: 60 } },
  });
  await settle();
  assert.deepEqual(captured.alarmCreates, []);
});

test('repeated ensureKeywordRefreshAlarm calls create the alarm exactly once', async () => {
  const { context, captured } = loadBackground();
  await settle();
  await context.ensureKeywordRefreshAlarm();
  await context.ensureKeywordRefreshAlarm();
  assert.equal(captured.alarmCreates.length, 1);
});

test('onStartup re-creates the alarm (Firefox restart drops it)', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await settle();
  delete captured.alarms['keyword-refresh']; // simulate Firefox dropping alarms on restart
  captured.alarmCreates.length = 0;
  await captured.listeners.onStartup();
  assert.deepEqual(captured.alarmCreates, [{ name: 'keyword-refresh', periodInMinutes: 60 }]);
});

// --- #211: refresh-keywords must report failures ----------------------------

function sendRefreshMessage(captured) {
  return new Promise((resolve) => {
    const keepOpen = captured.listeners.onMessage({ type: 'refresh-keywords' }, null, resolve);
    assert.equal(keepOpen, true);
  });
}

test('refresh-keywords message responds ok:false when the server is unreachable', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: async () => { throw new TypeError('Failed to fetch'); },
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, false);
  assert.match(result.error, /reach server/i);
});

test('refresh-keywords message responds ok:false on a non-2xx response', async () => {
  const { captured } = loadBackground({
    storage: { ...STORAGE },
    fetchImpl: async () => ({ ok: false, status: 500 }),
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, false);
  assert.match(result.error, /500/);
});

test('refresh-keywords message responds ok:true and stores merged keywords', async () => {
  const storage = { ...STORAGE };
  const { captured } = loadBackground({
    storage,
    fetchImpl: async () => ({ ok: true, json: async () => ['jira', 'wiki'] }),
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  // Array.from: the merged array was built inside the vm realm (different Array prototype).
  assert.deepEqual(Array.from(storage.keywords), ['go.stump.rocks', 'go', 'jira', 'wiki']);
});

// --- #211: search interception fallthrough + encoding + query preservation --

async function navigate(captured, url) {
  await captured.listeners.onBeforeNavigate({ frameId: 0, tabId: 7, url });
}

const googleSearch = (q) => `https://www.google.com/search?q=${encodeURIComponent(q)}`;

test('search queries containing spaces fall through to the search engine', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, googleSearch('go/no-go decision matrix'));
  assert.deepEqual(captured.tabUpdates, []);
});

test('exact keyword/slug search redirects to the server', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, googleSearch('go/slack'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://go.stump.rocks/slack' }]);
});

test('template keyword search routes via /{keyword}/{slug}', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, googleSearch('jira/ABC-123'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://go.stump.rocks/jira/ABC-123' }]);
});

test('slug from search text is percent-encoded so go/100% builds a valid URL', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, googleSearch('go/100%'));
  assert.equal(captured.tabUpdates.length, 1);
  assert.equal(captured.tabUpdates[0].url, 'https://go.stump.rocks/100%25');
  assert.doesNotThrow(() => new URL(captured.tabUpdates[0].url));
});

test('unregistered keyword search falls through', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, googleSearch('zzz/slack'));
  assert.deepEqual(captured.tabUpdates, []);
});

test('direct keyword-host navigation preserves the query string', async () => {
  const { captured } = loadBackground({ storage: { ...STORAGE } });
  await navigate(captured, 'http://jira/ABC-123?focus=1');
  assert.deepEqual(captured.tabUpdates, [
    { tabId: 7, url: 'https://go.stump.rocks/jira/ABC-123?focus=1' },
  ]);
});

// --- #210: server-configured short keyword (JOE_SHORT_KEYWORD) --------------

// Deployment shape from the issue: UI advertises "go/" but the server lives at
// links.example.com, so hostname-derived "links" is the WRONG alias.
const OVERRIDE_STORAGE = {
  baseURL: 'https://links.example.com',
  keywords: ['links.example.com', 'go', 'jira'],
  shortKeyword: 'go',
};

// fetch stub that dispatches on URL: /api/v1/keywords → bare array,
// /api/v1/config → the given response.
const dispatchFetch = (keywords, configRes) => async (url) => {
  const u = String(url);
  if (u.endsWith('/api/v1/keywords')) return { ok: true, json: async () => keywords };
  if (u.endsWith('/api/v1/config')) return configRes;
  throw new Error(`unexpected fetch ${u}`);
};

test('refresh stores the server-configured short keyword and merges it into keywords', async () => {
  const storage = { baseURL: 'https://links.example.com' };
  const { captured } = loadBackground({
    storage,
    fetchImpl: dispatchFetch(['jira'], { ok: true, json: async () => ({ short_keyword: 'go' }) }),
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  assert.equal(storage.shortKeyword, 'go');
  assert.deepEqual(Array.from(storage.keywords), ['links.example.com', 'go', 'jira']);
});

test('refresh against an older server without /config falls back to the hostname label', async () => {
  const storage = { baseURL: 'https://links.example.com' };
  const { captured } = loadBackground({
    storage,
    fetchImpl: dispatchFetch(['jira'], { ok: false, status: 404 }),
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  // '' keeps the hostname fallback dynamic if the base URL later changes.
  assert.equal(storage.shortKeyword, '');
  assert.deepEqual(Array.from(storage.keywords), ['links.example.com', 'links', 'jira']);
});

test('a /config network error does not fail the keyword refresh', async () => {
  const storage = { baseURL: 'https://links.example.com' };
  const { captured } = loadBackground({
    storage,
    fetchImpl: async (url) => {
      if (String(url).endsWith('/api/v1/keywords')) return { ok: true, json: async () => [] };
      throw new TypeError('Failed to fetch');
    },
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  assert.equal(storage.shortKeyword, '');
});

test('a transient /config failure keeps the previously stored short keyword', async () => {
  // A 5xx (or timeout/LB blip) is NOT "endpoint absent" — wiping the stored
  // value would revert interception to the wrong hostname alias for an hour.
  for (const configRes of [{ ok: false, status: 500 }, { ok: false, status: 503 }]) {
    const storage = { baseURL: 'https://links.example.com', shortKeyword: 'go' };
    const { captured } = loadBackground({
      storage,
      fetchImpl: dispatchFetch(['jira'], configRes),
    });
    const result = await sendRefreshMessage(captured);
    assert.equal(result.ok, true);
    assert.equal(storage.shortKeyword, 'go');
    assert.deepEqual(Array.from(storage.keywords), ['links.example.com', 'go', 'jira']);
  }
});

test('a /config network error keeps the previously stored short keyword', async () => {
  const storage = { baseURL: 'https://links.example.com', shortKeyword: 'go' };
  const { captured } = loadBackground({
    storage,
    fetchImpl: async (url) => {
      if (String(url).endsWith('/api/v1/keywords')) return { ok: true, json: async () => ['jira'] };
      throw new TypeError('Failed to fetch');
    },
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  assert.equal(storage.shortKeyword, 'go');
  assert.deepEqual(Array.from(storage.keywords), ['links.example.com', 'go', 'jira']);
});

test('a definitive 404 from /config clears a stale stored short keyword', async () => {
  // Only a 404 means the endpoint (and thus the override) is really gone —
  // e.g. baseURL repointed at an older server.
  const storage = { baseURL: 'https://links.example.com', shortKeyword: 'go' };
  const { captured } = loadBackground({
    storage,
    fetchImpl: dispatchFetch(['jira'], { ok: false, status: 404 }),
  });
  const result = await sendRefreshMessage(captured);
  assert.equal(result.ok, true);
  assert.equal(storage.shortKeyword, '');
  assert.deepEqual(Array.from(storage.keywords), ['links.example.com', 'links', 'jira']);
});

test('an uppercase configured short keyword is lowercased for matching', async () => {
  // Typed keywords are lowercased and URL hostnames are inherently lowercase,
  // so a verbatim "Go" could never match without normalization.
  const storage = { ...OVERRIDE_STORAGE, shortKeyword: 'Go', keywords: ['links.example.com'] };
  const { context, captured } = loadBackground({ storage });
  await context.updateRedirectRules();
  assert.equal(applyDNR(captured.dnrRules, 'http://go/slack'), 'https://links.example.com/slack');
  await navigate(captured, googleSearch('go/slack'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://links.example.com/slack' }]);
});

test('DNR rule for the configured short keyword swaps host without double prefix', async () => {
  const { context, captured } = loadBackground({ storage: { ...OVERRIDE_STORAGE } });
  await context.updateRedirectRules();
  assert.equal(applyDNR(captured.dnrRules, 'http://go/slack'), 'https://links.example.com/slack');
  // Template keywords still route via the keyword path segment.
  assert.equal(applyDNR(captured.dnrRules, 'http://jira/ABC-123'), 'https://links.example.com/jira/ABC-123');
});

test('search interception honors the configured short keyword', async () => {
  const { captured } = loadBackground({ storage: { ...OVERRIDE_STORAGE } });
  await navigate(captured, googleSearch('go/slack'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://links.example.com/slack' }]);
});

test('configured short keyword intercepts even when storage keywords are stale', async () => {
  const { captured } = loadBackground({
    storage: { ...OVERRIDE_STORAGE, keywords: ['links.example.com'] },
  });
  await navigate(captured, googleSearch('go/slack'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://links.example.com/slack' }]);
});

test('direct navigation to the configured short keyword host redirects (Firefox path)', async () => {
  const { captured } = loadBackground({ storage: { ...OVERRIDE_STORAGE } });
  await navigate(captured, 'http://go/slack');
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://links.example.com/slack' }]);
});

test('without a stored short keyword the hostname-derived alias still works', async () => {
  const { captured } = loadBackground({
    storage: { baseURL: 'https://links.example.com', keywords: ['links.example.com'] },
  });
  await navigate(captured, googleSearch('links/slack'));
  assert.deepEqual(captured.tabUpdates, [{ tabId: 7, url: 'https://links.example.com/slack' }]);
});
