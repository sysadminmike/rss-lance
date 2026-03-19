/**
 * RSS-Lance - Server Runtime Status page module
 *
 * Fetches /api/server-status and renders live server stats with
 * stat cards, mini-charts (memory, GC pauses, goroutines), and
 * server info. Auto-refreshes every 5 seconds.
 */
import { apiFetch } from './app.js';
import { hideStatusPage, isStatusVisible } from './status.js';
import { hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { hideLogsPage, isLogsVisible } from './logs-page.js';
import { hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { hideDuckHuntPage, isDuckHuntVisible } from './duck-hunt.js';

let _visible = false;
let _refreshTimer = null;
let _refreshIntervalMs = 5000;

// Rolling data buffers for charts (seeded from server history, appended live)
let _heapAllocHistory = [];
let _heapSysHistory = [];
let _stackHistory = [];
let _goroutineHistory = [];
let _gcPauseHistory = [];
let _timestamps = [];

export async function showServerStatusPage() {
  if (isStatusVisible()) hideStatusPage();
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isDuckHuntVisible()) hideDuckHuntPage();

  const app = document.getElementById('app');
  const listPane = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  listPane.classList.add('hidden');
  readerPane.classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  let container = document.getElementById('server-status-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'server-status-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  container.innerHTML = '<div class="status-loading">Loading server status...<div class="page-loader"></div></div>';

  _visible = true;

  // Clear history then seed from server-side history
  _heapAllocHistory = [];
  _heapSysHistory = [];
  _stackHistory = [];
  _goroutineHistory = [];
  _gcPauseHistory = [];
  _timestamps = [];

  await loadHistory();
  await fetchAndRender(container);
  startAutoRefresh(container);
}

export function hideServerStatusPage() {
  _visible = false;
  stopAutoRefresh();
  const container = document.getElementById('server-status-page');
  if (container) container.classList.add('hidden');

  document.getElementById('article-list-pane').classList.remove('hidden');
  document.getElementById('reader-pane').classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    document.getElementById('app').classList.add('hide-middle-pane');
  }
}

export function isServerStatusVisible() { return _visible; }

function startAutoRefresh(container) {
  stopAutoRefresh();
  _refreshTimer = setInterval(() => {
    if (_visible) fetchAndRender(container);
  }, _refreshIntervalMs);
}

function stopAutoRefresh() {
  if (_refreshTimer) {
    clearInterval(_refreshTimer);
    _refreshTimer = null;
  }
}

async function loadHistory() {
  try {
    const resp = await apiFetch('/api/server-status/history');
    const items = resp.history || [];
    for (const snap of items) {
      _timestamps.push(new Date(snap.timestamp));
      _heapAllocHistory.push(snap.heap_alloc || 0);
      _heapSysHistory.push(snap.heap_sys || 0);
      _stackHistory.push(snap.stack_in_use || 0);
      _goroutineHistory.push(snap.goroutines || 0);
      _gcPauseHistory.push(snap.gc_pause_ns || 0);
    }
  } catch (e) {
    // Silently ignore — we'll still collect live data
  }
}

async function fetchAndRender(container) {
  try {
    const [data, offlineData] = await Promise.all([
      apiFetch('/api/server-status'),
      fetch('/api/offline-status').then(r => r.ok ? r.json() : {}).catch(() => {}),
    ]);
    if (!_visible) return;

    // Accumulate history
    const mem = data.memory || {};
    const now = new Date();
    _timestamps.push(now);
    _heapAllocHistory.push(mem.heap_alloc_bytes || 0);
    _heapSysHistory.push(mem.heap_sys_bytes || 0);
    _stackHistory.push(mem.stack_in_use_bytes || 0);
    _goroutineHistory.push(data.server?.goroutines || 0);

    // GC pauses - append the latest from recent_pauses_ns
    const recentPauses = data.gc?.recent_pauses_ns || [];
    if (recentPauses.length > 0) {
      // Only add the last pause if it's new
      const lastPause = recentPauses[recentPauses.length - 1];
      _gcPauseHistory.push(lastPause);
    }

    renderPage(container, data, offlineData || {});
  } catch (e) {
    if (_visible) {
      container.innerHTML = `<div class="status-error">Error loading server status: ${e.message}</div>`;
    }
  }
}

function renderPage(container, data, offlineData = {}) {
  const srv = data.server || {};
  const host = data.host || {};
  const mem = data.memory || {};
  const gc = data.gc || {};
  const cache = data.write_cache || {};
  const duckdb = data.duckdb_process || null;

  const refreshLabel = `Auto-refresh: ${_refreshIntervalMs / 1000}s`;

  container.innerHTML = `
    <div class="status-inner">
    <h1 class="status-title">Server Status</h1>
    <p class="status-subtitle">${host.hostname || 'unknown'} - ${srv.os || ''}/${srv.arch || ''} - ${srv.go_version || ''}</p>

    <div class="status-cards">
      ${statCard('Host Up', host.uptime_seconds >= 0 ? formatDuration(host.uptime_seconds) : 'N/A', '\uD83D\uDDA5')}
      ${statCard('RSS-Lance Up', formatDuration(srv.uptime_seconds), '\u23F1')}
      ${duckdb ? statCard('DuckDB Up', formatDuration(duckdb.uptime_seconds), '\uD83E\uDD86') : ''}
      ${duckdb && duckdb.duckdb_version ? statCard('DuckDB', duckdb.duckdb_version, '\uD83D\uDCE6') : ''}
      ${duckdb && duckdb.lance_version ? statCard('Lance Ext', duckdb.lance_version, '\uD83D\uDD31') : ''}
      ${(() => {
        const offline = offlineData.offline;
        const label = 'Lance Tables';
        return offline
          ? statCard(label, 'Offline', '\uD83D\uDDC4', '#f97316')
          : statCard(label, 'Online', '\uD83D\uDDC4', 'var(--accent)');
      })()}
    </div>

    <div class="status-cards">
      ${statCard('Goroutines', srv.goroutines, '\uD83E\udDF5')}
      ${statCard('GC Runs', gc.num_gc || 0, '\u267B')}
      ${statCard('Heap Used', formatBytes(mem.heap_alloc_bytes), '\uD83D\uDCE6')}
      ${statCard('Heap Sys', formatBytes(mem.heap_sys_bytes), '\uD83D\uDCBE')}
      ${statCard('Stack', formatBytes(mem.stack_in_use_bytes), '\uD83D\uDDC2')}
      ${statCard('Total Sys', formatBytes(mem.sys_bytes), '\uD83D\uDCCA')}
    </div>

    <div class="status-grid">
      <div class="status-section">
        <h2>Memory Over Time</h2>
        <div class="svs-chart-container">${renderLineChart([
          { data: _heapAllocHistory, color: 'var(--accent)', label: 'Heap Alloc' },
          { data: _heapSysHistory, color: 'var(--warning)', label: 'Heap Sys' },
          { data: _stackHistory, color: '#8b5cf6', label: 'Stack' },
        ], true)}</div>
        <div class="chart-legend">
          <span class="legend-item"><span class="legend-dot" style="background:var(--accent)"></span>Heap Alloc</span>
          <span class="legend-item"><span class="legend-dot" style="background:var(--warning)"></span>Heap Sys</span>
          <span class="legend-item"><span class="legend-dot" style="background:#8b5cf6"></span>Stack</span>
        </div>
      </div>

      <div class="status-section">
        <h2>GC Pause Times</h2>
        <div class="svs-chart-container">${renderBarChart(_gcPauseHistory, 'var(--accent)')}</div>
        <p class="chart-note">Last pause: ${formatNs(gc.last_pause_ns)} | Total: ${formatNs(gc.total_pause_ns)} | CPU: ${(gc.gc_cpu_fraction * 100).toFixed(3)}%</p>
      </div>
    </div>

    <div class="status-grid">
      <div class="status-section">
        <h2>Goroutine Count</h2>
        <div class="svs-chart-container">${renderLineChart([
          { data: _goroutineHistory, color: 'var(--accent)', label: 'Goroutines' },
        ], false)}</div>
      </div>

      <div class="status-section">
        <h2>Write Cache</h2>
        <div class="cache-stats">
          <div class="cache-stat">
            <span class="cache-stat-value">${cache.pending_reads || 0}</span>
            <span class="cache-stat-label">Pending Reads</span>
          </div>
          <div class="cache-stat">
            <span class="cache-stat-value">${cache.pending_stars || 0}</span>
            <span class="cache-stat-label">Pending Stars</span>
          </div>
          <div class="cache-stat">
            <span class="cache-stat-value">${cache.last_flush_time ? formatRelTime(cache.last_flush_time) : 'N/A'}</span>
            <span class="cache-stat-label">Last Flush</span>
          </div>
          <div class="cache-stat">
            <span class="cache-stat-value">${cache.last_flush_duration_ms ? cache.last_flush_duration_ms + 'ms' : 'N/A'}</span>
            <span class="cache-stat-label">Flush Duration</span>
          </div>
        </div>
        <button id="flush-cache-btn" class="flush-btn" aria-label="Flush write cache to Lance">Flush Now</button>
      </div>
    </div>

    ${duckdb ? `
    <div class="status-section">
      <h2>DuckDB Process</h2>
      ${duckdb.stopped ? `
      <div style="background:var(--bg-warning, #fff3cd);border:1px solid var(--border-warning, #ffc107);border-radius:6px;padding:12px 16px;margin-bottom:12px;color:var(--text-warning, #856404)">
        <strong>DuckDB is stopped for upgrade.</strong> Replace the <code>tools/duckdb</code> binary, then click <strong>Start DuckDB</strong> below.
        ${duckdb.duckdb_version ? `<br>Previous version: DuckDB ${duckdb.duckdb_version}` : ''}
        ${duckdb.lance_version ? `, Lance ext ${duckdb.lance_version}` : ''}
      </div>
      <button id="start-duckdb-btn" class="flush-btn" aria-label="Start DuckDB after binary upgrade">Start DuckDB</button>
      ` : `
      <div class="cache-stats">
        <div class="cache-stat">
          <span class="cache-stat-value">${duckdb.pid}</span>
          <span class="cache-stat-label">PID</span>
        </div>
        <div class="cache-stat">
          <span class="cache-stat-value">${formatDuration(duckdb.uptime_seconds)}</span>
          <span class="cache-stat-label">Uptime</span>
        </div>
        <div class="cache-stat">
          <span class="cache-stat-value">${duckdb.duckdb_version || 'N/A'}</span>
          <span class="cache-stat-label">DuckDB Version</span>
        </div>
        <div class="cache-stat">
          <span class="cache-stat-value">${duckdb.lance_version || 'N/A'}</span>
          <span class="cache-stat-label">Lance Extension</span>
        </div>
      </div>
      <div style="display:flex;gap:8px;flex-wrap:wrap">
        <button id="restart-duckdb-btn" class="flush-btn" aria-label="Gracefully restart DuckDB process">Restart DuckDB</button>
        <button id="stop-duckdb-btn" class="flush-btn" aria-label="Flush cache and stop DuckDB for binary upgrade" style="background:var(--btn-warning-bg, #e2a300);color:var(--btn-warning-text, #000)">Stop for Upgrade</button>
      </div>
      `}
    </div>
    ` : ''}

    <div class="status-section">
      <h2>Server Info</h2>
      <table class="status-table">
        <tbody>
          <tr><td>PID</td><td>${srv.pid}</td></tr>
          ${duckdb && !duckdb.stopped ? `<tr><td>DuckDB PID</td><td>${duckdb.pid}</td></tr>` : ''}
          ${duckdb && duckdb.stopped ? `<tr><td>DuckDB Status</td><td>Stopped for upgrade</td></tr>` : ''}
          ${duckdb && duckdb.duckdb_version ? `<tr><td>DuckDB Version</td><td>${duckdb.duckdb_version}</td></tr>` : ''}
          ${duckdb && duckdb.lance_version ? `<tr><td>Lance Extension</td><td>${duckdb.lance_version}</td></tr>` : ''}
          <tr><td>Go Version</td><td>${srv.go_version}</td></tr>
          <tr><td>OS / Arch</td><td>${srv.os} / ${srv.arch}</td></tr>
          <tr><td>CPUs</td><td>${srv.num_cpu}</td></tr>
          <tr><td>Hostname</td><td>${host.hostname || 'unknown'}</td></tr>
          <tr><td>Started</td><td>${srv.start_time ? new Date(srv.start_time).toLocaleString() : 'N/A'}</td></tr>
          <tr><td>Build Revision</td><td>${srv.build_vcs_revision || 'N/A'}</td></tr>
          <tr><td>VCS Time</td><td>${srv.build_vcs_time || 'N/A'}</td></tr>
          <tr><td>Build Time</td><td>${srv.build_time ? new Date(srv.build_time).toLocaleString() : 'N/A'}</td></tr>
          <tr><td>Build Version</td><td>${srv.build_version || 'N/A'}</td></tr>
          <tr><td>Heap Objects</td><td>${(mem.heap_objects || 0).toLocaleString()}</td></tr>
          <tr><td>Total Alloc (lifetime)</td><td>${formatBytes(mem.total_alloc_bytes)}</td></tr>
          <tr><td>Mallocs / Frees</td><td>${(mem.mallocs || 0).toLocaleString()} / ${(mem.frees || 0).toLocaleString()}</td></tr>
          <tr><td>Next GC Target</td><td>${formatBytes(gc.next_gc_target_bytes)}</td></tr>
        </tbody>
      </table>
    </div>

    <p class="status-subtitle" style="text-align:right;margin-top:8px">${refreshLabel} | ${_timestamps.length} data points</p>
    </div>
  `;

  // Wire up flush button
  const flushBtn = container.querySelector('#flush-cache-btn');
  if (flushBtn) {
    flushBtn.onclick = async () => {
      flushBtn.disabled = true;
      flushBtn.textContent = 'Flushing...';
      try {
        await apiFetch('/api/flush', { method: 'POST' });
        flushBtn.textContent = 'Flushed!';
        setTimeout(() => { flushBtn.textContent = 'Flush Now'; flushBtn.disabled = false; }, 2000);
      } catch {
        flushBtn.textContent = 'Error';
        setTimeout(() => { flushBtn.textContent = 'Flush Now'; flushBtn.disabled = false; }, 2000);
      }
    };
  }

  // Wire up DuckDB restart button
  const restartBtn = container.querySelector('#restart-duckdb-btn');
  if (restartBtn) {
    restartBtn.onclick = async () => {
      restartBtn.disabled = true;
      restartBtn.textContent = 'Restarting...';
      try {
        await apiFetch('/api/duckdb/restart', { method: 'POST' });
        restartBtn.textContent = 'Restarted!';
        setTimeout(() => { restartBtn.textContent = 'Restart DuckDB'; restartBtn.disabled = false; }, 2000);
        fetchAndRender(container);
      } catch {
        restartBtn.textContent = 'Error';
        setTimeout(() => { restartBtn.textContent = 'Restart DuckDB'; restartBtn.disabled = false; }, 2000);
      }
    };
  }

  // Wire up DuckDB stop-for-upgrade button
  const stopBtn = container.querySelector('#stop-duckdb-btn');
  if (stopBtn) {
    stopBtn.onclick = async () => {
      if (!confirm('This will flush the write cache and stop DuckDB.\\nAPI queries will fail until you start it again.\\n\\nContinue?')) return;
      stopBtn.disabled = true;
      stopBtn.textContent = 'Stopping...';
      try {
        await apiFetch('/api/duckdb/stop', { method: 'POST' });
        stopBtn.textContent = 'Stopped!';
        fetchAndRender(container);
      } catch {
        stopBtn.textContent = 'Error';
        setTimeout(() => { stopBtn.textContent = 'Stop for Upgrade'; stopBtn.disabled = false; }, 2000);
      }
    };
  }

  // Wire up DuckDB start-after-upgrade button
  const startBtn = container.querySelector('#start-duckdb-btn');
  if (startBtn) {
    startBtn.onclick = async () => {
      startBtn.disabled = true;
      startBtn.textContent = 'Starting...';
      try {
        await apiFetch('/api/duckdb/start', { method: 'POST' });
        startBtn.textContent = 'Started!';
        fetchAndRender(container);
      } catch {
        startBtn.textContent = 'Error';
        setTimeout(() => { startBtn.textContent = 'Start DuckDB'; startBtn.disabled = false; }, 2000);
      }
    };
  }
}

// ── SVG Chart Renderers ─────────────────────────────────────────────────────

function renderLineChart(series, formatAsBytes) {
  const w = 500, h = 120, pad = 4;
  const chartW = w - pad * 2, chartH = h - pad * 2;

  // Find global min/max across all series
  let allVals = [];
  for (const s of series) allVals = allVals.concat(s.data);
  if (allVals.length < 2) {
    return `<svg viewBox="0 0 ${w} ${h}" class="svs-chart"><text x="${w/2}" y="${h/2}" text-anchor="middle" fill="var(--text-muted)" font-size="12">Collecting data...</text></svg>`;
  }

  let minVal = Math.min(...allVals);
  let maxVal = Math.max(...allVals);
  if (maxVal === minVal) { maxVal = minVal + 1; }

  const paths = series.map(s => {
    const points = s.data.map((v, i) => {
      const x = pad + (i / (s.data.length - 1)) * chartW;
      const y = pad + chartH - ((v - minVal) / (maxVal - minVal)) * chartH;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    });
    return `<polyline points="${points.join(' ')}" fill="none" stroke="${s.color}" stroke-width="1.5" stroke-linejoin="round"/>`;
  }).join('');

  // Axis labels
  const topLabel = formatAsBytes ? formatBytes(maxVal) : maxVal;
  const botLabel = formatAsBytes ? formatBytes(minVal) : minVal;

  return `<svg viewBox="0 0 ${w} ${h}" class="svs-chart">
    <rect x="${pad}" y="${pad}" width="${chartW}" height="${chartH}" fill="none" stroke="var(--bg3)" stroke-width="0.5"/>
    ${paths}
    <text x="${pad + 2}" y="${pad + 10}" fill="var(--text-muted)" font-size="9">${topLabel}</text>
    <text x="${pad + 2}" y="${h - pad - 2}" fill="var(--text-muted)" font-size="9">${botLabel}</text>
  </svg>`;
}

function renderBarChart(data, color) {
  const w = 500, h = 120, pad = 4;
  const chartW = w - pad * 2, chartH = h - pad * 2;

  if (data.length < 1) {
    return `<svg viewBox="0 0 ${w} ${h}" class="svs-chart"><text x="${w/2}" y="${h/2}" text-anchor="middle" fill="var(--text-muted)" font-size="12">Collecting data...</text></svg>`;
  }

  const maxVal = Math.max(...data, 1);
  const barW = Math.max(1, chartW / data.length - 1);

  const bars = data.map((v, i) => {
    const barH = (v / maxVal) * chartH;
    const x = pad + (i / data.length) * chartW;
    const y = pad + chartH - barH;
    return `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${barW.toFixed(1)}" height="${barH.toFixed(1)}" fill="${color}" opacity="0.8"/>`;
  }).join('');

  return `<svg viewBox="0 0 ${w} ${h}" class="svs-chart">
    <rect x="${pad}" y="${pad}" width="${chartW}" height="${chartH}" fill="none" stroke="var(--bg3)" stroke-width="0.5"/>
    ${bars}
    <text x="${pad + 2}" y="${pad + 10}" fill="var(--text-muted)" font-size="9">${formatNs(maxVal)}</text>
  </svg>`;
}

// ── Formatting Helpers ──────────────────────────────────────────────────────

function statCard(label, value, icon, valueColor = null) {
  const colorStyle = valueColor ? ` style="color:${valueColor}"` : '';
  return `
    <div class="stat-card">
      <div class="stat-icon">${icon}</div>
      <div class="stat-value"${colorStyle}>${value}</div>
      <div class="stat-label">${label}</div>
    </div>`;
}

function formatDuration(totalSecs) {
  if (totalSecs == null || totalSecs < 0) return 'N/A';
  const d = Math.floor(totalSecs / 86400);
  const h = Math.floor((totalSecs % 86400) / 3600);
  const m = Math.floor((totalSecs % 3600) / 60);
  const s = Math.floor(totalSecs % 60);
  const parts = [];
  if (d > 0) parts.push(`${d}d`);
  if (h > 0 || d > 0) parts.push(`${h}h`);
  parts.push(`${m}m`);
  parts.push(`${s}s`);
  return parts.join(' ');
}

function formatBytes(bytes) {
  if (bytes == null || bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const val = bytes / Math.pow(1024, i);
  return `${val < 10 ? val.toFixed(1) : Math.round(val)} ${units[i]}`;
}

function formatNs(ns) {
  if (ns == null) return 'N/A';
  if (ns < 1000) return ns + 'ns';
  if (ns < 1000000) return (ns / 1000).toFixed(1) + 'us';
  if (ns < 1000000000) return (ns / 1000000).toFixed(2) + 'ms';
  return (ns / 1000000000).toFixed(2) + 's';
}

function formatRelTime(isoStr) {
  if (!isoStr) return 'N/A';
  const t = new Date(isoStr);
  const diff = (Date.now() - t.getTime()) / 1000;
  if (diff < 60) return Math.round(diff) + 's ago';
  if (diff < 3600) return Math.round(diff / 60) + 'm ago';
  return Math.round(diff / 3600) + 'h ago';
}
