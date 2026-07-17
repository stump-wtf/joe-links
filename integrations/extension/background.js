// Governing: SPEC-0008 REQ "Search Interception and Redirect", ADR-0012
'use strict';

// Known search engines and their query parameter names.
// Governing: SPEC-0008 REQ "Search Interception and Redirect"
// Google serves country domains (google.co.uk, google.de, google.com.au, …).
const GOOGLE_HOST_RE = /^(www\.)?google\.([a-z]{2,3})(\.[a-z]{2})?$/;
function getSearchQuery(url) {
  const h = url.hostname;
  const p = url.pathname;
  const s = url.searchParams;
  if (GOOGLE_HOST_RE.test(h) && p === '/search') return s.get('q');
  if ((h === 'www.bing.com' || h === 'bing.com') && p === '/search') return s.get('q');
  if ((h === 'duckduckgo.com' || h === 'start.duckduckgo.com') && p === '/') return s.get('q');
  if (h === 'search.yahoo.com' && p.startsWith('/search')) return s.get('p');
  if (h === 'search.brave.com' && p === '/search') return s.get('q');
  if (h === 'www.ecosia.org' && p === '/search') return s.get('q');
  if (h === 'www.qwant.com' && p === '/') return s.get('q');
  if (h === 'kagi.com' && p === '/search') return s.get('q');
  if (h === 'www.perplexity.ai' && p === '/search') return s.get('q');
  return null;
}

// Pattern: keyword/slug — keyword is alphanumeric+hyphens (case-insensitive), slug is any
// non-whitespace run. Queries containing spaces (e.g. "go/no-go decision matrix") are real
// searches and must fall through to the search engine.
// Governing: SPEC-0008 REQ "Search Interception and Redirect", REQ "Fallthrough Safety"
const KEYWORD_RE = /^([A-Za-z][A-Za-z0-9-]*)\/(\S+)$/;

const DEFAULTS = { baseURL: 'http://go', keywords: ['go'], shortKeyword: '' };

// Resolve the server's short-link prefix: prefer the value discovered from
// GET /api/v1/config (JOE_SHORT_KEYWORD deployments where the prefix differs
// from the hostname's first label, e.g. "go/" on links.example.com), else
// derive it from the server hostname's first label. Lowercased: typed keywords
// are lowercased before comparison and URL hostnames are inherently lowercase,
// so an uppercase JOE_SHORT_KEYWORD (e.g. "Go") could never match verbatim.
// Governing: SPEC-0008 REQ "Keyword Host Discovery"
function resolveServerKeyword(serverHost, storedShortKeyword) {
  return (typeof storedShortKeyword === 'string' && storedShortKeyword)
    ? storedShortKeyword.toLowerCase()
    : serverHost.split('.')[0];
}

// Draw a clean chain-link icon programmatically at a given size.
// Avoids the diagonal-stroke "X" appearance of the PNG at 16px.
// Governing: ADR-0012
function makeIconData(size) {
  try {
    const canvas = new OffscreenCanvas(size, size);
    const ctx = canvas.getContext('2d');
    const s = size;
    // Purple background
    ctx.fillStyle = '#7c3aed';
    ctx.beginPath();
    ctx.roundRect(0, 0, s, s, s * 0.18);
    ctx.fill();
    // Draw two horizontal chain-link ovals
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = Math.max(1.5, s * 0.11);
    ctx.lineCap = 'round';
    const ry = s * 0.15;  // oval vertical radius
    const rx = s * 0.24;  // oval horizontal radius
    const gap = s * 0.06;
    const cy = s / 2;
    const x1 = s * 0.5 - gap / 2 - rx * 0.5; // left link center x
    const x2 = s * 0.5 + gap / 2 + rx * 0.5; // right link center x
    // Left oval
    ctx.beginPath();
    ctx.ellipse(x1, cy, rx, ry, 0, 0, 2 * Math.PI);
    ctx.stroke();
    // Right oval (overlapping)
    ctx.beginPath();
    ctx.ellipse(x2, cy, rx, ry, 0, 0, 2 * Math.PI);
    ctx.stroke();
    // Cover the inner overlap area so links look joined
    ctx.fillStyle = '#7c3aed';
    ctx.fillRect(x1 + rx * 0.55, cy - ry - 1, x2 - x1 - rx * 1.1, ry * 2 + 2);
    // Redraw right half of left oval and left half of right oval over the cover
    ctx.strokeStyle = '#ffffff';
    ctx.beginPath();
    ctx.ellipse(x1, cy, rx, ry, 0, -Math.PI / 2, Math.PI / 2);
    ctx.stroke();
    ctx.beginPath();
    ctx.ellipse(x2, cy, rx, ry, 0, Math.PI / 2, (3 * Math.PI) / 2);
    ctx.stroke();
    return ctx.getImageData(0, 0, s, s);
  } catch {
    return null;
  }
}

