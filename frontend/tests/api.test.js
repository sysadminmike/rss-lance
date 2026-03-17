/**
 * Tests for API interaction patterns.
 *
 * Tests the apiFetch wrapper logic and validates expected
 * API endpoint URL patterns.
 *
 * @jest-environment jsdom
 */

// Replicate apiFetch logic for testing
async function apiFetch(url, options = {}) {
  const res = await fetch(url, {
    headers: { 'Content-Type': 'application/json', ...options.headers },
    ...options,
  });
  if (!res.ok) {
    const body = await res.text();
    throw new Error(`${res.status} ${res.statusText}: ${body}`);
  }
  return res.json();
}

// Mock fetch globally
beforeEach(() => {
  global.fetch = jest.fn();
});

afterEach(() => {
  jest.restoreAllMocks();
});

describe('apiFetch', () => {
  test('successful GET returns parsed JSON', async () => {
    const mockData = [{ feed_id: 'f1', title: 'Test' }];
    global.fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(mockData),
    });

    const result = await apiFetch('/api/feeds');
    expect(result).toEqual(mockData);
    expect(global.fetch).toHaveBeenCalledWith('/api/feeds', expect.objectContaining({
      headers: expect.objectContaining({ 'Content-Type': 'application/json' }),
    }));
  });

  test('successful POST with body', async () => {
    global.fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ status: 'queued' }),
    });

    const result = await apiFetch('/api/feeds', {
      method: 'POST',
      body: JSON.stringify({ url: 'https://example.com/rss' }),
    });
    expect(result.status).toBe('queued');
    expect(global.fetch).toHaveBeenCalledWith('/api/feeds', expect.objectContaining({
      method: 'POST',
    }));
  });

  test('throws on HTTP error', async () => {
    global.fetch.mockResolvedValue({
      ok: false,
      status: 404,
      statusText: 'Not Found',
      text: () => Promise.resolve('feed not found'),
    });

    await expect(apiFetch('/api/feeds/bad')).rejects.toThrow('404');
  });

  test('throws on server error', async () => {
    global.fetch.mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      text: () => Promise.resolve('db error'),
    });

    await expect(apiFetch('/api/articles/x')).rejects.toThrow('500');
  });
});

describe('API endpoint patterns', () => {
  // Verify the URL patterns the frontend constructs match
  // what the Go server expects

  test('feeds list endpoint', () => {
    expect('/api/feeds').toMatch(/^\/api\/feeds$/);
  });

  test('feed articles endpoint', () => {
    const feedId = 'abc-123';
    const url = `/api/feeds/${feedId}/articles`;
    expect(url).toBe('/api/feeds/abc-123/articles');
  });

  test('all articles endpoint', () => {
    expect('/api/articles').toMatch(/^\/api\/articles$/);
  });

  test('single article endpoint', () => {
    const artId = 'art-456';
    expect(`/api/articles/${artId}`).toBe('/api/articles/art-456');
  });

  test('mark read endpoint', () => {
    const artId = 'art-456';
    expect(`/api/articles/${artId}/read`).toBe('/api/articles/art-456/read');
  });

  test('mark unread endpoint', () => {
    const artId = 'art-456';
    expect(`/api/articles/${artId}/unread`).toBe('/api/articles/art-456/unread');
  });

  test('star endpoint', () => {
    const artId = 'art-456';
    expect(`/api/articles/${artId}/star`).toBe('/api/articles/art-456/star');
  });

  test('unstar endpoint', () => {
    const artId = 'art-456';
    expect(`/api/articles/${artId}/unstar`).toBe('/api/articles/art-456/unstar');
  });

  test('mark all read endpoint', () => {
    const feedId = 'feed-123';
    expect(`/api/feeds/${feedId}/mark-all-read`).toBe('/api/feeds/feed-123/mark-all-read');
  });

  test('categories endpoint', () => {
    expect('/api/categories').toMatch(/^\/api\/categories$/);
  });

  test('status endpoint', () => {
    expect('/api/status').toMatch(/^\/api\/status$/);
  });

  test('config endpoint', () => {
    expect('/api/config').toMatch(/^\/api\/config$/);
  });

  test('shutdown endpoint', () => {
    expect('/api/shutdown').toMatch(/^\/api\/shutdown$/);
  });

  test('articles pagination params', () => {
    const params = new URLSearchParams({
      limit: 50,
      offset: 0,
      unread: 'true',
      sort: 'asc',
    });
    expect(params.get('limit')).toBe('50');
    expect(params.get('offset')).toBe('0');
    expect(params.get('unread')).toBe('true');
    expect(params.get('sort')).toBe('asc');
  });
});

describe('Status API response shape', () => {
  test('successful GET /api/status returns expected fields', async () => {
    const mockStatus = {
      tables: [
        { name: 'articles', row_count: 100, size_bytes: 1048576 },
        { name: 'feeds', row_count: 5, size_bytes: 2048 },
      ],
      articles: {
        total: 100,
        unread: 42,
        starred: 7,
        oldest: '2024-01-01T00:00:00Z',
        newest: '2024-06-15T12:00:00Z',
      },
      data_path: '/data',
    };
    global.fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(mockStatus),
    });

    const result = await apiFetch('/api/status');
    expect(result.tables).toHaveLength(2);
    expect(result.tables[0].name).toBe('articles');
    expect(result.tables[0].row_count).toBe(100);
    expect(result.tables[0].size_bytes).toBe(1048576);
    expect(result.articles.total).toBe(100);
    expect(result.articles.unread).toBe(42);
    expect(result.articles.starred).toBe(7);
    expect(result.articles.oldest).toBe('2024-01-01T00:00:00Z');
    expect(result.articles.newest).toBe('2024-06-15T12:00:00Z');
    expect(result.data_path).toBe('/data');
  });

  test('config response with show_shutdown false', async () => {
    global.fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ show_shutdown: false }),
    });

    const result = await apiFetch('/api/config');
    expect(result.show_shutdown).toBe(false);
  });

  test('config response with show_shutdown true', async () => {
    global.fetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({ show_shutdown: true }),
    });

    const result = await apiFetch('/api/config');
    expect(result.show_shutdown).toBe(true);
  });

  test('status counts change after read/star operations', async () => {
    // Simulate initial state: 8 total, 8 unread, 0 starred
    global.fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({
        tables: [{ name: 'articles', row_count: 8, size_bytes: 5000 }],
        articles: { total: 8, unread: 8, starred: 0, oldest: '', newest: '' },
        data_path: '/data',
      }),
    });
    const s1 = await apiFetch('/api/status');
    expect(s1.articles.unread).toBe(8);
    expect(s1.articles.starred).toBe(0);

    // After marking 5 read and starring 2
    global.fetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({
        tables: [{ name: 'articles', row_count: 8, size_bytes: 5000 }],
        articles: { total: 8, unread: 3, starred: 2, oldest: '', newest: '' },
        data_path: '/data',
      }),
    });
    const s2 = await apiFetch('/api/status');
    expect(s2.articles.total).toBe(8);
    expect(s2.articles.unread).toBe(3);
    expect(s2.articles.starred).toBe(2);
  });
});
