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
import { hideDuckHuntPage, isDuckHuntVisible } from './duck-hunt.js';

let _settingsVisible = false;

/** Show the settings page, replacing middle + reader panes. */
export async function showSettingsPage() {
  // Dismiss any other active special page
  if (isStatusVisible()) hideStatusPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();
  if (isDuckHuntVisible()) hideDuckHuntPage();

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
        { key: 'log.api.easter_eggs',        label: 'Easter eggs',           desc: 'Duck hunt and other fun stuff' },
        { key: 'log.api.errors',            label: 'Errors',                desc: 'Error responses' },
      ]
    }
  ];

  const retentionVal = settings['log.max_entries'] ?? 10000;
  const retentionMode = settings['log.retention_mode'] ?? 'count';
  const maxAgeDays = settings['log.max_age_days'] ?? 30;

  // Advanced settings grouped by component, with collapsible subgroups
  const advancedGroups = [
    {
      title: 'Fetcher',
      desc: 'Feed fetching, tier management, and fetcher-side compaction. Changes take effect on next restart.',
      subgroups: [
        {
          id: 'fetcher-general',
          title: 'Fetching',
          desc: 'Core fetch cycle settings.',
          fields: [
            { key: 'fetcher.interval_minutes',     label: 'Fetch interval (mins)',           desc: 'Minutes between fetch cycles (daemon mode)', def: 30 },
            { key: 'fetcher.max_concurrent',        label: 'Max concurrent fetches',          desc: 'Number of feeds fetched in parallel', def: 5 },
            { key: 'fetcher.user_agent',            label: 'User-Agent',                      desc: 'HTTP User-Agent header sent when fetching feeds', def: 'RSS-Lance/1.0', type: 'text' },
            { key: 'fetcher.fetch_timeout_secs',    label: 'Fetch timeout (secs)',            desc: 'HTTP request timeout in seconds for fetching feeds', def: 20 },
            { key: 'fetcher.poll_interval_secs',    label: 'Scheduler poll interval (secs)',  desc: 'Seconds between scheduler poll checks in daemon mode', def: 30 },
          ]
        },
        {
          id: 'fetcher-tier-thresholds',
          title: 'Tier Thresholds',
          desc: 'Days of inactivity before a feed is downgraded to a slower tier.',
          fields: [
            { key: 'tier.threshold.active',  label: 'Active (days)',   desc: 'Days without articles before downgrading from active to slowing', def: 3 },
            { key: 'tier.threshold.slowing', label: 'Slowing (days)',  desc: 'Days without articles before downgrading from slowing to quiet', def: 14 },
            { key: 'tier.threshold.quiet',   label: 'Quiet (days)',    desc: 'Days without articles before downgrading from quiet to dormant', def: 60 },
            { key: 'tier.threshold.dormant', label: 'Dormant (days)',  desc: 'Days without articles before downgrading from dormant to dead', def: 180 },
          ]
        },
        {
          id: 'fetcher-tier-intervals',
          title: 'Tier Intervals',
          desc: 'How often feeds in each tier are fetched.',
          fields: [
            { key: 'tier.interval.active',   label: 'Active (mins)',   desc: 'Minutes between fetches for active feeds', def: 30 },
            { key: 'tier.interval.slowing',  label: 'Slowing (mins)',  desc: 'Minutes between fetches for slowing feeds', def: 1440 },
            { key: 'tier.interval.quiet',    label: 'Quiet (mins)',    desc: 'Minutes between fetches for quiet feeds', def: 10080 },
            { key: 'tier.interval.dormant',  label: 'Dormant (mins)',  desc: 'Minutes between fetches for dormant feeds', def: 43200 },
          ]
        },
        {
          id: 'fetcher-compaction',
          title: 'Compaction',
          desc: 'Compact tables after this many data fragments accumulate.',
          fields: [
            { key: 'compaction.articles',       label: 'Articles',       desc: 'Compact after this many fragments (articles table grows fastest)', def: 20 },
            { key: 'compaction.feeds',          label: 'Feeds',          desc: 'Compact after this many fragments', def: 50 },
            { key: 'compaction.categories',     label: 'Categories',     desc: 'Compact after this many fragments', def: 50 },
            { key: 'compaction.pending_feeds',  label: 'Pending feeds',  desc: 'Compact after this many fragments', def: 10 },
            { key: 'compaction.log_fetcher',    label: 'Fetcher logs',   desc: 'Compact fetcher log table after this many fragments', def: 20 },
          ]
        },
      ]
    },
    {
      title: 'Server',
      desc: 'Go API server write-cache, log buffering, and server-side compaction. Changes take effect on next restart.',
      subgroups: [
        {
          id: 'server-general',
          title: 'General',
          desc: 'Table viewer and metrics retention.',
          fields: [
            { key: 'server.table_page_size',  label: 'Table viewer page size',    desc: 'Rows per page in the DB Table Viewer (max 5000)', def: 200 },
            { key: 'stats.retention_minutes', label: 'Stats retention (minutes)', desc: 'How many minutes of server metrics history to keep in memory (charts on Server Status page)', def: 60 },
          ]
        },
        {
          id: 'server-cache',
          title: 'Write Cache',
          desc: 'Batches article read/star updates before flushing to Lance.',
          fields: [
            { key: 'cache.flush_threshold',     label: 'Flush threshold',     desc: 'Flush article read/star cache after N changes', def: 20 },
            { key: 'cache.flush_interval_secs', label: 'Flush interval (secs)', desc: 'Flush cache after N seconds (whichever comes first)', def: 120 },
          ]
        },
        {
          id: 'server-log-buffer',
          title: 'Log Buffer',
          desc: 'Batches API log entries before flushing to Lance.',
          fields: [
            { key: 'log_buffer.flush_threshold',     label: 'Flush threshold',     desc: 'Flush buffered log entries after N entries', def: 20 },
            { key: 'log_buffer.flush_interval_secs', label: 'Flush interval (secs)', desc: 'Flush buffered log entries after N seconds', def: 30 },
          ]
        },
        {
          id: 'server-compaction',
          title: 'Compaction',
          desc: 'Compact server-side log tables after this many fragments.',
          fields: [
            { key: 'compaction.log_api', label: 'API logs', desc: 'Compact API log table after this many fragments', def: 20 },
          ]
        },
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

      ${logGroups.map(group => {
        const allOn = group.options.every(opt => settings[opt.key] !== false);
        const allOff = group.options.every(opt => settings[opt.key] === false);
        const mode = allOn ? 'on' : allOff ? 'off' : 'custom';
        const stateLabel = mode === 'on' ? 'All On' : mode === 'off' ? 'All Off' : 'Custom';
        return `
      <div class="settings-page-section log-section" data-log-group="${group.id}" data-mode="${mode}">
        <div class="log-group-header" data-group="${group.id}">
          <span class="log-group-arrow">\u25B6</span>
          <div class="log-group-title-area">
            <h2>${group.title}</h2>
            <p class="settings-page-hint">${group.desc}</p>
          </div>
          <div class="log-group-controls">
            <span class="log-group-state-label">${stateLabel}</span>
          </div>
        </div>
        <div class="log-options" data-group-body="${group.id}" style="display:none">
          <div class="log-mode-control" data-group="${group.id}">
            <button class="log-mode-btn${mode === 'off' ? ' active' : ''}" data-mode="off">All Off</button>
            <button class="log-mode-btn${mode === 'custom' ? ' active' : ''}" data-mode="custom">Custom</button>
            <button class="log-mode-btn${mode === 'on' ? ' active' : ''}" data-mode="on">All On</button>
          </div>
          <div class="log-options-list${mode !== 'custom' ? ' options-locked' : ''}">
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
      </div>
      `;
      }).join('')}

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

      <div class="settings-page-section">
        <h2>Offline Mode</h2>
        <p class="settings-page-hint">
          Cache data locally so the app keeps working when Lance storage is unreachable.
          The local DuckDB cache is always active.
        </p>
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">Snapshot interval (minutes)</span>
            <span class="log-option-desc">How often to copy recent data into the local cache</span>
          </div>
          <input type="number" id="sp-offline-interval" class="settings-page-input settings-page-input-sm"
            value="${settings['offline_snapshot_interval_mins'] ?? 10}" min="1" max="1440" step="1">
        </div>
        <div class="log-option-row">
          <div class="log-option-info">
            <span class="log-option-label">Article cache window (days)</span>
            <span class="log-option-desc">Cache articles updated within this many days</span>
          </div>
          <input type="number" id="sp-offline-days" class="settings-page-input settings-page-input-sm"
            value="${settings['offline_article_days'] ?? 7}" min="1" max="365" step="1">
        </div>
        <div class="settings-page-actions" style="margin-top: 8px">
          <button id="btn-save-offline" class="settings-page-btn primary">Save Offline Settings</button>
          <span id="offline-save-status" class="settings-page-status"></span>
        </div>
      </div>

      ${advancedGroups.map(group => `
      <div class="settings-page-section">
        <h2>${group.title}</h2>
        <p class="settings-page-hint">${group.desc}</p>
        ${group.subgroups.map(sg => `
        <div class="adv-subgroup" data-adv-group="${sg.id}">
          <div class="log-group-header" data-adv-header="${sg.id}">
            <span class="log-group-arrow">\u25B6</span>
            <div class="log-group-title-area">
              <h2>${sg.title}</h2>
              <p class="settings-page-hint">${sg.desc}</p>
            </div>
          </div>
          <div class="adv-subgroup-body" data-adv-body="${sg.id}" style="display:none">
          ${sg.fields.map(f => `
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

  // ---- Collapsible log groups ----
  const _collapsedLogGroups = {};
  container.querySelectorAll('.log-group-header').forEach(header => {
    const groupId = header.dataset.group;
    const body = container.querySelector(`[data-group-body="${groupId}"]`);
    if (!body) return;

    header.addEventListener('click', (e) => {
      if (e.target.closest('.toggle-switch')) return;
      _collapsedLogGroups[groupId] = !_collapsedLogGroups[groupId];
      header.classList.toggle('expanded', !_collapsedLogGroups[groupId]);
      body.style.display = _collapsedLogGroups[groupId] ? 'none' : '';
    });

    // Start collapsed
    _collapsedLogGroups[groupId] = true;
  });

  // ---- Collapsible advanced subgroups ----
  const _collapsedAdvGroups = {};
  container.querySelectorAll('[data-adv-header]').forEach(header => {
    const sgId = header.dataset.advHeader;
    const body = container.querySelector(`[data-adv-body="${sgId}"]`);
    if (!body) return;

    header.addEventListener('click', () => {
      _collapsedAdvGroups[sgId] = !_collapsedAdvGroups[sgId];
      header.classList.toggle('expanded', !_collapsedAdvGroups[sgId]);
      body.style.display = _collapsedAdvGroups[sgId] ? 'none' : '';
    });

    // Start collapsed
    _collapsedAdvGroups[sgId] = true;
  });

  // ---- Helper: sync mode state across a log group ----
  function _syncGroupMode(section, newMode) {
    section.dataset.mode = newMode;
    const optInputs = section.querySelectorAll('.log-options-list input[data-log-key]');
    const optList = section.querySelector('.log-options-list');
    const stateLabel = section.closest('.log-section')
      ? section.querySelector('.log-group-state-label') || section.closest('.log-section').querySelector('.log-group-state-label')
      : null;
    // Update mode buttons
    section.querySelectorAll('.log-mode-btn').forEach(b => {
      b.classList.toggle('active', b.dataset.mode === newMode);
    });
    if (newMode === 'off') {
      optInputs.forEach(inp => { inp.checked = false; });
      if (optList) optList.classList.add('options-locked');
      if (stateLabel) stateLabel.textContent = 'All Off';
    } else if (newMode === 'on') {
      optInputs.forEach(inp => { inp.checked = true; });
      if (optList) optList.classList.add('options-locked');
      if (stateLabel) stateLabel.textContent = 'All On';
    } else {
      if (optList) optList.classList.remove('options-locked');
      if (stateLabel) stateLabel.textContent = 'Custom';
    }
  }

  // ---- Mode control buttons (All Off / Custom / All On) ----
  container.querySelectorAll('.log-mode-control').forEach(ctrl => {
    ctrl.addEventListener('click', (e) => {
      const btn = e.target.closest('.log-mode-btn');
      if (!btn) return;
      const section = ctrl.closest('[data-log-group]');
      if (!section) return;
      _syncGroupMode(section, btn.dataset.mode);
    });
  });

  // ---- Sync mode label when individual toggles change ----
  container.querySelectorAll('.log-options-list input[data-log-key]').forEach(inp => {
    inp.addEventListener('change', () => {
      const section = inp.closest('[data-log-group]');
      if (!section) return;
      const optInputs = section.querySelectorAll('.log-options-list input[data-log-key]');
      const allOn = Array.from(optInputs).every(i => i.checked);
      const allOff = Array.from(optInputs).every(i => !i.checked);
      const mode = allOn ? 'on' : allOff ? 'off' : 'custom';
      _syncGroupMode(section, mode);
    });
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

  // ---- Save offline settings ----
  const btnSaveOffline = document.getElementById('btn-save-offline');
  const offlineStatus = document.getElementById('offline-save-status');

  if (btnSaveOffline) {
    btnSaveOffline.addEventListener('click', async () => {
      btnSaveOffline.disabled = true;
      offlineStatus.textContent = 'Saving...';
      offlineStatus.className = 'settings-page-status';

      const interval = parseInt(document.getElementById('sp-offline-interval').value, 10) || 10;
      const days = parseInt(document.getElementById('sp-offline-days').value, 10) || 7;

      try {
        await apiFetch('/api/settings', {
          method: 'PUT',
          body: JSON.stringify({
            offline_snapshot_interval_mins: interval,
            offline_article_days: days,
          }),
        });
        offlineStatus.textContent = 'Saved! Restart server to apply.';
        offlineStatus.className = 'settings-page-status success';
      } catch (e) {
        offlineStatus.textContent = `Error: ${e.message}`;
        offlineStatus.className = 'settings-page-status error';
      } finally {
        btnSaveOffline.disabled = false;
      }
    });
  }

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
