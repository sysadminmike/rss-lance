/**
 * RSS-Lance - Reading pane module (continuous scroll)
 *
 * Renders articles in a vertical stream so the user can keep scrolling
 * through consecutive articles without returning to the list.  The
 * middle article-list pane highlights whichever article is currently
 * in view.  Articles are rendered 10 at a time with 50 buffered
 * ahead for smooth scrolling.
 */
import { apiFetch } from './app.js';
import { markArticleReadInList, markArticleStarredInList, getArticles, highlightArticleInList, fetchMoreArticles, hasMoreArticles } from './articles.js';
import { updateUnreadCount } from './feeds.js';

const placeholder   = document.getElementById('reader-placeholder');
const readerContent = document.getElementById('reader-content');
const readerStream  = document.getElementById('reader-stream');
const readerPane    = document.getElementById('reader-pane');

const BATCH_SIZE    = 15;   // articles to render per batch
const BUFFER_TARGET = 100;  // how many articles to keep fetched ahead
const BUFFER_LOW    = 50;   // refill buffer when remaining drops below this

// ── State ─────────────────────────────────────────────────────────────────────
let _articles        = [];            // reference to current feed's article list
let _renderedIds     = new Set();
let _nextIndex       = 0;            // next index in _articles to render
let _loading         = false;        // prevents concurrent batch loads
let _articleCache    = new Map();    // articleId → full article data
let _currentVisibleId = null;
let _suppressAutoRead = false;   // true after click-to-scroll; cleared on real user scroll

function _enableAutoReadOnUserScroll() {
  // Re-enable auto-read the moment the user scrolls by themselves
  function handler() {
    _suppressAutoRead = false;
    readerPane.removeEventListener('wheel', handler);
    readerPane.removeEventListener('touchstart', handler);
  }
  readerPane.addEventListener('wheel', handler, { once: true, passive: true });
  readerPane.addEventListener('touchstart', handler, { once: true, passive: true });
}

// Observers
let _loadObserver = null;   // triggers loading more articles
let _visObserver  = null;   // tracks which article is in view
let _readObserver = null;   // auto-marks articles as read
let _sentinel     = null;

// ── Public API ────────────────────────────────────────────────────────────────

export function resetReader() {
  _articles = [];
  _renderedIds.clear();
  _nextIndex = 0;
  _loading = false;
  _articleCache.clear();
  _currentVisibleId = null;
  _suppressAutoRead = false;

  if (_loadObserver) _loadObserver.disconnect();
  if (_visObserver)  _visObserver.disconnect();
  if (_readObserver) _readObserver.disconnect();
  if (_sentinel) { _sentinel.remove(); _sentinel = null; }

  readerStream.innerHTML = '';
  readerStream.classList.add('hidden');
  readerContent.classList.add('hidden');
  placeholder.classList.remove('hidden');
}

export async function showArticle(articleId /*, previewArt – unused now */) {
  const articles = getArticles();
  const clickedIndex = articles.findIndex(a => a.article_id === articleId);
  if (clickedIndex < 0) return;

  // If article is already in the stream just scroll to it
  if (_renderedIds.has(articleId)) {
    const el = readerStream.querySelector(`[data-article-id="${CSS.escape(articleId)}"]`);
    if (el) {
      _suppressAutoRead = true;
      _enableAutoReadOnUserScroll();
      el.scrollIntoView({ behavior: 'smooth', block: 'start' });
      highlightArticleInList(articleId);
    }
    return;
  }

  // -- Build a new stream starting from the clicked article --
  _articles = articles;
  _renderedIds.clear();
  _nextIndex = clickedIndex;
  _currentVisibleId = null;
  if (_sentinel) { _sentinel.remove(); _sentinel = null; }

  placeholder.classList.add('hidden');
  readerContent.classList.add('hidden');
  readerStream.classList.remove('hidden');
  readerStream.innerHTML = '';

  showStreamLoader();
  setupObservers();
  await renderBatch();
  hideStreamLoader();

  // Start buffering articles ahead in the background
  bufferAhead();

  // Scroll to the clicked article
  _suppressAutoRead = true;
  _enableAutoReadOnUserScroll();
  const el = readerStream.querySelector(`[data-article-id="${CSS.escape(articleId)}"]`);
  if (el) {
    readerPane.scrollTop = 0;
    el.scrollIntoView({ behavior: 'instant', block: 'start' });
  }

  highlightArticleInList(articleId);

  // Mark the clicked article itself as read
  autoMarkRead(articleId);
}

