/**
 * RSS-Lance - Main app entry point
 *
 * Wires together feeds, articles and reader modules.
 * Also provides the shared `apiFetch` helper.
 */
import { loadFeeds, onFeedSelect, getSelectedFeedId, markFeedAllRead, selectFeed } from './feeds.js';
import { setFeed, onArticleSelect, onArticleListChange, loadArticles, getArticles, fetchMoreArticles } from './articles.js';
import { showArticle, resetReader } from './reader.js';
import { showStatusPage, hideStatusPage, isStatusVisible } from './status.js';
import { showSettingsPage, hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { showLogsPage, hideLogsPage, isLogsVisible } from './logs-page.js';
import { showTableViewerPage, hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { showServerStatusPage, hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';
import { showDuckHuntPage, hideDuckHuntPage, isDuckHuntVisible, playQuack } from './duck-hunt.js';

// ── Restore saved preferences immediately ─────────────────────────────────────
if (localStorage.getItem('rss-lance-theme') === 'light') {
  document.body.classList.add('light-theme');
}
if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
  document.getElementById('app').classList.add('hide-middle-pane');
}
// Restore saved pane widths
const _savedSidebarW = localStorage.getItem('rss-lance-sidebar-w');
const _savedListW    = localStorage.getItem('rss-lance-list-w');
if (_savedSidebarW) document.documentElement.style.setProperty('--sidebar-w', _savedSidebarW + 'px');
if (_savedListW)    document.documentElement.style.setProperty('--list-w',    _savedListW + 'px');

// ── Build revision tracking ───────────────────────────────────────────────────

let _knownRevision = null;

function checkBuildRevision(res) {
  const rev = res.headers.get('X-Build-Revision');
  if (!rev) return;
  if (_knownRevision === null) {
    _knownRevision = rev;
    return;
  }
  if (rev !== _knownRevision) {
    showReloadBanner();
  }
}

function showReloadBanner() {
  if (document.getElementById('reload-banner')) return;
  const banner = document.createElement('div');
  banner.id = 'reload-banner';
  banner.className = 'reload-banner';
  banner.innerHTML = 'Server has been updated — <a href="#" id="reload-banner-link">reload page</a>';
  document.body.prepend(banner);
  document.getElementById('reload-banner-link').addEventListener('click', (e) => {
    e.preventDefault();
    location.reload();
  });
}

// ── Shared API fetch ──────────────────────────────────────────────────────────

export async function apiFetch(url, options = {}) {
  const res = await fetch(url, {
    headers: { 'Content-Type': 'application/json', ...options.headers },
    ...options,
  });
  checkBuildRevision(res);
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`${res.status} ${res.statusText}: ${body}`);
  }
  return res.json();
}

// ── Wire up callbacks ─────────────────────────────────────────────────────────

onFeedSelect(async (feedId) => {
  if (isStatusVisible()) hideStatusPage();
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();
  if (isDuckHuntVisible()) hideDuckHuntPage();
  resetReader();  // clear continuous-scroll stream on feed change
  const feeds = window.__rlFeeds || [];
  const feed = feeds.find(f => f.feed_id === feedId);
  await setFeed(feedId, feed?.title || feed?.url || 'All Articles');
  // Auto-open the first article
  const arts = getArticles();
  if (arts.length) await showArticle(arts[0].article_id);
});

onArticleSelect(async (articleId, previewArt) => {
  await showArticle(articleId, previewArt);
});

onArticleListChange(async () => {
  resetReader();
  const arts = getArticles();
  if (arts.length) await showArticle(arts[0].article_id);
});

// ── Add-feed modal ────────────────────────────────────────────────────────────

const btnAddFeed    = document.getElementById('btn-add-feed');
const modalOverlay  = document.getElementById('modal-overlay');
const btnCancel     = document.getElementById('btn-modal-cancel');
const btnModalAdd   = document.getElementById('btn-modal-add');
const inputFeedUrl  = document.getElementById('input-feed-url');
const modalStatus   = document.getElementById('modal-status');

btnAddFeed.addEventListener('click', () => {
  modalOverlay.classList.remove('hidden');
  inputFeedUrl.value = '';
  modalStatus.textContent = '';
  modalStatus.className = '';
  inputFeedUrl.focus();
});

btnCancel.addEventListener('click', hideModal);
modalOverlay.addEventListener('click', e => { if (e.target === modalOverlay) hideModal(); });

function hideModal() { modalOverlay.classList.add('hidden'); }

