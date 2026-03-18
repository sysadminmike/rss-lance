/**
 * RSS-Lance - DB Table Viewer page module
 *
 * Displays raw Lance table data with pagination.
 * Queries /api/tables/{name} with limit/offset.
 */
import { apiFetch } from './app.js';
import { hideStatusPage, isStatusVisible } from './status.js';
import { hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { hideLogsPage, isLogsVisible } from './logs-page.js';
import { hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';
import { hideDuckHuntPage, isDuckHuntVisible } from './duck-hunt.js';

let _tableViewerVisible = false;
let _currentTable = '';
let _pageSize = 200;
let _currentOffset = 0;

/** Show the table viewer page, replacing middle + reader panes. */
export async function showTableViewerPage() {
  // Dismiss any other active special page
  if (isStatusVisible()) hideStatusPage();
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isLogsVisible()) hideLogsPage();
  if (isServerStatusVisible()) hideServerStatusPage();
  if (isDuckHuntVisible()) hideDuckHuntPage();

  const app = document.getElementById('app');
  const listPane = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  listPane.classList.add('hidden');
  readerPane.classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  let container = document.getElementById('table-viewer-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'table-viewer-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  container.innerHTML = '<div class="tv-loading">Loading…<div class="page-loader"></div></div>';

  _tableViewerVisible = true;

  // Fetch page size from settings (stored in DB, editable via Advanced Settings)
  try {
    const settings = await apiFetch('/api/settings');
    const ps = settings['server.table_page_size'];
    if (ps && Number(ps) > 0) {
      _pageSize = Number(ps);
    }
  } catch (_) {}

  renderTableViewer(container);
}

/** Hide the table viewer page and restore normal panes. */
export function hideTableViewerPage() {
  _tableViewerVisible = false;
  const container = document.getElementById('table-viewer-page');
  if (container) container.classList.add('hidden');

  document.getElementById('article-list-pane').classList.remove('hidden');
  document.getElementById('reader-pane').classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    document.getElementById('app').classList.add('hide-middle-pane');
  }
}

export function isTableViewerVisible() { return _tableViewerVisible; }

// ---- Rendering ----

function renderTableViewer(container) {
  container.innerHTML = `
    <div class="tv-inner">
      <h1 class="tv-title">DB Table Viewer</h1>

      <div class="tv-controls">
        <div class="tv-filter-group">
          <label>Table</label>
          <select id="tv-table-select">
            <option value="">— Select a table —</option>
            <option value="articles">articles</option>
            <option value="feeds">feeds</option>
            <option value="categories">categories</option>
            <option value="pending_feeds">pending_feeds</option>
            <option value="settings">settings</option>
            <option value="log_api">log_api</option>
            <option value="log_fetcher">log_fetcher</option>
          </select>
        </div>
        <button id="tv-btn-refresh" class="tv-btn">Refresh</button>
      </div>

      <div id="tv-summary" class="tv-summary"></div>

      <div id="tv-table-container" class="tv-table-container">
        <div class="tv-placeholder">Select a table above to view its data.</div>
      </div>

      <div id="tv-pagination" class="tv-pagination"></div>
    </div>
  `;

  const tableSelect = document.getElementById('tv-table-select');
  const btnRefresh = document.getElementById('tv-btn-refresh');

  // Restore last-viewed table
  if (_currentTable) {
    tableSelect.value = _currentTable;
  }

  tableSelect.addEventListener('change', () => {
    _currentTable = tableSelect.value;
    _currentOffset = 0;
    if (_currentTable) fetchAndRenderTable();
  });

  btnRefresh.addEventListener('click', () => {
    if (_currentTable) fetchAndRenderTable();
  });

  if (_currentTable) fetchAndRenderTable();
}

async function fetchAndRenderTable() {
  if (!_tableViewerVisible || !_currentTable) return;

  const tableContainer = document.getElementById('tv-table-container');
  const summary = document.getElementById('tv-summary');
  const pagination = document.getElementById('tv-pagination');
  if (!tableContainer) return;

  tableContainer.innerHTML = '<div class="tv-loading">Loading…</div>';

  try {
    const data = await apiFetch(
      `/api/tables/${encodeURIComponent(_currentTable)}?limit=${_pageSize}&offset=${_currentOffset}`
    );

    const rows = data.rows || [];
    const total = data.total || 0;
    const columns = data.columns || [];

    // Sort columns for consistent order
    columns.sort();

    const rangeStart = _currentOffset + 1;
    const rangeEnd = Math.min(_currentOffset + rows.length, total);
    summary.textContent = rows.length
      ? `Showing ${rangeStart}–${rangeEnd} of ${total} rows`
      : `${total} rows total`;

    if (rows.length === 0) {
      tableContainer.innerHTML = '<div class="tv-placeholder">Table is empty.</div>';
      pagination.innerHTML = '';
      return;
    }

    // Build table HTML
    const thHTML = columns.map(c => `<th>${escapeHTML(c)}</th>`).join('');
    const tbodyHTML = rows.map(row => {
      const cells = columns.map(c => {
        const val = row[c];
        const display = val == null ? '' : String(val);
        return `<td title="${escapeAttr(display)}">${escapeHTML(truncateCell(display, 120))}</td>`;
      }).join('');
      return `<tr>${cells}</tr>`;
    }).join('');

    tableContainer.innerHTML = `
      <table class="tv-table">
        <thead><tr>${thHTML}</tr></thead>
        <tbody>${tbodyHTML}</tbody>
      </table>
    `;

    // Pagination
    const totalPages = Math.ceil(total / _pageSize);
    const currentPage = Math.floor(_currentOffset / _pageSize) + 1;
    if (totalPages > 1) {
      let html = '';
      if (currentPage > 1) {
        html += `<button class="tv-btn" data-page="${currentPage - 1}">← Prev</button>`;
      }
      html += `<span class="tv-page-info">Page ${currentPage} of ${totalPages}</span>`;
      if (currentPage < totalPages) {
        html += `<button class="tv-btn" data-page="${currentPage + 1}">Next →</button>`;
      }
      pagination.innerHTML = html;
      pagination.querySelectorAll('button[data-page]').forEach(btn => {
        btn.addEventListener('click', () => {
          _currentOffset = (parseInt(btn.dataset.page, 10) - 1) * _pageSize;
          fetchAndRenderTable();
        });
      });
    } else {
      pagination.innerHTML = '';
    }
  } catch (e) {
    tableContainer.innerHTML = `<div class="tv-error">Error: ${escapeHTML(e.message)}</div>`;
  }
}

function truncateCell(str, max) {
  if (str.length <= max) return str;
  return str.slice(0, max) + '…';
}

function escapeHTML(str) {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}

function escapeAttr(str) {
  return str.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