// ── Observers ─────────────────────────────────────────────────────────────────

function setupObservers() {
  if (_loadObserver) _loadObserver.disconnect();
  if (_visObserver)  _visObserver.disconnect();
  if (_readObserver) _readObserver.disconnect();

  // Load more when the sentinel nears the viewport
  _loadObserver = new IntersectionObserver(entries => {
    if (entries.some(e => e.isIntersecting) && !_loading) renderBatch();
  }, { root: readerPane, rootMargin: '0px 0px 400px 0px' });

  // Track which article occupies the top slice of the pane
  _visObserver = new IntersectionObserver(entries => {
    for (const entry of entries) {
      if (entry.isIntersecting) {
        const id = entry.target.dataset.articleId;
        if (id && id !== _currentVisibleId) {
          _currentVisibleId = id;
          highlightArticleInList(id);
        }
      }
    }
  }, { root: readerPane, rootMargin: '-5% 0px -85% 0px' });

  // Auto-mark-read when the article's top edge scrolls off the top of the pane
  _readObserver = new IntersectionObserver(entries => {
    for (const entry of entries) {
      // Fire when the element leaves the viewport going upward
      if (!entry.isIntersecting && entry.boundingClientRect.top < entry.rootBounds.top) {
        if (_suppressAutoRead) continue;   // skip during programmatic scroll
        _readObserver.unobserve(entry.target);
        autoMarkRead(entry.target.dataset.articleId);
      }
    }
  }, { root: readerPane, threshold: 0 });
}

// ── Batch rendering ───────────────────────────────────────────────────────────

async function renderBatch() {
  if (_loading) return;
  _loading = true;

  showStreamLoader();

  // Remove old sentinel
  if (_sentinel) {
    _loadObserver.unobserve(_sentinel);
    _sentinel.remove();
    _sentinel = null;
  }

  // Collect next batch of preview articles
  const batch = [];
  while (batch.length < BATCH_SIZE && _nextIndex < _articles.length) {
    const art = _articles[_nextIndex];
    if (!_renderedIds.has(art.article_id)) batch.push(art);
    _nextIndex++;
  }

  // If we ran out of articles but more pages exist, fetch the next page
  if (batch.length < BATCH_SIZE && hasMoreArticles()) {
    const more = await fetchMoreArticles();
    if (more.length) {
      // Update our reference to the extended list
      _articles = getArticles();
      while (batch.length < BATCH_SIZE && _nextIndex < _articles.length) {
        const art = _articles[_nextIndex];
        if (!_renderedIds.has(art.article_id)) batch.push(art);
        _nextIndex++;
      }
    }
  }

  if (batch.length === 0) { _loading = false; hideStreamLoader(); return; }

  // Fetch full content in one batch query
  const fullArts = await fetchArticleBatch(batch);

  // Append each article block
  for (const art of fullArts) {
    const block = createArticleBlock(art);
    readerStream.appendChild(block);
    _renderedIds.add(art.article_id);
    _visObserver.observe(block);
    if (!art.is_read) _readObserver.observe(block);
  }

  // Re-add sentinel if more articles remain (in list or from API)
  if (_nextIndex < _articles.length || hasMoreArticles()) {
    _sentinel = document.createElement('div');
    _sentinel.className = 'reader-stream-sentinel';
    readerStream.appendChild(_sentinel);
    _loadObserver.observe(_sentinel);
  }

  _loading = false;
  hideStreamLoader();
  bufferAhead();
}

// ── Fetching & caching ────────────────────────────────────────────────────────

/** Fetch multiple articles with full content in a single request. */
async function fetchArticleBatch(previewArts) {
  // Separate cached from uncached
  const uncachedIds = [];
  const uncachedFallbacks = new Map();
  const results = [];

  for (const a of previewArts) {
    if (_articleCache.has(a.article_id)) {
      results.push(_articleCache.get(a.article_id));
    } else {
      uncachedIds.push(a.article_id);
      uncachedFallbacks.set(a.article_id, a);
    }
  }

  if (uncachedIds.length > 0) {
    try {
      const arts = await apiFetch('/api/articles/batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ids: uncachedIds }),
      });
      const artMap = new Map(arts.map(a => [a.article_id, a]));
      for (const id of uncachedIds) {
        const art = artMap.get(id) || uncachedFallbacks.get(id) || { article_id: id, title: '(failed to load)', content: '' };
        _articleCache.set(id, art);
        results.push(art);
      }
    } catch (e) {
      console.error('Batch fetch failed', e);
      for (const id of uncachedIds) {
        const fb = uncachedFallbacks.get(id) || { article_id: id, title: '(failed to load)', content: '' };
        results.push(fb);
      }
    }
  }

  return results;
}