btnModalAdd.addEventListener('click', async () => {
  const url = inputFeedUrl.value.trim();
  if (!url) { modalStatus.textContent = 'Please enter a URL.'; modalStatus.className = 'error'; return; }

  btnModalAdd.disabled = true;
  modalStatus.textContent = 'Queuing…';
  modalStatus.className = '';

  try {
    await apiFetch('/api/feeds', {
      method: 'POST',
      body: JSON.stringify({ url }),
    });
    modalStatus.textContent = 'Feed queued! It will appear after the next fetch.';
    modalStatus.className = 'success';
    setTimeout(() => { hideModal(); loadFeeds(); }, 1500);
  } catch (e) {
    modalStatus.textContent = `Error: ${e.message}`;
    modalStatus.className = 'error';
  } finally {
    btnModalAdd.disabled = false;
  }
});

// ── Mark all read ─────────────────────────────────────────────────────────────

document.getElementById('btn-mark-all-read').addEventListener('click', async () => {
  const feedId = getSelectedFeedId();
  if (!feedId) return;
  try {
    await apiFetch(`/api/feeds/${feedId}/mark-all-read`, { method: 'POST' });
    markFeedAllRead(feedId);
    loadArticles(); // Refresh list
  } catch (e) {
    console.error('mark-all-read failed', e);
  }
});

// ── Keyboard shortcuts ────────────────────────────────────────────────────────
// j = next, k = prev, r = reload feeds

document.addEventListener('keydown', e => {
  if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;

  if (e.key === 'r' && !e.ctrlKey && !e.metaKey) {
    loadFeeds();
  }
});

// ── Periodic feed-list refresh ────────────────────────────────────────────────

const FEED_POLL_INTERVAL = 60_000; // 60 seconds

function startFeedPolling() {
  setInterval(async () => {
    try { await window.__rlLoadFeeds(); } catch (_) { /* silent */ }
  }, FEED_POLL_INTERVAL);
}

// ── Offline status polling ────────────────────────────────────────────────────

const OFFLINE_POLL_INTERVAL = 30_000; // 30 seconds
let _prevOffline = false;

function getOrCreateOfflineBanner() {
  let banner = document.getElementById('offline-banner');
  if (!banner) {
    banner = document.createElement('div');
    banner.id = 'offline-banner';
    banner.className = 'offline-banner hidden';
    document.body.prepend(banner);
  }
  return banner;
}

function getOrCreateOfflineDot() {
  let dot = document.getElementById('offline-dot');
  if (!dot) {
    dot = document.createElement('div');
    dot.id = 'offline-dot';
    document.body.appendChild(dot);
  }
  return dot;
}

function startOfflinePolling() {
  // Initialise dot as online immediately
  getOrCreateOfflineDot();

  setInterval(async () => {
    try {
      const res = await fetch('/api/offline-status');
      if (!res.ok) return;
      const st = await res.json();
      const banner = getOrCreateOfflineBanner();
      const dot = getOrCreateOfflineDot();

      if (st.offline) {
        const pending = st.pending_changes || 0;
        banner.textContent = 'Working offline -- ' + pending + ' change' + (pending !== 1 ? 's' : '') + ' pending';
        banner.classList.remove('hidden', 'offline-banner-online');
        banner.classList.add('offline-banner-offline');
        dot.classList.add('offline-dot-offline');
        _prevOffline = true;
      } else if (_prevOffline) {
        // Just came back online
        banner.textContent = 'Back online -- changes synced';
        banner.classList.remove('hidden', 'offline-banner-offline');
        banner.classList.add('offline-banner-online');
        dot.classList.remove('offline-dot-offline');
        _prevOffline = false;
        setTimeout(() => banner.classList.add('hidden'), 5000);
      } else {
        banner.classList.add('hidden');
        dot.classList.remove('offline-dot-offline');
      }
    } catch (_) { /* server unreachable */ }
  }, OFFLINE_POLL_INTERVAL);
}

// ── Settings panel ────────────────────────────────────────────────────────────