// Set the browser action icon using programmatically drawn image data.
async function setActionIcon() {
  try {
    const imageData = {};
    for (const size of [16, 48, 128]) {
      const data = makeIconData(size);
      if (data) imageData[size] = data;
    }
    if (Object.keys(imageData).length > 0) {
      await chrome.action.setIcon({ imageData });
    }
  } catch {
    // setIcon not available or OffscreenCanvas not supported — fall back to PNG.
  }
}

// Install declarativeNetRequest dynamic rules that redirect keyword hostnames
// (e.g. http(s)://go/*) to the real server before Chrome opens a socket.
// This avoids the async race in onBeforeNavigate where Chrome's TLS failure
// can beat the storage read.
// Requires Chrome 90+ or Firefox 127+; older versions degrade gracefully.
async function updateRedirectRules() {
  try {
    const { baseURL, keywords, shortKeyword } = await chrome.storage.local.get({
      baseURL: DEFAULTS.baseURL,
      keywords: DEFAULTS.keywords,
      shortKeyword: DEFAULTS.shortKeyword,
    });
    let serverURL;
    try { serverURL = new URL(baseURL); } catch { return; }
    const serverHost = serverURL.hostname;
    const scheme = serverURL.protocol.slice(0, -1); // strip trailing ':'
    const kws = Array.isArray(keywords) ? keywords : DEFAULTS.keywords;

    // Always include the short alias (the server-configured short keyword, else
    // e.g. 'go' from 'go.stump.rocks') so a declarativeNetRequest rule is
    // created even when storage keywords are stale.
    const serverKeyword = resolveServerKeyword(serverHost, shortKeyword);
    const allKeywords = [...new Set([...kws, serverKeyword])].filter(k => k !== serverHost);

    // One rule per keyword that differs from the server hostname.
    // A plain host swap is only correct for the server's own alias; template keywords
    // need their keyword path segment preserved (http://jira/ABC-123 must become
    // {baseURL}/jira/ABC-123, not {baseURL}/ABC-123) — mirrors redirectFor() below.
    // Governing: SPEC-0008 REQ "Search Interception and Redirect"
    const addRules = allKeywords
      .map((keyword, i) => ({
        id: i + 1,
        priority: 1,
        action: {
          type: 'redirect',
          redirect: {
            regexSubstitution: keyword === serverKeyword
              ? `${scheme}://${serverHost}\\1`
              : `${scheme}://${serverHost}/${keyword}\\1`,
          },
        },
        condition: {
          regexFilter: `^https?://${keyword.replace(/\./g, '\\.')}(/.*)?$`,
          resourceTypes: ['main_frame'],
        },
      }));

    const existing = await chrome.declarativeNetRequest.getDynamicRules();
    await chrome.declarativeNetRequest.updateDynamicRules({
      removeRuleIds: existing.map(r => r.id),
      addRules,
    });
  } catch {
    // declarativeNetRequest not available in this browser version — no-op.
  }
}

// Fetch the server-configured short keyword from GET /api/v1/config.
// Returns '' only when the endpoint is definitively absent (404 — a pre-#210
// server), so callers fall back to deriving the prefix from the hostname's
// first label. Transient failures (5xx, timeout, network error) return null so
// callers keep a previously discovered value — one blip during the hourly
// refresh must not drop a working alias until the next successful refresh.
// Never throws: a /config failure must NOT fail the keyword refresh.
// Governing: SPEC-0008 REQ "Keyword Host Discovery"
async function fetchShortKeyword(baseURL, headers) {
  try {
    const res = await fetch(`${baseURL}/api/v1/config`, {
      signal: AbortSignal.timeout(5000),
      headers,
    });
    if (res.status === 404) return ''; // endpoint absent — older server
    if (!res.ok) return null;          // transient server error — keep stored value
    const data = await res.json();
    return (data && typeof data.short_keyword === 'string') ? data.short_keyword : '';
  } catch {
    return null; // network error / timeout — keep stored value
  }
}

