// Governing: SPEC-0008 REQ "Browser Action — Create Link", ADR-0012
'use strict';

const DEFAULTS = { baseURL: 'http://go', apiKey: '' };

const ICON_CLIPBOARD = `<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2"><path stroke-linecap="round" stroke-linejoin="round" d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z"/></svg>`;
const ICON_CHECK     = `<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2.5"><path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7"/></svg>`;

// Parse an SVG string into a DOM node (avoids innerHTML for AMO compliance).
function svgNode(svgStr) {
  return new DOMParser().parseFromString(svgStr, 'image/svg+xml').documentElement;
}

// Return a copy button that copies text and briefly shows a checkmark.
function makeCopyBtn(text) {
  const btn = document.createElement('button');
  btn.className = 'copy-btn';
  btn.replaceChildren(svgNode(ICON_CLIPBOARD));
  btn.title = 'Copy';
  btn.addEventListener('click', () => {
    navigator.clipboard.writeText(text).then(() => {
      btn.replaceChildren(svgNode(ICON_CHECK));
      setTimeout(() => { btn.replaceChildren(svgNode(ICON_CLIPBOARD)); }, 1500);
    }).catch(() => {});
  });
  return btn;
}

// Try to extract a slug from a tab URL given a keyword URL template.
// Templates contain a {slug} placeholder (e.g. "https://jira.example.com/browse/{slug}");
// the server validates its presence (internal/handler/keywords.go). Matches the template
// prefix, strips any query/fragment, and strips the template's path suffix if it has one.
// Returns the extracted slug or null if the URL doesn't match.
function matchKeywordTemplate(tabURL, urlTemplate) {
  if (!urlTemplate) return null;
  const idx = urlTemplate.indexOf('{slug}');
  if (idx === -1) return null;
  const prefix = urlTemplate.slice(0, idx);
  const suffix = urlTemplate.slice(idx + '{slug}'.length).split('?')[0].split('#')[0];
  if (!tabURL.startsWith(prefix)) return null;
  let slug = tabURL.slice(prefix.length).split('?')[0].split('#')[0];
  if (suffix) {
    if (!slug.endsWith(suffix)) return null;
    slug = slug.slice(0, -suffix.length);
  }
  return slug || null;
}

// --- Tag pill management ---
const tagList = [];

function setupTagInput() {
  const container = document.getElementById('tags-container');
  const input     = document.getElementById('tags-input');

  function addTag(raw) {
    // Normalise: lowercase, replace non-alphanumeric runs with hyphens, strip leading/trailing hyphens.
    const tag = raw.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    if (!tag || tagList.includes(tag)) return;
    tagList.push(tag);

    const pill = document.createElement('span');
    pill.className = 'tag-pill';

    const text = document.createElement('span');
    text.textContent = tag;

    const removeBtn = document.createElement('button');
    removeBtn.className = 'tag-pill-remove';
    removeBtn.textContent = '\u00d7';
    removeBtn.title = 'Remove';
    removeBtn.type = 'button';
    removeBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      tagList.splice(tagList.indexOf(tag), 1);
      pill.remove();
    });

    pill.appendChild(text);
    pill.appendChild(removeBtn);
    container.insertBefore(pill, input);
  }

  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',' || e.key === 'Tab') {
      if (input.value.trim()) {
        e.preventDefault();
        addTag(input.value);
        input.value = '';
      }
    } else if (e.key === 'Backspace' && !input.value && tagList.length > 0) {
      const last = tagList[tagList.length - 1];
      const pills = container.querySelectorAll('.tag-pill');
      if (pills.length > 0) {
        pills[pills.length - 1].remove();
        tagList.splice(tagList.indexOf(last), 1);
      }
    }
  });

  input.addEventListener('blur', () => {
    if (input.value.trim()) {
      addTag(input.value);
      input.value = '';
    }
  });

  // Clicking anywhere in the tags row focuses the input.
  container.addEventListener('click', () => input.focus());
}