function initSettings() {
  const sidebar = document.getElementById('sidebar');

  // ── Other section (DB Status, etc.) ──
  const otherSection = document.createElement('div');
  otherSection.id = 'other-section';

  const otherHeader = document.createElement('div');
  otherHeader.className = 'feed-section-header';

  const otherArrow = document.createElement('span');
  otherArrow.className = 'feed-section-arrow';
  otherArrow.textContent = '\u25B6';

  const otherLabel = document.createElement('span');
  otherLabel.textContent = 'Other';

  otherHeader.appendChild(otherArrow);
  otherHeader.appendChild(otherLabel);

  const otherItems = document.createElement('div');
  otherItems.className = 'other-items';
  otherItems.style.display = 'none';

  // DB Status link
  const statusItem = document.createElement('div');
  statusItem.className = 'feed-item other-item';
  const statusIcon = document.createElement('div');
  statusIcon.className = 'feed-icon-placeholder';
  statusIcon.textContent = '📊';
  const statusName = document.createElement('span');
  statusName.className = 'feed-name';
  statusName.textContent = 'DB Status';
  statusItem.appendChild(statusIcon);
  statusItem.appendChild(statusName);
  statusItem.addEventListener('click', () => {
    showStatusPage();
  });
  otherItems.appendChild(statusItem);

  // Table viewer link
  const tableViewerItem = document.createElement('div');
  tableViewerItem.className = 'feed-item other-item';
  const tableViewerIcon = document.createElement('div');
  tableViewerIcon.className = 'feed-icon-placeholder';
  tableViewerIcon.textContent = '\uD83D\uDDC3';
  const tableViewerName = document.createElement('span');
  tableViewerName.className = 'feed-name';
  tableViewerName.textContent = 'DB Tables';
  tableViewerItem.appendChild(tableViewerIcon);
  tableViewerItem.appendChild(tableViewerName);
  tableViewerItem.addEventListener('click', () => {
    showTableViewerPage();
  });
  otherItems.appendChild(tableViewerItem);

  // Server Status link
  const serverStatusItem = document.createElement('div');
  serverStatusItem.className = 'feed-item other-item';
  const serverStatusIcon = document.createElement('div');
  serverStatusIcon.className = 'feed-icon-placeholder';
  serverStatusIcon.textContent = '\u23F1';
  const serverStatusName = document.createElement('span');
  serverStatusName.className = 'feed-name';
  serverStatusName.textContent = 'Server Status';
  serverStatusItem.appendChild(serverStatusIcon);
  serverStatusItem.appendChild(serverStatusName);
  serverStatusItem.addEventListener('click', () => {
    showServerStatusPage();
  });
  otherItems.appendChild(serverStatusItem);

  // Settings page link
  const settingsPageItem = document.createElement('div');
  settingsPageItem.className = 'feed-item other-item';
  const settingsPageIcon = document.createElement('div');
  settingsPageIcon.className = 'feed-icon-placeholder';
  settingsPageIcon.textContent = '\u2699';
  const settingsPageName = document.createElement('span');
  settingsPageName.className = 'feed-name';
  settingsPageName.textContent = 'Settings';
  settingsPageItem.appendChild(settingsPageIcon);
  settingsPageItem.appendChild(settingsPageName);
  settingsPageItem.addEventListener('click', () => {
    showSettingsPage();
  });
  otherItems.appendChild(settingsPageItem);

  // Logs page link
  const logsItem = document.createElement('div');
  logsItem.className = 'feed-item other-item';
  const logsIcon = document.createElement('div');
  logsIcon.className = 'feed-icon-placeholder';
  logsIcon.textContent = '\uD83D\uDCCB';
  const logsName = document.createElement('span');
  logsName.className = 'feed-name';
  logsName.textContent = 'System Logs';
  logsItem.appendChild(logsIcon);
  logsItem.appendChild(logsName);
  logsItem.addEventListener('click', () => {
    showLogsPage();
  });
  otherItems.appendChild(logsItem);

  // Shoot the Duck easter egg - emoji only, appears for 10s at random intervals (2-5 min)
  const duckHuntItem = document.createElement('div');
  duckHuntItem.className = 'feed-item other-item duck-hunt-trigger';
  duckHuntItem.style.display = 'none';
  duckHuntItem.textContent = '\uD83E\uDD86';
  duckHuntItem.title = 'Shoot the Duck';
  duckHuntItem.addEventListener('click', () => {
    showDuckHuntPage();
  });
  otherItems.appendChild(duckHuntItem);

  // Show the duck emoji for 10s, then hide, repeat every 2-5 min
  let _duckAppearCount = 0;
  function scheduleDuckAppearance() {
    const delayMs = (Math.random() * 3 + 2) * 60 * 1000; // 2-5 minutes
    setTimeout(() => {
      duckHuntItem.style.display = '';
      _duckAppearCount++;
      if (_duckAppearCount % 10 === 0) {
        setTimeout(() => { playQuack(800, 0.18); }, 3000);
      }
      setTimeout(() => {
        duckHuntItem.style.display = 'none';
        scheduleDuckAppearance();
      }, 10000);
    }, delayMs);
  }
  scheduleDuckAppearance();

  // Stop Server button (conditionally shown based on server config)
  const shutdownItem = document.createElement('div');
  shutdownItem.className = 'feed-item other-item';
  shutdownItem.id = 'btn-stop-server';
  shutdownItem.style.display = 'none'; // hidden until config confirms it
  const shutdownIcon = document.createElement('div');
  shutdownIcon.className = 'feed-icon-placeholder';
  shutdownIcon.textContent = '\u23FB';
  const shutdownName = document.createElement('span');
  shutdownName.className = 'feed-name';
  shutdownName.textContent = 'Stop Server';
  shutdownItem.appendChild(shutdownIcon);
  shutdownItem.appendChild(shutdownName);
  shutdownItem.addEventListener('click', async () => {
    if (!confirm('Stop the RSS-Lance server?')) return;
    try {
      await apiFetch('/api/shutdown', { method: 'POST' });
      shutdownName.textContent = 'Server stopping\u2026';
    } catch (_) {
      shutdownName.textContent = 'Server stopped';
    }
  });
  otherItems.appendChild(shutdownItem);

  // Fetch config to decide whether to show the shutdown button
  fetch('/api/config').then(r => r.json()).then(cfg => {
    if (cfg.show_shutdown) shutdownItem.style.display = '';
  }).catch(() => {});

  otherHeader.addEventListener('click', () => {
    const isHidden = otherItems.style.display === 'none';
    otherItems.style.display = isHidden ? '' : 'none';
    otherHeader.classList.toggle('expanded', isHidden);
  });

  otherSection.appendChild(otherHeader);
  otherSection.appendChild(otherItems);
  document.getElementById('feed-list').appendChild(otherSection);
}