// Governing: SPEC-0008 REQ "Keyword Host Discovery", REQ "API Key Authentication"
// Returns { ok: true } on success or { ok: false, error } so callers (the options
// page's "Refresh now" and save flows) can surface failures instead of a false success.
async function refreshKeywords() {
  const { baseURL, apiKey, shortKeyword: storedShortKeyword } = await chrome.storage.local.get({
    baseURL: DEFAULTS.baseURL,
    apiKey: '',
    shortKeyword: DEFAULTS.shortKeyword,
  });
  const headers = {};
  if (apiKey) headers['Authorization'] = `Bearer ${apiKey}`;
  try {
    const res = await fetch(`${baseURL}/api/v1/keywords`, {
      signal: AbortSignal.timeout(5000),
      headers,
    });
    if (!res.ok) return { ok: false, error: `Server returned ${res.status}.` };
    const data = await res.json();
    if (!Array.isArray(data)) return { ok: false, error: 'Unexpected response from server.' };
    // Discover the configured short prefix (JOE_SHORT_KEYWORD). Stored as ''
    // when definitively absent (404 — older server) so the hostname fallback
    // stays dynamic if baseURL changes; on a transient /config failure (null)
    // keep the previously stored value instead of wiping a working alias.
    const fetched = await fetchShortKeyword(baseURL, headers);
    const shortKeyword = fetched === null ? storedShortKeyword : fetched;
    // Always include the canonical hostname and its short alias (the configured
    // short keyword, else the first hostname label — e.g. 'go.stump.rocks' and
    // 'go') so declarativeNetRequest rules are created even when no keyword
    // templates are configured on the server.
    const canonical = new URL(baseURL).hostname;
    const serverKeyword = resolveServerKeyword(canonical, shortKeyword);
    const merged = [...new Set([canonical, serverKeyword, ...data])];
    await chrome.storage.local.set({ keywords: merged, shortKeyword });
    return { ok: true };
  } catch {
    // Server unreachable — keep existing keyword list.
    // Governing: SPEC-0008 REQ "Keyword Host Discovery" scenario "Server is unreachable"
    return { ok: false, error: 'Could not reach server.' };
  }
}

// Create the periodic refresh alarm if it doesn't already exist with the right
// period. Runs on every service-worker start because Firefox drops alarms across
// browser restarts and a Chrome disable→enable cycle clears them without firing
// onInstalled or onStartup. The get() guard matters: chrome.alarms.create with an
// existing name resets its timer, and MV3 workers wake frequently. The period
// check matters too: Chrome persists alarms across extension updates, so a stale
// 5-minute alarm created by v1.2.4–v1.2.8 must be replaced, not kept.
// Governing: SPEC-0008 REQ "Keyword Host Discovery" — default refresh interval is 60 minutes.
async function ensureKeywordRefreshAlarm() {
  try {
    const existing = await chrome.alarms.get('keyword-refresh');
    if (!existing || existing.periodInMinutes !== 60) {
      chrome.alarms.create('keyword-refresh', { periodInMinutes: 60 });
    }
  } catch {
    // alarms.get failed (unlikely) — create unconditionally rather than lose the refresh.
    chrome.alarms.create('keyword-refresh', { periodInMinutes: 60 });
  }
}
ensureKeywordRefreshAlarm();

// Governing: SPEC-0008 REQ "Keyword Host Discovery", REQ "On-Install Setup"
chrome.runtime.onInstalled.addListener(async (details) => {
  if (details.reason === 'install') {
    // Governing: SPEC-0008 REQ "On-Install Setup"
    const { baseURL } = await chrome.storage.local.get({ baseURL: '' });
    if (!baseURL) {
      chrome.tabs.create({ url: chrome.runtime.getURL('options.html') });
    }
  }
  await refreshKeywords();
  await updateRedirectRules();
  await setActionIcon();
  await ensureKeywordRefreshAlarm();
});