async function fetchArticle(articleId, fallback) {
  if (_articleCache.has(articleId)) return _articleCache.get(articleId);
  try {
    const art = await apiFetch(`/api/articles/${articleId}`);
    _articleCache.set(articleId, art);
    return art;
  } catch (e) {
    console.error(`Failed to fetch article ${articleId}`, e);
    return fallback || { article_id: articleId, title: '(failed to load)', content: '' };
  }
}

/** Prefetch upcoming articles in batches (single query per chunk). */
async function prefetchBatch() {
  const end = Math.min(_nextIndex + BUFFER_TARGET, _articles.length);
  const toFetch = [];
  for (let i = _nextIndex; i < end; i++) {
    const a = _articles[i];
    if (!_articleCache.has(a.article_id)) toFetch.push(a);
  }
  if (toFetch.length === 0) return;

  // Fetch in chunks of 50 to avoid overly large requests
  for (let i = 0; i < toFetch.length; i += 50) {
    const chunk = toFetch.slice(i, i + 50);
    const ids = chunk.map(a => a.article_id);
    try {
      const arts = await apiFetch('/api/articles/batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ids }),
      });
      for (const art of arts) _articleCache.set(art.article_id, art);
    } catch (e) {
      console.error('Prefetch batch failed', e);
    }
  }
}

/** Keep the buffer full: prefetch article content and pull more pages from API. */
async function bufferAhead() {
  // Keep fetching API pages until we have BUFFER_TARGET articles ahead
  while ((_articles.length - _nextIndex) < BUFFER_LOW && hasMoreArticles()) {
    const more = await fetchMoreArticles();
    if (!more.length) break;
    _articles = getArticles();
  }

  // Prefetch full content for the next BUFFER_TARGET articles
  prefetchBatch();
}

// ── Auto mark-read ────────────────────────────────────────────────────────────

async function autoMarkRead(articleId) {
  const art = _articleCache.get(articleId);
  if (!art || art.is_read) return;
  try {
    await apiFetch(`/api/articles/${articleId}/read`, { method: 'POST' });
    art.is_read = true;
    markArticleReadInList(articleId);
    updateUnreadCount(art.feed_id, -1);
    const block = readerStream.querySelector(`[data-article-id="${CSS.escape(articleId)}"]`);
    if (block) {
      const btn = block.querySelector('.btn-stream-read');
      if (btn) { btn.textContent = '\u25cf Read'; btn.title = 'Mark unread'; }
    }
  } catch (e) {
    console.error('auto-mark-read failed', e);
  }
}

// ── DOM helpers ───────────────────────────────────────────────────────────────

function createArticleBlock(art) {
  const block = document.createElement('article');
  block.className = 'reader-stream-article';
  block.dataset.articleId = art.article_id;

  // Header
  const header = document.createElement('header');
  header.className = 'reader-stream-header';

  const title = document.createElement('h2');
  title.className = 'reader-stream-title';
  title.textContent = art.title || '(no title)';

  const meta = document.createElement('div');
  meta.className = 'reader-stream-meta';
  if (art.author) {
    const s = document.createElement('span');
    s.textContent = `By ${art.author}`;
    meta.appendChild(s);
  }
  if (art.published_at) {
    const s = document.createElement('span');
    s.textContent = new Date(art.published_at).toLocaleString();
    meta.appendChild(s);
  }
  if (art.url) {
    const a = document.createElement('a');
    a.href = art.url;
    a.target = '_blank';
    a.rel = 'noopener';
    a.textContent = 'Original \u2197';
    meta.appendChild(a);
  }

  const actions = document.createElement('div');
  actions.className = 'reader-stream-actions';

  const starBtn = document.createElement('button');
  starBtn.className = 'btn-stream-star' + (art.is_starred ? ' starred' : '');
  starBtn.textContent = art.is_starred ? '\u2605' : '\u2606';
  starBtn.title = art.is_starred ? 'Unstar' : 'Star';
  starBtn.addEventListener('click', () => toggleStar(art.article_id, block));

  const readBtn = document.createElement('button');
  readBtn.className = 'btn-stream-read';
  readBtn.textContent = art.is_read ? '\u25cf Read' : '\u25cb Unread';
  readBtn.title = art.is_read ? 'Mark unread' : 'Mark read';
  readBtn.addEventListener('click', () => toggleRead(art.article_id, block));

  actions.appendChild(starBtn);
  actions.appendChild(readBtn);

  header.appendChild(title);
  header.appendChild(meta);
  header.appendChild(actions);

  // Body
  const body = document.createElement('div');
  body.className = 'reader-stream-body';
  body.innerHTML = sanitise(art.content || art.summary || '<em>No content.</em>');
  body.querySelectorAll('a[href]').forEach(a => { a.target = '_blank'; a.rel = 'noopener noreferrer'; });

  block.appendChild(header);
  block.appendChild(body);
  return block;
}