// ── Pane resize handles ──────────────────────────────────────────────────────

function initPaneResize() {
  const app = document.getElementById('app');
  const MIN_PANE = 120; // px minimum for any pane

  function setupDivider(dividerId, cssVar, storageKey) {
    const divider = document.getElementById(dividerId);
    if (!divider) return;

    // Restore saved width
    const saved = localStorage.getItem(storageKey);
    if (saved) document.documentElement.style.setProperty(cssVar, saved + 'px');

    let startX, startWidth;

    function onPointerDown(e) {
      e.preventDefault();
      startX = e.clientX;
      // Read current computed width of the pane to the left of this divider
      const prev = divider.previousElementSibling;
      startWidth = prev.getBoundingClientRect().width;

      divider.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';

      document.addEventListener('pointermove', onPointerMove);
      document.addEventListener('pointerup', onPointerUp);
    }

    function onPointerMove(e) {
      const delta = e.clientX - startX;
      const newWidth = Math.max(MIN_PANE, startWidth + delta);
      document.documentElement.style.setProperty(cssVar, newWidth + 'px');
    }

    function onPointerUp() {
      divider.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';

      // Persist
      const prev = divider.previousElementSibling;
      const w = Math.round(prev.getBoundingClientRect().width);
      localStorage.setItem(storageKey, w);

      document.removeEventListener('pointermove', onPointerMove);
      document.removeEventListener('pointerup', onPointerUp);
    }

    divider.addEventListener('pointerdown', onPointerDown);
  }

  setupDivider('divider-sidebar', '--sidebar-w', 'rss-lance-sidebar-w');
  setupDivider('divider-list',    '--list-w',    'rss-lance-list-w');
}

// ── Boot ─────────────────────────────────────────────────────────────────────

(async () => {
  initSettings();
  initPaneResize();

  // Cache feeds for title lookup
  const origLoadFeeds = loadFeeds;
  window.__rlLoadFeeds = async () => {
    const res = await fetch('/api/feeds');
    checkBuildRevision(res);
    if (res.ok) window.__rlFeeds = await res.json();
    await origLoadFeeds();
  };
  await window.__rlLoadFeeds();

  // Sync appearance settings from server → localStorage cache
  try {
    const s = await apiFetch('/api/settings');
    const theme = s['ui.theme'] ?? 'dark';
    localStorage.setItem('rss-lance-theme', theme);
    document.body.classList.toggle('light-theme', theme === 'light');

    const showList = s['ui.show_article_list'] !== false;
    localStorage.setItem('rss-lance-middle-pane', showList ? 'visible' : 'hidden');
    document.getElementById('app').classList.toggle('hide-middle-pane', !showList);

    const autoRead = s['ui.auto_read'] !== false;
    localStorage.setItem('rss-lance-auto-read', autoRead ? 'on' : 'off');
  } catch (_) {}

  // Pre-select "All Articles" on boot — this triggers onFeedSelect which
  // calls setFeed + showArticle, so no need to duplicate that here.
  selectFeed(null);

  startFeedPolling();
  startOfflinePolling();
})();
