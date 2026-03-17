/**
 * RSS-Lance - Feed list module
 */
import { apiFetch } from './app.js';

const feedList = document.getElementById('feed-list');

let _feeds = [];
let _selectedFeedId = null;
let _onSelect = null;
let _collapsedSections = { active: false, older: true };

export function onFeedSelect(cb) { _onSelect = cb; }
export function getSelectedFeedId() { return _selectedFeedId; }

export async function loadFeeds() {
  feedList.innerHTML = '<div class="loading-msg">Loading…</div>';
  try {
    _feeds = await apiFetch('/api/feeds');
    renderFeeds();
  } catch (e) {
    feedList.innerHTML = `<div class="loading-msg" style="color:var(--error)">Error: ${e.message}</div>`;
  }
}

export function updateUnreadCount(feedId, delta) {
  const feed = _feeds.find(f => f.feed_id === feedId);
  if (feed) {
    feed.unread_count = Math.max(0, (feed.unread_count || 0) + delta);
    renderFeeds();
  }
}

export function markFeedAllRead(feedId) {
  const feed = _feeds.find(f => f.feed_id === feedId);
  if (feed) { feed.unread_count = 0; renderFeeds(); }
}

function renderFeeds() {
  const active  = _feeds.filter(f => isActiveFeed(f));
  const stale   = _feeds.filter(f => !isActiveFeed(f));

  feedList.innerHTML = '';

  // All-articles shortcut
  feedList.appendChild(makeFeedItem(null, '📰 All Articles', null, 0));

  if (active.length) {
    feedList.appendChild(makeSection('active', 'Active Feeds', active));
  }

  if (stale.length) {
    feedList.appendChild(makeSection('older', 'Older Feeds', stale));
  }
}

function isActiveFeed(f) {
  if (!f.last_article_date) return false;
  const ms = Date.parse(f.last_article_date);
  if (isNaN(ms)) return false;
  const threeMo = 90 * 24 * 60 * 60 * 1000;
  return (Date.now() - ms) < threeMo;
}

function makeSection(key, label, feeds) {
  const section = document.createElement('div');
  section.className = 'feed-section';

  const header = document.createElement('div');
  header.className = 'feed-section-header' + (_collapsedSections[key] ? '' : ' expanded');

  const arrow = document.createElement('span');
  arrow.className = 'feed-section-arrow';
  arrow.textContent = '\u25B6';

  const labelEl = document.createElement('span');
  labelEl.textContent = label;

  header.appendChild(arrow);
  header.appendChild(labelEl);

  const items = document.createElement('div');
  items.className = 'feed-section-items';
  if (_collapsedSections[key]) items.style.display = 'none';

  feeds.forEach(f => items.appendChild(makeFeedItem(f.feed_id, f.title || f.url, f.icon_url, f.unread_count, f.error_count, f.last_error)));

  header.addEventListener('click', () => {
    _collapsedSections[key] = !_collapsedSections[key];
    header.classList.toggle('expanded');
    items.style.display = _collapsedSections[key] ? 'none' : '';
  });

  section.appendChild(header);
  section.appendChild(items);
  return section;
}

function makeFeedItem(feedId, title, iconUrl, unreadCount, errorCount, lastError) {
  const el = document.createElement('div');
  el.className = 'feed-item' + (unreadCount > 0 ? ' has-unread' : '') + (feedId === _selectedFeedId ? ' active' : '');
  el.dataset.feedId = feedId || '';

  // Icon
  const iconEl = iconUrl
    ? Object.assign(document.createElement('img'), { src: iconUrl, className: 'feed-icon', alt: '' })
    : Object.assign(document.createElement('div'), { className: 'feed-icon-placeholder', textContent: '⬡' });
  el.appendChild(iconEl);

  // Name
  const nameEl = document.createElement('span');
  nameEl.className = 'feed-name';
  nameEl.textContent = title;
  el.appendChild(nameEl);

  // Unread badge
  if (unreadCount > 0) {
    const badge = document.createElement('span');
    badge.className = 'feed-unread-count';
    badge.textContent = unreadCount > 999 ? '999+' : String(unreadCount);
    el.appendChild(badge);
  }

  // Error dot
  if (errorCount > 0) {
    const dot = document.createElement('span');
    dot.className = 'feed-error-dot ' + (lastError?.includes('410') ? 'red' : 'orange');
    dot.title = lastError || 'Fetch errors';
    el.appendChild(dot);
  }

  el.addEventListener('click', () => selectFeed(feedId));
  return el;
}

export function selectFeed(feedId) {
  _selectedFeedId = feedId;
  renderFeeds();
  if (_onSelect) _onSelect(feedId);
}