// ── Per-article actions ───────────────────────────────────────────────────────

async function toggleStar(articleId, block) {
  const art = _articleCache.get(articleId);
  if (!art) return;
  const next = !art.is_starred;
  try {
    await apiFetch(`/api/articles/${articleId}/${next ? 'star' : 'unstar'}`, { method: 'POST' });
    art.is_starred = next;
    const btn = block.querySelector('.btn-stream-star');
    if (btn) {
      btn.textContent = next ? '\u2605' : '\u2606';
      btn.classList.toggle('starred', next);
      btn.title = next ? 'Unstar' : 'Star';
    }
    markArticleStarredInList(articleId, next);
  } catch (e) {
    console.error('star failed', e);
  }
}

async function toggleRead(articleId, block) {
  const art = _articleCache.get(articleId);
  if (!art) return;
  const next = !art.is_read;
  try {
    await apiFetch(`/api/articles/${articleId}/${next ? 'read' : 'unread'}`, { method: 'POST' });
    art.is_read = next;
    const btn = block.querySelector('.btn-stream-read');
    if (btn) {
      btn.textContent = next ? '\u25cf Read' : '\u25cb Unread';
      btn.title = next ? 'Mark unread' : 'Mark read';
    }
    markArticleReadInList(articleId);
    updateUnreadCount(art.feed_id, next ? -1 : +1);
  } catch (e) {
    console.error('mark read failed', e);
  }
}

// ── Stream loader ─────────────────────────────────────────────────────────────

function showStreamLoader() {
  if (readerPane.querySelector('.stream-loader')) return;
  const loader = document.createElement('div');
  loader.className = 'stream-loader';
  loader.innerHTML = '<div class="stream-loader-bar"></div><span class="stream-loader-text">Loading articles…</span>';
  // Insert at top of stream if stream has content, otherwise append to pane
  if (readerStream.children.length) {
    readerStream.appendChild(loader);
  } else {
    readerStream.appendChild(loader);
  }
}

function hideStreamLoader() {
  readerPane.querySelectorAll('.stream-loader').forEach(el => el.remove());
}

// ── Sanitisation ──────────────────────────────────────────────────────────────

const SOCIAL_DOMAINS = [
  'facebook.com', 'twitter.com', 'x.com', 'linkedin.com',
  'pinterest.com', 'reddit.com', 'tumblr.com', 'whatsapp.com',
  'wa.me', 't.me', 'threads.net', 'instagram.com', 'buffer.com',
  'getpocket.com', 'stumbleupon.com', 'digg.com', 'flipboard.com',
  'mix.com',
];

const _SOCIAL_RE = new RegExp(
  '<a\\b[^>]*\\bhref=["\'][^"\']*(?:'
  + SOCIAL_DOMAINS.map(d => d.replace(/\./g, '\\.')).join('|')
  + ')[^"\']*["\'][^>]*>[\\s\\S]*?</a>',
  'gi'
);

// Class/id substrings that indicate a social-sharing container
const _SHARE_CLASS_RE = /\b(share|sharing|social[-_]?icon|social[-_]?link|social[-_]?media|social[-_]?button|social[-_]?bar|sharedaddy|sd-sharing|jetpack[-_]sharing|addtoany|a2a_kit|post-share|entry-share|wp[-_]share|ssba|ssbp|heateor|sumo[-_]share|mashsb|dpsp|novashare|shareaholic|sociable|post-socials)\b/i;

// Heading text that signals a "Share this" block
const _SHARE_HEADING_RE = /^\s*(share\s*(this)?|share\s*on|spread\s*the\s*word|tell\s*your\s*friends)\s*:?\s*$/i;

