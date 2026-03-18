/**
 * RSS-Lance - DB Status page module
 *
 * Fetches /api/status and renders table stats, bar charts, and article
 * breakdown in the middle + reader pane area.
 */
import { apiFetch } from './app.js';
import { hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { hideLogsPage, isLogsVisible } from './logs-page.js';
import { hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';
import { hideDuckHuntPage, isDuckHuntVisible } from './duck-hunt.js';

let _statusVisible = false;

/** Show the DB status page, replacing middle + reader panes. */
export async function showStatusPage() {
  // Dismiss any other active special page
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();
  if (isDuckHuntVisible()) hideDuckHuntPage();

  const app = document.getElementById('app');
  const listPane  = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  // Hide normal panes and dividers
  listPane.classList.add('hidden');
  readerPane.classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  // Create or reuse status container
  let container = document.getElementById('status-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'status-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  container.innerHTML = '<div class="status-loading">Loading database status…<div class="page-loader"></div></div>';

  _statusVisible = true;

  try {
    const data = await apiFetch('/api/status');
    if (!_statusVisible) return; // user navigated away
    renderStatusPage(container, data);
  } catch (e) {
    container.innerHTML = `<div class="status-error">Error loading status: ${e.message}</div>`;
  }
}

/** Hide the status page and restore normal panes. */
export function hideStatusPage() {
  _statusVisible = false;
  const container = document.getElementById('status-page');
  if (container) container.classList.add('hidden');

  const app = document.getElementById('app');
  const listPane  = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  listPane.classList.remove('hidden');
  readerPane.classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  // Restore grid columns if middle pane was hidden
  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    app.classList.add('hide-middle-pane');
  }
}

export function isStatusVisible() { return _statusVisible; }

// ── Rendering ─────────────────────────────────────────────────────────────────

function renderStatusPage(container, data) {
  const totalSize = data.tables.reduce((sum, t) => sum + t.size_bytes, 0);

  container.innerHTML = `
    <div class="status-inner">
      <h1 class="status-title">Database Status</h1>
      <p class="status-subtitle">${data.data_path}</p>

      <div class="status-cards">
        ${renderStatCard('Total Records', data.tables.reduce((s, t) => s + t.row_count, 0).toLocaleString(), '📊')}
        ${renderStatCard('Total Size', formatBytes(totalSize), '💾')}
        ${renderStatCard('Tables', data.tables.length, '🗄️')}
        ${renderStatCard('Articles', data.articles.total.toLocaleString(), '📰')}
      </div>

      <div class="status-grid">
        <div class="status-section">
          <h2>Article Breakdown</h2>
          <div class="status-article-stats">
            ${renderDonut(data.articles)}
            <div class="status-article-details">
              <div class="status-detail-row">
                <span class="dot dot-unread"></span>
                <span>Unread</span>
                <strong>${data.articles.unread.toLocaleString()}</strong>
              </div>
              <div class="status-detail-row">
                <span class="dot dot-read"></span>
                <span>Read</span>
                <strong>${(data.articles.total - data.articles.unread).toLocaleString()}</strong>
              </div>
              <div class="status-detail-row">
                <span class="dot dot-starred"></span>
                <span>Starred</span>
                <strong>${data.articles.starred.toLocaleString()}</strong>
              </div>
              <div class="status-detail-row date-row">
                <span>Oldest</span>
                <span>${formatDate(data.articles.oldest)}</span>
              </div>
              <div class="status-detail-row date-row">
                <span>Newest</span>
                <span>${formatDate(data.articles.newest)}</span>
              </div>
            </div>
          </div>
        </div>

        <div class="status-section">
          <h2>Table Sizes</h2>
          <div class="status-bar-chart">
            ${renderBarChart(data.tables)}
          </div>
        </div>
      </div>

      <div class="status-section">
        <h2>Table Details</h2>
        <table class="status-table">
          <thead>
            <tr><th>Table</th><th>Rows</th><th>Size</th><th>Version</th><th>Columns</th><th>Indexes</th></tr>
          </thead>
          <tbody>
            ${data.tables.map(t => `
              <tr>
                <td><code>${t.name}</code></td>
                <td class="num">${t.row_count.toLocaleString()}</td>
                <td class="num">${formatBytes(t.size_bytes)}</td>
                <td class="num">${t.version || '—'}</td>
                <td class="num">${t.num_columns || '—'}</td>
                <td class="num">${(t.indexes && t.indexes.length) || 0}</td>
              </tr>
            `).join('')}
            <tr class="total-row">
              <td><strong>Total</strong></td>
              <td class="num"><strong>${data.tables.reduce((s, t) => s + t.row_count, 0).toLocaleString()}</strong></td>
              <td class="num"><strong>${formatBytes(totalSize)}</strong></td>
              <td></td><td></td><td></td>
            </tr>
          </tbody>
        </table>
      </div>

      ${renderSchemaDetails(data.tables)}
    </div>
  `;
}

function renderStatCard(label, value, icon) {
  return `
    <div class="stat-card">
      <div class="stat-icon">${icon}</div>
      <div class="stat-value">${value}</div>
      <div class="stat-label">${label}</div>
    </div>
  `;
}

function renderDonut(articles) {
  const total = articles.total || 1;
  const readPct = ((total - articles.unread) / total) * 100;
  const unreadPct = (articles.unread / total) * 100;
  // SVG donut chart
  const r = 50, cx = 60, cy = 60, sw = 14;
  const circ = 2 * Math.PI * r;
  const readDash = (readPct / 100) * circ;
  const unreadDash = (unreadPct / 100) * circ;

  return `
    <svg class="donut-chart" viewBox="0 0 120 120" width="140" height="140">
      <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--bg3)" stroke-width="${sw}" />
      <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--accent)" stroke-width="${sw}"
        stroke-dasharray="${readDash} ${circ}" stroke-dashoffset="0"
        transform="rotate(-90 ${cx} ${cy})" />
      <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--warning)" stroke-width="${sw}"
        stroke-dasharray="${unreadDash} ${circ}" stroke-dashoffset="-${readDash}"
        transform="rotate(-90 ${cx} ${cy})" />
      <text x="${cx}" y="${cy}" text-anchor="middle" dominant-baseline="central"
        fill="var(--text)" font-size="16" font-weight="700">${total.toLocaleString()}</text>
    </svg>
  `;
}

function renderBarChart(tables) {
  const maxSize = Math.max(...tables.map(t => t.size_bytes), 1);
  return tables.map(t => {
    const pct = (t.size_bytes / maxSize) * 100;
    return `
      <div class="bar-row">
        <span class="bar-label">${t.name}</span>
        <div class="bar-track">
          <div class="bar-fill" style="width: ${Math.max(pct, 1)}%"></div>
        </div>
        <span class="bar-value">${formatBytes(t.size_bytes)}</span>
      </div>
    `;
  }).join('');
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const val = bytes / Math.pow(1024, i);
  return `${val < 10 ? val.toFixed(1) : Math.round(val)} ${units[i]}`;
}

function formatDate(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

function renderSchemaDetails(tables) {
  return tables.map(t => {
    const cols = (t.columns || []).map(c =>
      `<tr><td><code>${c.name}</code></td><td>${c.type}</td></tr>`
    ).join('');

    const idxRows = (t.indexes || []).map(idx =>
      `<tr><td><code>${idx.name}</code></td><td>${(idx.columns || []).join(', ')}</td><td>${idx.index_type}</td></tr>`
    ).join('');

    const idxSection = idxRows
      ? `<h3>Indexes</h3>
         <table class="status-table">
           <thead><tr><th>Name</th><th>Columns</th><th>Type</th></tr></thead>
           <tbody>${idxRows}</tbody>
         </table>`
      : '<p class="status-muted">No indexes</p>';

    return `
      <div class="status-section">
        <h2><code>${t.name}</code> — Schema (v${t.version || '?'})</h2>
        <div class="status-grid">
          <div>
            <h3>Columns (${t.num_columns || 0})</h3>
            <table class="status-table">
              <thead><tr><th>Name</th><th>Type</th></tr></thead>
              <tbody>${cols || '<tr><td colspan="2">—</td></tr>'}</tbody>
            </table>
          </div>
          <div>
            ${idxSection}
          </div>
        </div>
      </div>
    `;
  }).join('');
}