// Governing: SPEC-0017 REQ "Extension Meta Extraction"
async function extractPageMeta(tabId) {
  try {
    const results = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => ({
        title: document.title || '',
        description: document.querySelector('meta[name="description"]')?.content || '',
      }),
    });
    return results?.[0]?.result || {};
  } catch {
    // Fails on chrome:// pages, privileged pages, etc. — fall back to empty
    return {};
  }
}

// Governing: SPEC-0017 REQ "Extension Suggestion Strip"
async function fetchSuggestions(baseURL, apiKey, tabURL, tabTitle, tabId) {
  try {
    const meta = await extractPageMeta(tabId);
    const title = meta.title || tabTitle || '';
    const description = meta.description || '';

    const res = await fetch(`${baseURL}/api/v1/links/suggest`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${apiKey}`,
      },
      body: JSON.stringify({ url: tabURL, title, description }),
      signal: AbortSignal.timeout(8000),
    });

    if (res.ok) {
      const data = await res.json();
      renderSuggestionStrip(data);
    }
    // Non-200 or network error — silently do nothing
  } catch {
    // Silently ignore errors (503 no LLM, 502 LLM failure, network, timeout)
  }
}

// Governing: SPEC-0017 REQ "Extension Suggestion Strip"
function renderSuggestionStrip(suggestions) {
  if (!suggestions) return;

  const fields = [];
  if (suggestions.slug) fields.push({ key: 'slug', label: 'Slug', value: suggestions.slug });
  if (suggestions.title) fields.push({ key: 'title', label: 'Title', value: suggestions.title });
  if (suggestions.description) fields.push({ key: 'desc', label: 'Desc', value: suggestions.description });
  if (suggestions.tags && suggestions.tags.length > 0) fields.push({ key: 'tags', label: 'Tags', value: suggestions.tags.join(', ') });

  if (fields.length === 0) return;

  const container = document.getElementById('suggest-container');
  const strip = document.createElement('div');
  strip.className = 'suggest-strip';

  // Header row
  const header = document.createElement('div');
  header.className = 'suggest-strip-header';

  const label = document.createElement('span');
  label.className = 'suggest-strip-label';
  label.textContent = '\u2726 Suggested';

  const dismiss = document.createElement('button');
  dismiss.className = 'suggest-dismiss';
  dismiss.textContent = '\u00d7';
  dismiss.title = 'Dismiss suggestions';
  dismiss.addEventListener('click', () => strip.remove());

  header.appendChild(label);
  header.appendChild(dismiss);
  strip.appendChild(header);

  // One row per field
  for (const f of fields) {
    const row = document.createElement('div');
    row.className = 'suggest-row';

    const fieldLabel = document.createElement('span');
    fieldLabel.className = 'suggest-field-label';
    fieldLabel.textContent = f.label;

    const fieldValue = document.createElement('span');
    fieldValue.className = 'suggest-field-value';
    fieldValue.textContent = f.value;
    fieldValue.title = f.value;

    const useBtn = document.createElement('button');
    useBtn.className = 'suggest-use-btn';
    useBtn.textContent = 'Use';
    useBtn.addEventListener('click', () => applySuggestion(f.key, suggestions));

    row.appendChild(fieldLabel);
    row.appendChild(fieldValue);
    row.appendChild(useBtn);
    strip.appendChild(row);
  }

  container.appendChild(strip);
}

function applySuggestion(key, suggestions) {
  if (key === 'slug') {
    document.getElementById('slug').value = suggestions.slug;
  } else if (key === 'title') {
    document.getElementById('title').value = suggestions.title;
    // Auto-expand the "More details" section so the user can see the filled value
    document.getElementById('more-details').open = true;
  } else if (key === 'desc') {
    document.getElementById('description').value = suggestions.description;
    document.getElementById('more-details').open = true;
  } else if (key === 'tags') {
    // Clear existing tags
    tagList.length = 0;
    document.querySelectorAll('.tag-pill').forEach(p => p.remove());
    // Add each suggested tag using the same logic as setupTagInput
    const container = document.getElementById('tags-container');
    const input = document.getElementById('tags-input');
    for (const raw of suggestions.tags) {
      const tag = raw.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
      if (!tag || tagList.includes(tag)) continue;
      tagList.push(tag);

      const pill = document.createElement('span');
      pill.className = 'tag-pill';

      const text = document.createElement('span');
      text.textContent = tag;

      const removeBtn = document.createElement('button');
      removeBtn.className = 'tag-pill-remove';
      removeBtn.textContent = '\u00d7';
      removeBtn.title = 'Remove';
      removeBtn.type = 'button';
      removeBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        tagList.splice(tagList.indexOf(tag), 1);
        pill.remove();
      });

      pill.appendChild(text);
      pill.appendChild(removeBtn);
      container.insertBefore(pill, input);
    }
  }
}

document.addEventListener('DOMContentLoaded', async () => {
  setupTagInput();

  const { baseURL, apiKey } = await chrome.storage.local.get(DEFAULTS);

  // Governing: SPEC-0008 REQ "Browser Action — Create Link" scenario "no API key"
  if (!apiKey) {
    document.getElementById('no-api-key').style.display = 'block';
    document.getElementById('create').disabled = true;
  }

  document.getElementById('open-options').addEventListener('click', () => {
    chrome.runtime.openOptionsPage();
  });

  document.getElementById('var-toggle').addEventListener('click', () => {
    const hint = document.getElementById('var-hint');
    hint.hidden = !hint.hidden;
  });

  // Derive the short-link prefix from the base URL hostname.
  // e.g. baseURL="https://go.stump.rocks" → serverKeyword="go"
  let serverKeyword = 'go';
  try { serverKeyword = new URL(baseURL).hostname.split('.')[0]; } catch {}

  // Show the server keyword prefix in the slug field.
  document.getElementById('slug-prefix').textContent = serverKeyword + '/';

  // Get the current tab URL and title.
  // Governing: SPEC-0008 REQ "Browser Action — Create Link" scenario "popup opens"
  const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
  const tabURL   = tabs[0]?.url   || '';
  const tabTitle = tabs[0]?.title || '';
  if (tabURL)   document.getElementById('url').value   = tabURL;
  if (tabTitle) document.getElementById('title').value = tabTitle;

  document.getElementById('slug').focus();

  if (!apiKey || !tabURL) return;

  // Governing: SPEC-0017 REQ "Extension Suggestion Strip"
  // Fire suggest request asynchronously — do NOT await, form stays interactive immediately
  const tabId = tabs[0]?.id;
  fetchSuggestions(baseURL, apiKey, tabURL, tabTitle, tabId);

  const headers = { Authorization: `Bearer ${apiKey}` };

  // Fetch existing links for this URL and keyword templates in parallel.
  const [linksResult, kwResult] = await Promise.allSettled([
    fetch(`${baseURL}/api/v1/links?url=${encodeURIComponent(tabURL)}`, {
      headers,
      signal: AbortSignal.timeout(5000),
    }),
    fetch(`${baseURL}/api/v1/keywords/templates`, {
      headers,
      signal: AbortSignal.timeout(5000),
    }),
  ]);

  // Show existing go links for this URL.
  if (linksResult.status === 'fulfilled' && linksResult.value.ok) {
    const data = await linksResult.value.json().catch(() => ({}));
    const links = data.links || [];
    if (links.length > 0) {
      renderExistingLinks(links, serverKeyword, baseURL);
    }
  }

  // Show keyword shortcut suggestions.
  if (kwResult.status === 'fulfilled' && kwResult.value.ok) {
    const templates = await kwResult.value.json().catch(() => []);
    if (Array.isArray(templates)) {
      const suggestions = [];
      for (const tpl of templates) {
        const slug = matchKeywordTemplate(tabURL, tpl.url_template);
        if (slug) suggestions.push({ keyword: tpl.keyword, slug });
      }
      if (suggestions.length > 0) {
        renderKeywordSuggestions(suggestions, baseURL);
        // Pre-fill the slug field with the first suggestion's slug if it's empty.
        // Template matches preserve case and can span path segments ("PROJ-123",
        // "a/b"), but the create form's slug must satisfy the server's slug rules
        // ([a-z0-9][a-z0-9-]*[a-z0-9]) — lowercase first, and skip the pre-fill
        // entirely if the value still wouldn't pass, so we never pre-fill a slug
        // the server would reject. The keyword suggestion rows above keep the
        // original case because template substitution is case-preserving.
        const slugInput = document.getElementById('slug');
        const prefill = suggestions[0].slug.toLowerCase();
        if (!slugInput.value && /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(prefill)) {
          slugInput.value = prefill;
        }
      }
    }
  }
});

const ICON_EDIT = `<svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2"><path stroke-linecap="round" stroke-linejoin="round" d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z"/></svg>`;

// Render the "Go links for this URL" section.
function renderExistingLinks(links, serverKeyword, baseURL) {
  const container = document.getElementById('existing-links');
  const section = document.createElement('div');
  section.className = 'match-section';

  const label = document.createElement('div');
  label.className = 'match-section-label';
  label.textContent = links.length === 1 ? 'Go link for this URL' : 'Go links for this URL';
  section.appendChild(label);

  for (const link of links) {
    const short = `${serverKeyword}/${link.slug}`;
    const row = document.createElement('div');
    row.className = 'match-row';

    const span = document.createElement('span');
    span.className = 'match-link';
    span.textContent = short;

    const editBtn = document.createElement('button');
    editBtn.className = 'copy-btn';
    editBtn.replaceChildren(svgNode(ICON_EDIT));
    editBtn.title = 'Edit link';
    editBtn.addEventListener('click', () => {
      chrome.tabs.create({ url: `${baseURL}/dashboard/links/${link.id}/edit` });
    });

    row.appendChild(span);
    row.appendChild(editBtn);
    // Copy the full URL — bare "go/slug" only works for recipients who also run the extension.
    row.appendChild(makeCopyBtn(`${baseURL}/${link.slug}`));
    section.appendChild(row);
  }

  container.appendChild(section);
}

// Render the "Keyword shortcuts" section for matching keyword templates.
function renderKeywordSuggestions(suggestions, baseURL) {
  const container = document.getElementById('keyword-suggestions');
  const section = document.createElement('div');
  section.className = 'kw-section';

  const label = document.createElement('div');
  label.className = 'kw-section-label';
  label.textContent = 'Keyword shortcut' + (suggestions.length > 1 ? 's' : '');
  section.appendChild(label);

  for (const { keyword, slug } of suggestions) {
    const short = `${keyword}/${slug}`;
    const row = document.createElement('div');
    row.className = 'kw-row';

    const span = document.createElement('span');
    span.className = 'kw-link';
    span.textContent = short;

    row.appendChild(span);
    // Copy the full path-routed URL — the server resolves /{keyword}/{slug} for everyone.
    row.appendChild(makeCopyBtn(`${baseURL}/${keyword}/${slug}`));
    section.appendChild(row);
  }

  container.appendChild(section);
}

document.getElementById('create').addEventListener('click', async () => {
  const urlInput  = document.getElementById('url');
  const slugInput = document.getElementById('slug');
  const btn       = document.getElementById('create');

  const url  = urlInput.value.trim();
  const slug = slugInput.value.trim();

  if (!url || !slug) {
    showError('URL and slug are required.');
    return;
  }

  // Flush any pending tag in the input box.
  const tagsInput = document.getElementById('tags-input');
  if (tagsInput.value.trim()) {
    const raw = tagsInput.value.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    if (raw && !tagList.includes(raw)) tagList.push(raw);
    tagsInput.value = '';
  }

  btn.disabled    = true;
  btn.textContent = 'Creating…';
  clearStatus();

  const { baseURL, apiKey } = await chrome.storage.local.get(DEFAULTS);

  // Derive short-link prefix.
  let serverKeyword = 'go';
  try { serverKeyword = new URL(baseURL).hostname.split('.')[0]; } catch {}

  const headers = { 'Content-Type': 'application/json' };
  if (apiKey) headers['Authorization'] = `Bearer ${apiKey}`;

  const title       = document.getElementById('title').value.trim();
  const description = document.getElementById('description').value.trim();
  // Governing: SPEC-0010 REQ "REST API Visibility Field" — defaults to "public"
  const visibility  = document.getElementById('visibility').value;

  const body = { url, slug, visibility };
  if (title)            body.title       = title;
  if (description)      body.description = description;
  if (tagList.length)   body.tags        = [...tagList];

  try {
    // Governing: SPEC-0008 REQ "Browser Action — Create Link" scenario "submit form"
    const res = await fetch(`${baseURL}/api/v1/links`, {
      method: 'POST',
      headers,
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(10000),
    });

    if (res.ok) {
      const data     = await res.json();
      const linkSlug = data.slug || slug;
      // Display the short form (e.g. "go/my-link") but copy the full URL — the bare
      // short form only resolves for recipients who also run the extension.
      const shortLink = `${serverKeyword}/${linkSlug}`;
      const fullLink  = `${baseURL}/${linkSlug}`;

      // Auto-copy full link to clipboard.
      // Governing: SPEC-0008 REQ "Browser Action — Create Link" scenario "successful link creation"
      const copied = await navigator.clipboard.writeText(fullLink).then(() => true).catch(() => false);

      showSuccess(shortLink, fullLink, copied);
      slugInput.value = '';
      // Clear tags and reset visibility after successful creation.
      tagList.length = 0;
      document.querySelectorAll('.tag-pill').forEach(p => p.remove());
      document.getElementById('visibility').value = 'public';
    } else {
      const errData = await res.json().catch(() => ({}));
      const msg = errData.error || errData.message || `Error ${res.status}`;
      // Governing: SPEC-0008 REQ "Browser Action — Create Link" scenario "POST fails"
      showError(msg);
    }
  } catch (err) {
    showError(err.message || 'Network error');
  } finally {
    btn.disabled    = false;
    btn.textContent = 'Create Link';
  }
});

function clearStatus() {
  const el = document.getElementById('status');
  el.replaceChildren();
  el.style.display = 'none';
}

function showSuccess(shortLink, fullLink, copied = false) {
  const el  = document.getElementById('status');
  const box = document.createElement('div');
  box.className = 'status-box success';

  const label = document.createElement('span');
  label.className   = 'status-label';
  label.textContent = copied ? 'Link created — copied to clipboard!' : 'Link created';

  const row = document.createElement('div');
  row.className = 'status-link-row';

  const link = document.createElement('span');
  link.className   = 'status-link';
  link.textContent = shortLink;

  row.appendChild(link);
  row.appendChild(makeCopyBtn(fullLink));
  box.appendChild(label);
  box.appendChild(row);
  el.appendChild(box);
  el.style.display = 'block';
}

function showError(msg) {
  const el  = document.getElementById('status');
  const box = document.createElement('div');
  box.className = 'status-box error';

  const label = document.createElement('span');
  label.className   = 'status-label';
  label.textContent = 'Error';

  const text = document.createElement('span');
  text.className   = 'status-msg';
  text.textContent = msg;

  box.appendChild(label);
  box.appendChild(text);
  el.appendChild(box);
  el.style.display = 'block';
}
