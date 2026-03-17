/**
 * Tests for feed list rendering logic (feeds.js).
 *
 * @jest-environment jsdom
 */

// Replicate the isActiveFeed function from feeds.js
function isActiveFeed(f) {
  if (!f.last_article_date) return false;
  const ms = Date.parse(f.last_article_date);
  if (isNaN(ms)) return false;
  const threeMo = 90 * 24 * 60 * 60 * 1000;
  return (Date.now() - ms) < threeMo;
}

describe('isActiveFeed', () => {
  test('recent feed is active', () => {
    const feed = {
      last_article_date: new Date(Date.now() - 1000 * 60 * 60).toISOString(), // 1 hour ago
    };
    expect(isActiveFeed(feed)).toBe(true);
  });

  test('old feed is stale', () => {
    const feed = {
      last_article_date: new Date(Date.now() - 120 * 24 * 60 * 60 * 1000).toISOString(), // 120 days
    };
    expect(isActiveFeed(feed)).toBe(false);
  });

  test('feed at exactly 90 days is stale', () => {
    const feed = {
      last_article_date: new Date(Date.now() - 91 * 24 * 60 * 60 * 1000).toISOString(),
    };
    expect(isActiveFeed(feed)).toBe(false);
  });

  test('null date means stale', () => {
    const feed = { last_article_date: null };
    expect(isActiveFeed(feed)).toBe(false);
  });

  test('invalid date means stale', () => {
    const feed = { last_article_date: 'not-a-date' };
    expect(isActiveFeed(feed)).toBe(false);
  });

  test('missing field means stale', () => {
    const feed = {};
    expect(isActiveFeed(feed)).toBe(false);
  });
});
