/**
 * Tests for the relativeTime function (articles.js logic).
 *
 * @jest-environment jsdom
 */

// Replicate the relativeTime function from articles.js
function relativeTime(date) {
  const s = Math.floor((Date.now() - date.getTime()) / 1000);
  if (s < 60)    return `${s}s ago`;
  if (s < 3600)  return `${Math.floor(s/60)}m ago`;
  if (s < 86400) return `${Math.floor(s/3600)}h ago`;
  if (s < 86400*30) return `${Math.floor(s/86400)}d ago`;
  return date.toLocaleDateString();
}

describe('relativeTime', () => {
  test('shows seconds for recent times', () => {
    const now = new Date();
    const result = relativeTime(new Date(now.getTime() - 30000));
    expect(result).toMatch(/^\d+s ago$/);
  });

  test('shows minutes', () => {
    const result = relativeTime(new Date(Date.now() - 5 * 60 * 1000));
    expect(result).toMatch(/^\d+m ago$/);
  });

  test('shows hours', () => {
    const result = relativeTime(new Date(Date.now() - 3 * 3600 * 1000));
    expect(result).toMatch(/^\d+h ago$/);
  });

  test('shows days', () => {
    const result = relativeTime(new Date(Date.now() - 5 * 86400 * 1000));
    expect(result).toMatch(/^\d+d ago$/);
  });

  test('shows locale date for old dates', () => {
    const oldDate = new Date('2020-01-01');
    const result = relativeTime(oldDate);
    // Should not contain "ago"
    expect(result).not.toContain('ago');
  });
});
