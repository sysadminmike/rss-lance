/**
 * RSS-Lance - Logs page module
 *
 * Displays combined log entries from API and fetcher services.
 * Queries /api/logs with filters for service, level, and category.
 */
import { apiFetch } from './app.js';
import { hideStatusPage, isStatusVisible } from './status.js';
import { hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';

let _logsVisible = false;
let _autoRefreshTimer = null;

/** Show the logs page, replacing middle + reader panes. */
export async function showLogsPage() {
  // Dismiss any other active special page
  if (isStatusVisible()) hideStatusPage();
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();

  const app = document.getElementById('app');
  const listPane  = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  listPane.classList.add('hidden');
  readerPane.classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  let container = document.getElementById('logs-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'logs-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  container.innerHTML = '<div class="logs-loading">Loading logs...<div class="page-loader"></div></div>';

  _logsVisible = true;
  renderLogsPage(container);
}

/** Hide the logs page and restore normal panes. */
export function hideLogsPage() {
  _logsVisible = false;
  if (_autoRefreshTimer) {
    clearInterval(_autoRefreshTimer);
    _autoRefreshTimer = null;
  }
  const container = document.getElementById('logs-page');
  if (container) container.classList.add('hidden');

  document.getElementById('article-list-pane').classList.remove('hidden');
  document.getElementById('reader-pane').classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    document.getElementById('app').classList.add('hide-middle-pane');
  }
}

export function isLogsVisible() { return _logsVisible; }

// ---- State ----
let _currentService = '';
let _currentLevel = '';
let _currentLimit = 100;
let _currentOffset = 0;

// ---- Rendering ----

function renderLogsPage(container) {
  container.innerHTML = `
    <div class="logs-inner">
      <h1 class="logs-title">System Logs</h1>

      <div class="logs-filters">
        <div class="logs-filter-group">
          <label>Service</label>
          <select id="log-filter-service">
            <option value="">All Services</option>
            <option value="api">API Server</option>
            <option value="fetcher">Fetcher</option>
          </select>
        </div>
        <div class="logs-filter-group">
          <label>Level</label>
          <select id="log-filter-level">
            <option value="">All Levels</option>
            <option value="error">Error</option>
            <option value="warn">Warning</option>
            <option value="info">Info</option>
            <option value="debug">Debug</option>
          </select>
        </div>
        <div class="logs-filter-group">
          <label>Show</label>
          <select id="log-filter-limit">
            <option value="50">50 entries</option>
            <option value="100" selected>100 entries</option>
            <option value="250">250 entries</option>
            <option value="500">500 entries</option>
          </select>
        </div>
        <button id="btn-refresh-logs" class="logs-btn">Refresh</button>
        <label class="logs-auto-refresh">
          <input type="checkbox" id="log-auto-refresh">
          <span>Auto-refresh (30s)</span>
        </label>
      </div>

      <div id="logs-summary" class="logs-summary"></div>

      <div id="logs-table-container" class="logs-table-container">
        <table class="logs-table">
          <thead>
            <tr>
              <th class="col-time">Time</th>
              <th class="col-service">Service</th>
              <th class="col-level">Level</th>
              <th class="col-category">Category</th>
              <th class="col-message">Message</th>
            </tr>
          </thead>
          <tbody id="logs-tbody"></tbody>
        </table>
      </div>

      <div id="logs-pagination" class="logs-pagination"></div>
    </div>
  `;

  // Wire up filter events
  const serviceSelect = document.getElementById('log-filter-service');
  const levelSelect = document.getElementById('log-filter-level');
  const limitSelect = document.getElementById('log-filter-limit');
  const btnRefresh = document.getElementById('btn-refresh-logs');
  const autoRefreshCb = document.getElementById('log-auto-refresh');

  serviceSelect.addEventListener('change', () => {
    _currentService = serviceSelect.value;
    _currentOffset = 0;
    fetchAndRenderLogs();
  });
  levelSelect.addEventListener('change', () => {
    _currentLevel = levelSelect.value;
    _currentOffset = 0;
    fetchAndRenderLogs();
  });
  limitSelect.addEventListener('change', () => {
    _currentLimit = parseInt(limitSelect.value, 10);
    _currentOffset = 0;
    fetchAndRenderLogs();
  });
  btnRefresh.addEventListener('click', fetchAndRenderLogs);

  autoRefreshCb.addEventListener('change', () => {
    if (autoRefreshCb.checked) {
      _autoRefreshTimer = setInterval(fetchAndRenderLogs, 30000);
    } else if (_autoRefreshTimer) {
      clearInterval(_autoRefreshTimer);
      _autoRefreshTimer = null;
    }
  });

  fetchAndRenderLogs();
}

async function fetchAndRenderLogs() {
  if (!_logsVisible) return;

  const tbody = document.getElementById('logs-tbody');
  const summary = document.getElementById('logs-summary');
  const pagination = document.getElementById('logs-pagination');
  if (!tbody) return;

  let params = `?limit=${_currentLimit}&offset=${_currentOffset}`;
  if (_currentService) params += `&service=${encodeURIComponent(_currentService)}`;
  if (_currentLevel) params += `&level=${encodeURIComponent(_currentLevel)}`;

  try {
    const data = await apiFetch('/api/logs' + params);
    const entries = data.entries || [];
    const total = data.total || 0;

    summary.textContent = `${total} total entries`;

    if (entries.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5" class="logs-empty">No log entries found</td></tr>';
      pagination.innerHTML = '';
      return;
    }

    tbody.innerHTML = entries.map(e => {
      const ts = formatLogTime(e.timestamp);
      const levelClass = 'log-level-' + (e.level || 'info');
      const detailsAttr = e.details ? ` data-details="${escapeAttr(e.details)}"` : '';
      return `
        <tr class="${levelClass}"${detailsAttr}>
          <td class="col-time">${ts}</td>
          <td class="col-service"><span class="log-service-badge service-${e.service}">${e.service}</span></td>
          <td class="col-level"><span class="log-level-badge ${levelClass}">${e.level}</span></td>
          <td class="col-category">${escapeHTML(formatCategory(e.category))}</td>
          <td class="col-message">${escapeHTML(e.message)}</td>
        </tr>
      `;
    }).join('');

    // Click row to show details
    tbody.querySelectorAll('tr[data-details]').forEach(row => {
      row.style.cursor = 'pointer';
      row.addEventListener('click', () => {
        let detailRow = row.nextElementSibling;
        if (detailRow && detailRow.classList.contains('log-detail-row')) {
          detailRow.remove();
          return;
        }
        detailRow = document.createElement('tr');
        detailRow.className = 'log-detail-row';
        const td = document.createElement('td');
        td.colSpan = 5;
        try {
          const parsed = JSON.parse(row.dataset.details);
          td.innerHTML = `<pre class="log-detail-pre">${escapeHTML(JSON.stringify(parsed, null, 2))}</pre>`;
        } catch {
          td.innerHTML = `<pre class="log-detail-pre">${escapeHTML(row.dataset.details)}</pre>`;
        }
        detailRow.appendChild(td);
        row.after(detailRow);
      });
    });

    // Pagination
    const totalPages = Math.ceil(total / _currentLimit);
    const currentPage = Math.floor(_currentOffset / _currentLimit) + 1;
    if (totalPages > 1) {
      let paginationHTML = '';
      if (currentPage > 1) {
        paginationHTML += `<button class="logs-btn" data-page="${currentPage - 1}">Prev</button>`;
      }
      paginationHTML += `<span class="logs-page-info">Page ${currentPage} of ${totalPages}</span>`;
      if (currentPage < totalPages) {
        paginationHTML += `<button class="logs-btn" data-page="${currentPage + 1}">Next</button>`;
      }
      pagination.innerHTML = paginationHTML;
      pagination.querySelectorAll('button[data-page]').forEach(btn => {
        btn.addEventListener('click', () => {
          _currentOffset = (parseInt(btn.dataset.page, 10) - 1) * _currentLimit;
          fetchAndRenderLogs();
        });
      });
    } else {
      pagination.innerHTML = '';
    }
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="5" class="logs-error">Error loading logs: ${escapeHTML(e.message)}</td></tr>`;
  }
}

function formatLogTime(ts) {
  if (!ts) return '-';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return ts;
  const pad = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function formatCategory(cat) {
  if (!cat) return '-';
  return cat.replace(/_/g, ' ');
}

function escapeHTML(str) {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}

function escapeAttr(str) {
  return str.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