chrome.runtime.onStartup.addListener(async () => {
  await refreshKeywords();
  await updateRedirectRules();
  await setActionIcon();
  await ensureKeywordRefreshAlarm();
});

chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name === 'keyword-refresh') {
    await refreshKeywords();
    await updateRedirectRules();
  }
});

// Allow the options page to trigger a keyword refresh after a base URL change.
// Responds with refreshKeywords()'s { ok, error? } so the page can report failures.
chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type === 'refresh-keywords') {
    refreshKeywords()
      .then(async (result) => {
        await updateRedirectRules();
        sendResponse(result);
      })
      .catch(() => sendResponse({ ok: false, error: 'Refresh failed.' }));
    return true; // keep channel open for async response
  }
});

// Governing: SPEC-0008 REQ "Search Interception and Redirect"
// Intercept navigations to search engines or direct keyword hostnames (Firefox).
chrome.webNavigation.onBeforeNavigate.addListener(async (details) => {
  // Only handle main-frame navigations.
  if (details.frameId !== 0) return;

  let url;
  try { url = new URL(details.url); } catch { return; }

  // Combine storage reads into a single call for efficiency.
  const { baseURL, keywords, shortKeyword } = await chrome.storage.local.get({
    baseURL: DEFAULTS.baseURL,
    keywords: DEFAULTS.keywords,
    shortKeyword: DEFAULTS.shortKeyword,
  });
  const kws = Array.isArray(keywords) ? keywords : DEFAULTS.keywords;
  const serverHost = new URL(baseURL).hostname;
  // Short alias: the server-configured short keyword (JOE_SHORT_KEYWORD), else
  // the first hostname label (e.g. 'go' from 'go.stump.rocks').
  const serverKeyword = resolveServerKeyword(serverHost, shortKeyword);

  // Build the redirect URL for a keyword+slug pair.
  // If the keyword matches the server hostname or its short alias, route directly to
  // baseURL/slug — avoids a double-prefix (e.g. /go/slack on a go.stump.rocks server).
  // Otherwise use path-based keyword routing: baseURL/keyword/slug.
  function redirectFor(keyword, slug) {
    return (keyword === serverHost || keyword === serverKeyword)
      ? `${baseURL}/${slug}`
      : `${baseURL}/${keyword}/${slug}`;
  }

  // Case 1: Search engine interception.
  // Governing: SPEC-0008 REQ "Fallthrough Safety" — only exact keyword/slug matches intercept.
  const query = getSearchQuery(url);
  if (query) {
    const match = query.match(KEYWORD_RE);
    if (match) {
      const [, rawKeyword, slug] = match;
      const keyword = rawKeyword.toLowerCase();
      // Accept stored keywords OR the server's own short alias (guards against stale storage).
      if (kws.includes(keyword) || keyword === serverKeyword) {
        // The slug comes from decoded search-query text — percent-encode it (keeping
        // "/" for multi-segment slugs) so e.g. "go/100%" can't build an invalid URL,
        // and swallow tabs.update rejections (closed tab, invalid URL).
        const encoded = encodeURI(slug).replace(/#/g, '%23');
        Promise.resolve(chrome.tabs.update(details.tabId, { url: redirectFor(keyword, encoded) })).catch(() => {});
        return;
      }
    }
  }

  // Case 2: Direct navigation to a keyword hostname (Firefox address bar behavior).
  // Firefox treats "go/slack" as a direct URL http://go/slack rather than routing
  // through a search engine, bypassing Case 1. Intercept and route via the server.
  // Also guards against stale storage by accepting serverKeyword directly.
  // Governing: SPEC-0008 REQ "Search Interception and Redirect"
  if ((kws.includes(url.hostname) || url.hostname === serverKeyword) && url.hostname !== serverHost && url.pathname.length > 1) {
    // Keep the query string — the declarativeNetRequest path preserves it, so the
    // fallback must behave identically. pathname/search are already percent-encoded.
    const slug = url.pathname.slice(1) + url.search; // strip leading "/"
    Promise.resolve(chrome.tabs.update(details.tabId, { url: redirectFor(url.hostname, slug) })).catch(() => {});
  }
});
