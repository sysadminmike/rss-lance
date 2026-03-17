/**
 * Tests for DOM structure expected by the frontend modules.
 *
 * Verifies that index.html contains all the DOM elements that
 * the JS modules expect via getElementById/querySelector.
 *
 * @jest-environment jsdom
 */

const fs = require('fs');
const path = require('path');

// Load the actual index.html
const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');

beforeEach(() => {
  document.documentElement.innerHTML = html;
});

describe('index.html DOM structure', () => {
  // Elements expected by app.js
  test('has add-feed button', () => {
    expect(document.getElementById('btn-add-feed')).not.toBeNull();
  });

  test('has modal overlay', () => {
    expect(document.getElementById('modal-overlay')).not.toBeNull();
  });

  test('has modal cancel button', () => {
    expect(document.getElementById('btn-modal-cancel')).not.toBeNull();
  });

  test('has modal add button', () => {
    expect(document.getElementById('btn-modal-add')).not.toBeNull();
  });

  test('has feed URL input', () => {
    expect(document.getElementById('input-feed-url')).not.toBeNull();
  });

  test('has modal status', () => {
    expect(document.getElementById('modal-status')).not.toBeNull();
  });

  test('has mark-all-read button', () => {
    expect(document.getElementById('btn-mark-all-read')).not.toBeNull();
  });

  // Elements expected by feeds.js
  test('has feed-list container', () => {
    expect(document.getElementById('feed-list')).not.toBeNull();
  });

  // Elements expected by articles.js
  test('has article-list container', () => {
    expect(document.getElementById('article-list')).not.toBeNull();
  });

  test('has current-feed-title', () => {
    expect(document.getElementById('current-feed-title')).not.toBeNull();
  });

  test('has unread-only toggle', () => {
    expect(document.getElementById('unread-only-toggle')).not.toBeNull();
  });

  test('has sort order button', () => {
    expect(document.getElementById('btn-sort-order')).not.toBeNull();
  });



  // Elements expected by reader.js
  test('has reader placeholder', () => {
    expect(document.getElementById('reader-placeholder')).not.toBeNull();
  });

  test('has reader content container', () => {
    expect(document.getElementById('reader-content')).not.toBeNull();
  });

  test('has reader title', () => {
    expect(document.getElementById('reader-title')).not.toBeNull();
  });

  test('has reader meta elements', () => {
    expect(document.getElementById('reader-feed')).not.toBeNull();
    expect(document.getElementById('reader-author')).not.toBeNull();
    expect(document.getElementById('reader-date')).not.toBeNull();
  });

  test('has reader original link', () => {
    expect(document.getElementById('reader-orig-link')).not.toBeNull();
  });

  test('has reader body', () => {
    expect(document.getElementById('reader-body')).not.toBeNull();
  });

  test('has star button', () => {
    expect(document.getElementById('btn-star')).not.toBeNull();
  });

  test('has mark-read button', () => {
    expect(document.getElementById('btn-mark-read')).not.toBeNull();
  });

  // Layout structure
  test('has three-pane layout', () => {
    expect(document.getElementById('sidebar')).not.toBeNull();
    expect(document.getElementById('article-list-pane')).not.toBeNull();
    expect(document.getElementById('reader-pane')).not.toBeNull();
  });

  test('modal starts hidden', () => {
    const overlay = document.getElementById('modal-overlay');
    expect(overlay.classList.contains('hidden')).toBe(true);
  });

  test('reader content starts hidden', () => {
    const content = document.getElementById('reader-content');
    expect(content.classList.contains('hidden')).toBe(true);
  });
});

describe('HTML metadata', () => {
  test('has correct title', () => {
    const titleEl = document.querySelector('title');
    expect(titleEl.textContent).toBe('RSS-Lance');
  });

  test('loads app.js as module', () => {
    const script = document.querySelector('script[type="module"]');
    expect(script).not.toBeNull();
    expect(script.src || script.getAttribute('src')).toContain('app.js');
  });

  test('loads stylesheet', () => {
    const link = document.querySelector('link[rel="stylesheet"]');
    expect(link).not.toBeNull();
    expect(link.href || link.getAttribute('href')).toContain('style.css');
  });
});
