/**
 * RSS-Lance - Settings page module
 *
 * Full-page settings panel for options that need more than a toggle switch,
 * such as custom CSS, displayed in the middle + reader pane area.
 */
import { apiFetch } from './app.js';
import { hideStatusPage, isStatusVisible } from './status.js';
import { hideLogsPage, isLogsVisible } from './logs-page.js';
import { hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';

let _settingsVisible = false;

/** Show the settings page, replacing middle + reader panes. */
export async function showSettingsPage() {
  // Dismiss any other active special page
  if (isStatusVisible()) hideStatusPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();

  const app = document.getElementById('app');
  const listPane  = document.getElementById('article-list-pane');
  const readerPane = document.getElementById('reader-pane');

  listPane.classList.add('hidden');
  readerPane.classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  let container = document.getElementById('settings-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'settings-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  container.innerHTML = '<div class="settings-page-loading">Loading settings…<div class="page-loader"></div></div>';

  _settingsVisible = true;

  try {
    const settings = await apiFetch('/api/settings');
    if (!_settingsVisible) return;
    renderSettingsPage(container, settings);
  } catch (e) {
    container.innerHTML = `<div class="settings-page-error">Error loading settings: ${e.message}</div>`;
  }
}

/** Hide the settings page and restore normal panes. */
export function hideSettingsPage() {
  _settingsVisible = false;
  const container = document.getElementById('settings-page');
  if (container) container.classList.add('hidden');

  document.getElementById('article-list-pane').classList.remove('hidden');
  document.getElementById('reader-pane').classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    document.getElementById('app').classList.add('hide-middle-pane');
  }
  if (localStorage.getItem('rss-lance-auto-read') === 'off') {
    // auto-read was off; reader.js checks localStorage directly
  }
}

export function isSettingsPageVisible() { return _settingsVisible; }

// ── Rendering ─────────────────────────────────────────────────────────────────

function renderSettingsPage(container, settings) {
  const customCSS = settings.custom_css || '';

  // Log settings grouped by service
  const logGroups = [
    {
      id: 'fetcher',
      title: 'Fetcher Logging',
      desc: 'Control what the feed fetcher logs during fetch cycles.',
      master: 'log.fetcher.enabled',
      options: [
        { key: 'log.fetcher.fetch_cycle',        label: 'Fetch cycles',          desc: 'Summary of each fetch cycle (feeds due, articles written)' },
        { key: 'log.fetcher.feed_fetch',          label: 'Feed fetches',          desc: 'Each feed fetched with article count' },
        { key: 'log.fetcher.article_processing',  label: 'Article processing',    desc: 'Debug: each article being processed (verbose)' },
        { key: 'log.fetcher.compaction',          label: 'Compaction',            desc: 'Table compaction events' },
        { key: 'log.fetcher.tier_changes',        label: 'Tier changes',          desc: 'Feed tier up/downgrades' },
        { key: 'log.fetcher.sanitization',       label: 'Sanitization',          desc: 'Debug: what the sanitizer stripped (tracking pixels, dangerous HTML, social links, chrome)' },
        { key: 'log.fetcher.errors',              label: 'Errors',                desc: 'Fetch errors and failures' },
      ]
    },
    {
      id: 'api',
      title: 'API Server Logging',
      desc: 'Control what the HTTP server logs.',
      master: 'log.api.enabled',
      options: [
        { key: 'log.api.lifecycle',         label: 'Server lifecycle',      desc: 'Server start/stop events' },
        { key: 'log.api.requests',          label: 'All API requests',      desc: 'Every API call (verbose)' },
        { key: 'log.api.settings_changes',  label: 'Settings changes',      desc: 'When settings are modified' },
        { key: 'log.api.feed_actions',      label: 'Feed actions',          desc: 'Add feed, mark-all-read, etc.' },
        { key: 'log.api.article_actions',   label: 'Article actions',       desc: 'Read/star individual articles (verbose)' },
        { key: 'log.api.errors',            label: 'Errors',                desc: 'Error responses' },
      ]
    }
  ];

  const retentionVal = settings['log.max_entries'] ?? 10000;
  const retentionMode = settings['log.retention_mode'] ?? 'count';
  const maxAgeDays = settings['log.max_age_days'] ?? 30;

  // Advanced settings grouped by component
  const advancedGroups = [
    {
      title: 'Fetcher',
      desc: 'Feed fetching, tier management, and fetcher-side compaction.',
      fields: [
        { key: 'fetcher.interval_minutes',     label: 'Fetch interval (mins)',      desc: 'Minutes between fetch cycles (daemon mode)', def: 30 },
        { key: 'fetcher.max_concurrent',        label: 'Max concurrent fetches',     desc: 'Number of feeds fetched in parallel', def: 5 },
        { key: 'fetcher.user_agent',            label: 'User-Agent',                 desc: 'HTTP User-Agent header sent when fetching feeds', def: 'RSS-Lance/1.0', type: 'text' },
        { key: 'fetcher.fetch_timeout_secs',    label: 'Fetch timeout (secs)',       desc: 'HTTP request timeout in seconds for fetching feeds', def: 20 },
        { key: 'fetcher.poll_interval_secs',    label: 'Scheduler poll interval (secs)', desc: 'Seconds between scheduler poll checks in daemon mode', def: 30 },
        { key: 'tier.threshold.active',         label: 'Tier threshold: active (days)',   desc: 'Days without articles before downgrading from active → slowing', def: 3 },
        { key: 'tier.threshold.slowing',        label: 'Tier threshold: slowing (days)',  desc: 'Days without articles before downgrading from slowing → quiet', def: 14 },
        { key: 'tier.threshold.quiet',          label: 'Tier threshold: quiet (days)',    desc: 'Days without articles before downgrading from quiet → dormant', def: 60 },
        { key: 'tier.threshold.dormant',        label: 'Tier threshold: dormant (days)',  desc: 'Days without articles before downgrading from dormant → dead', def: 180 },
        { key: 'tier.interval.active',          label: 'Tier interval: active (mins)',    desc: 'Minutes between fetches for active feeds', def: 30 },
        { key: 'tier.interval.slowing',         label: 'Tier interval: slowing (mins)',   desc: 'Minutes between fetches for slowing feeds', def: 1440 },
        { key: 'tier.interval.quiet',           label: 'Tier interval: quiet (mins)',     desc: 'Minutes between fetches for quiet feeds', def: 10080 },
        { key: 'tier.interval.dormant',         label: 'Tier interval: dormant (mins)',   desc: 'Minutes between fetches for dormant feeds', def: 43200 },
        { key: 'compaction.articles',           label: 'Compaction: articles',       desc: 'Compact after this many fragments (articles table grows fastest)', def: 20 },
        { key: 'compaction.feeds',              label: 'Compaction: feeds',          desc: 'Compact after this many fragments', def: 50 },
        { key: 'compaction.categories',         label: 'Compaction: categories',     desc: 'Compact after this many fragments', def: 50 },
        { key: 'compaction.pending_feeds',      label: 'Compaction: pending feeds',  desc: 'Compact after this many fragments', def: 10 },
        { key: 'compaction.log_fetcher',        label: 'Compaction: fetcher logs',   desc: 'Compact fetcher log table after this many fragments', def: 20 },
      ]
    },
    {
      title: 'Server',
      desc: 'Go API server write-cache, log buffering, and server-side compaction.',
      fields: [
        { key: 'server.table_page_size',        label: 'Table viewer page size',     desc: 'Rows per page in the DB Table Viewer (max 5000)', def: 200 },
        { key: 'stats.retention_minutes',       label: 'Stats retention (minutes)',  desc: 'How many minutes of server metrics history to keep in memory (charts on Server Status page)', def: 60 },
        { key: 'cache.flush_threshold',         label: 'Cache: flush threshold',     desc: 'Flush article read/star cache after N changes', def: 20 },
        { key: 'cache.flush_interval_secs',     label: 'Cache: flush interval (secs)', desc: 'Flush cache after N seconds (whichever comes first)', def: 120 },
        { key: 'log_buffer.flush_threshold',    label: 'Log buffer: flush threshold', desc: 'Flush buffered log entries after N entries', def: 20 },
        { key: 'log_buffer.flush_interval_secs',label: 'Log buffer: flush interval (secs)', desc: 'Flush buffered log entries after N seconds', def: 30 },
        { key: 'compaction.log_api',            label: 'Compaction: API logs',       desc: 'Compact API log table after this many fragments', def: 20 },
      ]
    }
  ];

  container.innerHTML = `
    <div class="settings-page-inner">
      <h1 class="settings-page-title">Settings</h1>

      <div class="settings-page-section">
        <h2>Appearance</h2>
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">Dark mode</span>
            <span class="log-option-desc">Switch between dark and light themes</span>
          </div>
          <label class="toggle-switch toggle-sm">
            <input type="checkbox" id="sp-toggle-theme"
              ${settings['ui.theme'] !== 'light' ? 'checked' : ''}>
            <span class="toggle-slider"></span>
          </label>
        </div>
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">Show article list</span>
            <span class="log-option-desc">Show or hide the article list middle pane</span>
          </div>
          <label class="toggle-switch toggle-sm">
            <input type="checkbox" id="sp-toggle-middle"
              ${settings['ui.show_article_list'] !== false ? 'checked' : ''}>
            <span class="toggle-slider"></span>
          </label>
        </div>
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">Auto-read on scroll</span>
            <span class="log-option-desc">Automatically mark articles as read when they scroll out of view</span>
          </div>
          <label class="toggle-switch toggle-sm">
            <input type="checkbox" id="sp-toggle-autoread"
              ${settings['ui.auto_read'] !== false ? 'checked' : ''}>
            <span class="toggle-slider"></span>
          </label>
        </div>
      </div>

      ${logGroups.map(group => `
      <div class="settings-page-section log-section">
        <div class="log-section-header">
          <div>
            <h2>${group.title}</h2>
            <p class="settings-page-hint">${group.desc}</p>
          </div>
          <label class="toggle-switch">
            <input type="checkbox" data-log-key="${group.master}"
              ${settings[group.master] !== false ? 'checked' : ''}>
            <span class="toggle-slider"></span>
          </label>
        </div>
        <div class="log-options" data-master="${group.master}">
          ${group.options.map(opt => `
          <div class="log-option-row">
            <div class="log-option-info">
              <span class="log-option-label">${opt.label}</span>
              <span class="log-option-desc">${opt.desc}</span>
            </div>
            <label class="toggle-switch toggle-sm">
              <input type="checkbox" data-log-key="${opt.key}"
                ${settings[opt.key] !== false ? 'checked' : ''}>
              <span class="toggle-slider"></span>
            </label>
          </div>
          `).join('')}
        </div>
      </div>
      `).join('')}

      <div class="settings-page-section">
        <h2>Log Retention</h2>
        <p class="settings-page-hint">
          Choose how to limit stored log entries. Each service trims its own logs automatically.
        </p>
        <div class="log-retention-inline">
          <span class="log-option-label">Retention mode</span>
          <select id="log-retention-mode" class="settings-page-input" style="width:auto;min-width:100px">
            <option value="count" ${retentionMode !== 'age' ? 'selected' : ''}>Count</option>
            <option value="age" ${retentionMode === 'age' ? 'selected' : ''}>Age (days)</option>
          </select>
          <span id="retention-count-row" class="log-retention-row" style="${retentionMode === 'age' ? 'display:none' : ''}">
            <input type="number" id="log-max-entries" class="settings-page-input"
              value="${retentionVal}" min="0" max="100000" step="1000">
            <span class="settings-page-hint">entries &nbsp;(0 = keep all)</span>
          </span>
          <span id="retention-age-row" class="log-retention-row" style="${retentionMode !== 'age' ? 'display:none' : ''}">
            <input type="number" id="log-max-age-days" class="settings-page-input"
              value="${maxAgeDays}" min="1" max="3650" step="1">
            <span class="settings-page-hint">days</span>
          </span>
        </div>
      </div>

      <div class="settings-page-actions" style="margin-top: 16px">
        <button id="btn-save-log-settings" class="settings-page-btn primary">Save Log Settings</button>
        <span id="log-save-status" class="settings-page-status"></span>
      </div>

      ${advancedGroups.map(group => `
      <div class="settings-page-section">
        <h2>${group.title}</h2>
        <p class="settings-page-hint">${group.desc} Changes take effect on next restart.</p>
        ${group.fields.map(f => `
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">${f.label}</span>
            <span class="log-option-desc">${f.desc}</span>
          </div>
          <input type="${f.type || 'number'}" data-adv-key="${f.key}" class="settings-page-input settings-page-input-sm"
            value="${settings[f.key] ?? f.def}"${f.type === 'text' ? '' : ' min="1" max="100000" step="1"'}>
        </div>
        `).join('')}
      </div>
      `).join('')}
      <div class="settings-page-actions" style="margin-top: 12px">
        <button id="btn-save-advanced" class="settings-page-btn primary">Save Advanced Settings</button>
        <span id="advanced-save-status" class="settings-page-status"></span>
      </div>

      <div class="settings-page-section">
        <h2>Custom CSS</h2>
        <p class="settings-page-hint">
          Add your own CSS rules to customise the look of RSS-Lance.
          Changes are applied when you save.
        </p>
        <textarea id="custom-css-editor" class="settings-page-textarea"
          spellcheck="false" placeholder="/* e.g. body { font-size: 18px; } */">${escapeHTML(customCSS)}</textarea>
        <div class="settings-page-actions">
          <button id="btn-save-css" class="settings-page-btn primary">Save</button>
          <span id="css-save-status" class="settings-page-status"></span>
        </div>
      </div>
    </div>
  `;

  // ---- Custom CSS handling ----
  const editor = document.getElementById('custom-css-editor');
  const btnSave = document.getElementById('btn-save-css');
  const status  = document.getElementById('css-save-status');

  // Tab key inserts a tab character instead of moving focus
  editor.addEventListener('keydown', (e) => {
    if (e.key === 'Tab') {
      e.preventDefault();
      const start = editor.selectionStart;
      const end = editor.selectionEnd;
      editor.value = editor.value.substring(0, start) + '  ' + editor.value.substring(end);
      editor.selectionStart = editor.selectionEnd = start + 2;
    }
  });

  // ---- Appearance toggles (save to server + cache in localStorage) ----
  const themeToggle = document.getElementById('sp-toggle-theme');
  if (themeToggle) {
    themeToggle.addEventListener('change', async () => {
      const theme = themeToggle.checked ? 'dark' : 'light';
      document.body.classList.toggle('light-theme', !themeToggle.checked);
      localStorage.setItem('rss-lance-theme', theme);
      try { await apiFetch('/api/settings', { method: 'PUT', body: JSON.stringify({ 'ui.theme': theme }) }); } catch (_) {}
    });
  }
  const middleToggle = document.getElementById('sp-toggle-middle');
  if (middleToggle) {
    middleToggle.addEventListener('change', async () => {
      const show = middleToggle.checked;
      document.getElementById('app').classList.toggle('hide-middle-pane', !show);
      localStorage.setItem('rss-lance-middle-pane', show ? 'visible' : 'hidden');
      try { await apiFetch('/api/settings', { method: 'PUT', body: JSON.stringify({ 'ui.show_article_list': show }) }); } catch (_) {}
    });
  }
  const autoReadToggle = document.getElementById('sp-toggle-autoread');
  if (autoReadToggle) {
    autoReadToggle.addEventListener('change', async () => {
      const on = autoReadToggle.checked;
      localStorage.setItem('rss-lance-auto-read', on ? 'on' : 'off');
      try { await apiFetch('/api/settings', { method: 'PUT', body: JSON.stringify({ 'ui.auto_read': on }) }); } catch (_) {}
    });
  }

  // ---- Master toggle disables child options ----
  container.querySelectorAll('.log-section-header input[data-log-key]').forEach(masterInput => {
    const masterKey = masterInput.dataset.logKey;
    const optionsDiv = container.querySelector(`.log-options[data-master="${masterKey}"]`);
    function updateDisabled() {
      if (optionsDiv) {
        optionsDiv.classList.toggle('disabled', !masterInput.checked);
        optionsDiv.querySelectorAll('input').forEach(inp => {
          inp.disabled = !masterInput.checked;
        });
      }
    }
    updateDisabled();
    masterInput.addEventListener('change', updateDisabled);
  });

  // ---- Retention mode toggle ----
  const retModeSelect = document.getElementById('log-retention-mode');
  const retCountRow = document.getElementById('retention-count-row');
  const retAgeRow = document.getElementById('retention-age-row');
  if (retModeSelect) {
    retModeSelect.addEventListener('change', () => {
      const isAge = retModeSelect.value === 'age';
      retCountRow.style.display = isAge ? 'none' : '';
      retAgeRow.style.display = isAge ? '' : 'none';
    });
  }

  // ---- Save log settings ----
  const btnSaveLogs = document.getElementById('btn-save-log-settings');
  const logStatus = document.getElementById('log-save-status');

  btnSaveLogs.addEventListener('click', async () => {
    btnSaveLogs.disabled = true;
    logStatus.textContent = 'Saving...';
    logStatus.className = 'settings-page-status';

    const payload = {};
    container.querySelectorAll('input[data-log-key]').forEach(inp => {
      payload[inp.dataset.logKey] = inp.checked;
    });

    const mode = document.getElementById('log-retention-mode').value;
    payload['log.retention_mode'] = mode;

    if (mode === 'age') {
      const days = parseInt(document.getElementById('log-max-age-days').value, 10);
      if (days >= 1) {
        payload['log.max_age_days'] = days;
      }
    } else {
      const maxEntries = parseInt(document.getElementById('log-max-entries').value, 10);
      if (maxEntries === 0 || maxEntries >= 100) {
        payload['log.max_entries'] = maxEntries;
      }
    }

    try {
      await apiFetch('/api/settings', {
        method: 'PUT',
        body: JSON.stringify(payload),
      });
      logStatus.textContent = 'Saved!';
      logStatus.className = 'settings-page-status success';
    } catch (e) {
      logStatus.textContent = `Error: ${e.message}`;
      logStatus.className = 'settings-page-status error';
    } finally {
      btnSaveLogs.disabled = false;
    }
  });

  // ---- Save advanced settings ----
  const btnSaveAdv = document.getElementById('btn-save-advanced');
  const advStatus = document.getElementById('advanced-save-status');

  btnSaveAdv.addEventListener('click', async () => {
    btnSaveAdv.disabled = true;
    advStatus.textContent = 'Saving...';
    advStatus.className = 'settings-page-status';

    const payload = {};
    container.querySelectorAll('input[data-adv-key]').forEach(inp => {
      if (inp.type === 'text') {
        if (inp.value.trim()) payload[inp.dataset.advKey] = inp.value.trim();
      } else {
        const val = parseInt(inp.value, 10);
        if (val > 0) payload[inp.dataset.advKey] = val;
      }
    });

    try {
      await apiFetch('/api/settings', {
        method: 'PUT',
        body: JSON.stringify(payload),
      });
      advStatus.textContent = 'Saved! Restart server/fetcher to apply.';
      advStatus.className = 'settings-page-status success';
    } catch (e) {
      advStatus.textContent = `Error: ${e.message}`;
      advStatus.className = 'settings-page-status error';
    } finally {
      btnSaveAdv.disabled = false;
    }
  });

  btnSave.addEventListener('click', async () => {
    btnSave.disabled = true;
    status.textContent = 'Saving…';
    status.className = 'settings-page-status';

    try {
      await apiFetch('/api/settings/custom_css', {
        method: 'PUT',
        body: JSON.stringify({ value: editor.value }),
      });
      status.textContent = 'Saved!';
      status.className = 'settings-page-status success';

      // Reload the custom CSS stylesheet so changes take effect immediately
      reloadCustomCSS();
    } catch (e) {
      status.textContent = `Error: ${e.message}`;
      status.className = 'settings-page-status error';
    } finally {
      btnSave.disabled = false;
    }
  });
}

function reloadCustomCSS() {
  const link = document.querySelector('link[href*="custom.css"]');
  if (link) {
    const url = new URL(link.href, location.origin);
    url.searchParams.set('_t', Date.now());
    link.href = url.toString();
  }
}

function escapeHTML(str) {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}