/**
 * DOM-based removal of social-sharing containers.
 * After the regex pass strips individual links, this finds parent wrappers
 * (by class/id patterns or by detecting lists full of social links) and
 * removes them entirely, cleaning up empty headings left behind too.
 */
function _stripSocialContainers(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  // 1. Remove elements whose class or id matches common sharing patterns
  root.querySelectorAll('*').forEach(el => {
    const cls = el.getAttribute('class') || '';
    const id  = el.getAttribute('id') || '';
    if (_SHARE_CLASS_RE.test(cls) || _SHARE_CLASS_RE.test(id)) {
      el.remove();
    }
  });

  // 2. Remove <ul>/<ol> where every <li> is empty or contains only social links
  root.querySelectorAll('ul, ol').forEach(list => {
    const items = list.querySelectorAll(':scope > li');
    if (items.length === 0) return;
    const allSocial = Array.from(items).every(li => {
      const text = li.textContent.trim();
      const links = li.querySelectorAll('a');
      // Empty li, or li with only social-domain links (already stripped to empty)
      if (!text && links.length === 0) return true;
      if (links.length === 0) return false;
      return Array.from(links).every(a => {
        const href = a.getAttribute('href') || '';
        return SOCIAL_DOMAINS.some(d => href.includes(d));
      });
    });
    if (allSocial) list.remove();
  });

  // 3. Remove headings whose text matches share-like wording
  root.querySelectorAll('h1, h2, h3, h4, h5, h6').forEach(h => {
    if (_SHARE_HEADING_RE.test(h.textContent)) {
      // Also remove the next sibling if it's now empty (wrapper div left behind)
      const next = h.nextElementSibling;
      if (next && !next.textContent.trim() && !next.querySelector('img, video, picture, canvas')) {
        next.remove();
      }
      h.remove();
    }
  });

  // 4. Remove now-empty wrapper divs/sections (no text, no media)
  root.querySelectorAll('div, section, aside').forEach(el => {
    if (!el.textContent.trim() && !el.querySelector('img, video, picture, canvas, svg')) {
      el.remove();
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  const result = div.innerHTML;

  // Safety check: if we removed a lot of actual text, bail out and return
  // the pre-strip HTML.  Social blocks are mostly icons/links with very
  // little prose, so stripping > 200 chars of text is suspicious.
  const textBefore = _textLen(html);
  const textAfter  = _textLen(result);
  const removed    = textBefore - textAfter;
  if (removed > 200) return html;

  return result;
}

/** Quick plain-text length of an HTML string (via a disposable template). */
function _textLen(html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  return (t.content.textContent || '').trim().length;
}

/**
 * Strip empty / whitespace-only elements that create blank gaps.
 * Runs as a separate pass so it's never reverted by the social safety check.
 */
function _stripEmptyElements(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  const _hasMedia = el => el.querySelector('img, video, picture, canvas, svg, audio, iframe');
  // \u00A0 = &nbsp; which .trim() does NOT remove
  const _isEmpty = el => !el.textContent.replace(/[\s\u00A0]/g, '') && !_hasMedia(el);

  // 1. Remove empty <p>, <span>, <a>, <li>, <figcaption> elements
  root.querySelectorAll('p, span, a, li, figcaption').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });

  // 2. Remove empty <figure> with no images left
  root.querySelectorAll('figure').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });

  // 3. Remove empty divs / sections / aside / header / footer / nav
  root.querySelectorAll('div, section, aside, header, footer, nav').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });

  // 4. Remove empty <ul> / <ol> (all <li> children were already stripped)
  root.querySelectorAll('ul, ol').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });

  // 5. Collapse runs of 3+ <br> into a single <br>
  root.querySelectorAll('br').forEach(br => {
    let count = 1;
    while (br.nextSibling) {
      const sib = br.nextSibling;
      if (sib.nodeType === Node.ELEMENT_NODE && sib.tagName === 'BR') {
        count++;
        sib.remove();
      } else if (sib.nodeType === Node.TEXT_NODE && !sib.textContent.trim()) {
        sib.remove(); // skip whitespace text nodes between <br>s
      } else {
        break;
      }
    }
    // Keep at most 2 <br>s (one blank line)
    if (count >= 3) {
      // Already collapsed to just this one br
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

// Class patterns for "related content" / site-navigation blocks embedded in feeds
const _SITE_CHROME_CLASS_RE = /\b(topic-card|hds-topic-card|hds-topic-cards|related[-_]?post|related[-_]?article|related[-_]?content|related[-_]?link|more[-_]?stories|recommended[-_]?post|keep[-_]?exploring|further[-_]?reading|you[-_]?may[-_]?also|also[-_]?read|read[-_]?next|read[-_]?more[-_]?section|wp-block-nasa-blocks-topic-cards|wp-block-nasa-blocks-article-intro|article[-_]?footer|post[-_]?footer|entry[-_]?footer|site[-_]?footer|hds-media-background|skrim[-_]overlay|cover-hover-zoom)\b/i;

// Heading text that signals a "related / keep exploring" block
const _RELATED_HEADING_RE = /^\s*(keep\s*exploring|discover\s*more|related\s*(posts?|articles?|stories|content)|you\s*may\s*also\s*like|read\s*next|further\s*reading|more\s*(from|stories|posts?|articles?))\b/i;

/**
 * Strip site-navigation / related-content blocks that feeds embed.
 * These are large promo sections (cards, image grids, "Keep Exploring")
 * that are not part of the actual article.
 */
function _stripSiteChrome(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  // 1. Remove elements whose class matches site-chrome patterns
  root.querySelectorAll('*').forEach(el => {
    const cls = el.getAttribute('class') || '';
    const id  = el.getAttribute('id') || '';
    if (_SITE_CHROME_CLASS_RE.test(cls) || _SITE_CHROME_CLASS_RE.test(id)) {
      el.remove();
    }
  });

  // 2. Remove headings that introduce related-content sections,
  //    plus the next sibling (usually the card grid)
  root.querySelectorAll('h1, h2, h3, h4, h5, h6').forEach(h => {
    if (_RELATED_HEADING_RE.test(h.textContent)) {
      const next = h.nextElementSibling;
      if (next) next.remove();
      h.remove();
    }
  });

  // 3. Remove <figure> elements that only contain a background/decorative
  //    image (class contains "background" or "cover") and no caption
  root.querySelectorAll('figure').forEach(fig => {
    const cls = fig.getAttribute('class') || '';
    if (/\b(background|cover|hero)\b/i.test(cls) && !fig.querySelector('figcaption')) {
      fig.remove();
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

const _TRACKING_PARAMS = new Set([
  'utm_source','utm_medium','utm_campaign','utm_term','utm_content',
  'utm_id','utm_source_platform','utm_creative_format','utm_marketing_tactic',
  'gclid','gclsrc','dclid','gbraid','wbraid',
  'fbclid','fb_action_ids','fb_action_types','fb_ref','fb_source',
  'msclkid',
  '_hsenc','_hsmi','__hssc','__hstc','__hsfp',
  'mc_cid','mc_eid',
  'twclid','li_fat_id','s_cid','mkt_tok','vero_id','__s',
  'obOrigUrl','ob_click_id','taboolaclickid',
  'guccounter','guce_referrer','guce_referrer_sig',
]);
const _TRACKING_PREFIXES = ['utm_'];

function _isTrackingParam(name) {
  const lower = name.toLowerCase();
  if (_TRACKING_PARAMS.has(lower)) return true;
  return _TRACKING_PREFIXES.some(p => lower.startsWith(p));
}

function _stripTrackingParams(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;
  root.querySelectorAll('a[href]').forEach(a => {
    try {
      const u = new URL(a.getAttribute('href'), 'http://dummy');
      let changed = false;
      for (const key of [...u.searchParams.keys()]) {
        if (_isTrackingParam(key)) { u.searchParams.delete(key); changed = true; }
      }
      if (changed) {
        // If the original was a relative URL, strip the dummy origin
        const orig = a.getAttribute('href');
        if (!/^https?:\/\//i.test(orig)) {
          a.setAttribute('href', u.pathname + u.search + u.hash);
        } else {
          a.setAttribute('href', u.href);
        }
      }
    } catch (_) { /* malformed URL – leave as-is */ }
  });
  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

function sanitise(html) {
  const cleaned = html
    .replace(/<script[\s\S]*?<\/script>/gi, '')
    .replace(/<style[\s\S]*?<\/style>/gi, '')
    .replace(/<iframe[\s\S]*?>/gi, '')
    .replace(/\son\w+=["'][^"']*["']/gi, '')
    .replace(_SOCIAL_RE, '');
  const deSocialed = _stripSocialContainers(cleaned);
  const deNavd = _stripSiteChrome(deSocialed);
  const deTracked = _stripTrackingParams(deNavd);
  return _stripEmptyElements(deTracked);
}
