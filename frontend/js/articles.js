/**
 * RSS-Lance - Article list module
 */
import { apiFetch } from './app.js';

const articleList   = document.getElementById('article-list');
const articlePane   = document.getElementById('article-list-pane');
const titleEl       = document.getElementById('current-feed-title');
const unreadToggle  = document.getElementById('unread-only-toggle');
const btnSortOrder  = document.getElementById('btn-sort-order');


const PAGE_SIZE = 50;

function showPaneLoader() {
  if (articlePane.querySelector('.pane-loader')) return;
  const bar = document.createElement('div');
  bar.className = 'pane-loader';
  articlePane.appendChild(bar);
}

function hidePaneLoader() {
  articlePane.querySelectorAll('.pane-loader').forEach(el => el.remove());
}

let _feedId     = null;
let _feedTitle  = 'All Articles';
let _offset     = 0;
let _unreadOnly = true;
let _sortAsc    = false;   // false = newest first (DESC), true = oldest first (ASC)
let _lastPageFull = false; // true if last fetch returned PAGE_SIZE results (more may exist)
let _total      = 0;
let _onSelect   = null;
let _onListChange = null;
let _lastArticles = [];

export function onArticleSelect(cb) { _onSelect = cb; }
export function onArticleListChange(cb) { _onListChange = cb; }
export function getArticles() { return _lastArticles; }
export function hasMoreArticles() { return _lastPageFull; }

export async function setFeed(feedId, feedTitle) {
  _feedId    = feedId;
  _feedTitle = feedTitle || 'All Articles';
  _offset    = 0;
  titleEl.textContent = _feedTitle;
  await loadArticles();
}

// Default unread-only on
unreadToggle.checked = true;

unreadToggle.addEventListener('change', async () => {
  _unreadOnly = unreadToggle.checked;
  _offset = 0;
  await loadArticles();
  if (_onListChange) _onListChange();
});

btnSortOrder.addEventListener('click', async () => {
  _sortAsc = !_sortAsc;
  btnSortOrder.textContent = _sortAsc ? '↑ Oldest' : '↓ Newest';
  _offset = 0;
  await loadArticles();
  if (_onListChange) _onListChange();
});



export async function loadArticles() {
  articleList.innerHTML = '<li class="loading-msg">Loading…</li>';
  showPaneLoader();
  try {
    const endpoint = _feedId
      ? `/api/feeds/${_feedId}/articles`
      : '/api/articles';
    const params = new URLSearchParams({
      limit:  PAGE_SIZE,
      offset: _offset,
      ..._unreadOnly ? { unread: 'true' } : {},
      ...(_sortAsc ? { sort: 'asc' } : {}),
    });
    const arts = await apiFetch(`${endpoint}?${params}`);
    _lastArticles = arts;
    _lastPageFull = arts.length >= PAGE_SIZE;
    renderArticles(arts);
  } catch (e) {
    articleList.innerHTML = `<li class="loading-msg" style="color:var(--error)">Error: ${e.message}</li>`;
  } finally {
    hidePaneLoader();
  }
}

function renderArticles(arts) {
  articleList.innerHTML = '';
  if (!arts.length) {
    articleList.innerHTML = '<li class="loading-msg">No articles.</li>';
    return;
  }
  arts.forEach(a => articleList.appendChild(makeArticleItem(a)));
}

function makeArticleItem(art) {
  const li = document.createElement('li');
  li.className = `article-item ${art.is_read ? 'read' : 'unread'}`;
  li.dataset.id = art.article_id;

  const titleEl = document.createElement('div');
  titleEl.className = 'article-item-title';
  titleEl.textContent = art.title || '(no title)';

  const meta = document.createElement('div');
  meta.className = 'article-item-meta';

  const date = art.published_at ? new Date(art.published_at) : null;
  meta.innerHTML = `
    <span>${date ? relativeTime(date) : ''}</span>
    <span class="article-item-star ${art.is_starred ? 'starred' : ''}">${art.is_starred ? '★' : '☆'}</span>
  `;

  li.appendChild(titleEl);
  li.appendChild(meta);
  li.addEventListener('click', () => {
    document.querySelectorAll('.article-item.active')
      .forEach(el => el.classList.remove('active'));
    li.classList.add('active');
    if (_onSelect) _onSelect(art.article_id, art);
  });
  return li;
}


export function markArticleReadInList(articleId) {
  const li = articleList.querySelector(`[data-id="${articleId}"]`);
  if (li) { li.classList.remove('unread'); li.classList.add('read'); }
  const art = _lastArticles.find(a => a.article_id === articleId);
  if (art) art.is_read = true;
}

export function markArticleStarredInList(articleId, starred) {
  const li = articleList.querySelector(`[data-id="${articleId}"]`);
  if (li) {
    const star = li.querySelector('.article-item-star');
    if (star) { star.textContent = starred ? '★' : '☆'; star.classList.toggle('starred', starred); }
  }
}

/** Fetch the next page of articles and append to list (for continuous scroll). */
export async function fetchMoreArticles() {
  if (!_lastPageFull) return [];
  _offset += PAGE_SIZE;
  try {
    const endpoint = _feedId
      ? `/api/feeds/${_feedId}/articles`
      : '/api/articles';
    const params = new URLSearchParams({
      limit:  PAGE_SIZE,
      offset: _offset,
      ..._unreadOnly ? { unread: 'true' } : {},
      ...(_sortAsc ? { sort: 'asc' } : {}),
    });
    const arts = await apiFetch(`${endpoint}?${params}`);
    _lastPageFull = arts.length >= PAGE_SIZE;
    _lastArticles = _lastArticles.concat(arts);
    // Append to DOM list too
    arts.forEach(a => articleList.appendChild(makeArticleItem(a)));
    return arts;
  } catch (e) {
    console.error('fetchMoreArticles failed', e);
    return [];
  }
}

/** Highlight a specific article in the list (scrolls it into view). */
export function highlightArticleInList(articleId) {
  document.querySelectorAll('.article-item.active')
    .forEach(el => el.classList.remove('active'));
  const li = articleList.querySelector(`[data-id="${articleId}"]`);
  if (li) {
    li.classList.add('active');
    li.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  }
}

/** Format date as relative string */
function relativeTime(date) {
  const s = Math.floor((Date.now() - date.getTime()) / 1000);
  if (s < 60)    return `${s}s ago`;
  if (s < 3600)  return `${Math.floor(s/60)}m ago`;
  if (s < 86400) return `${Math.floor(s/3600)}h ago`;
  if (s < 86400*30) return `${Math.floor(s/86400)}d ago`;
  return date.toLocaleDateString();
}
