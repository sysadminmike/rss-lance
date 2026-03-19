/**
 * Tests for HTML content sanitisation (reader.js logic).
 *
 * The sanitise function is extracted and tested independently since
 * the reader module depends on DOM elements. We replicate the logic
 * here to test it in isolation.
 *
 * @jest-environment jsdom
 */

// Replicate the sanitise function from reader.js
const SOCIAL_DOMAINS = [
  'facebook.com', 'twitter.com', 'x.com', 'linkedin.com',
  'pinterest.com', 'reddit.com', 'tumblr.com', 'whatsapp.com',
  'wa.me', 't.me', 'threads.net', 'instagram.com', 'buffer.com',
  'getpocket.com', 'stumbleupon.com', 'digg.com', 'flipboard.com',
  'mix.com',
];

const _SOCIAL_RE = new RegExp(
  '<a\\b[^>]*\\bhref=["\'][^"\']*(?:'
  + SOCIAL_DOMAINS.map(d => d.replace(/\./g, '\\.')).join('|')
  + ')[^"\']*["\'][^>]*>[\\s\\S]*?</a>',
  'gi'
);

const _SHARE_CLASS_RE = /\b(share|sharing|social[-_]?icon|social[-_]?link|social[-_]?media|social[-_]?button|social[-_]?bar|sharedaddy|sd-sharing|jetpack[-_]sharing|addtoany|a2a_kit|post-share|entry-share|wp[-_]share|ssba|ssbp|heateor|sumo[-_]share|mashsb|dpsp|novashare|shareaholic|sociable|post-socials)\b/i;

const _SHARE_HEADING_RE = /^\s*(share\s*(this)?|share\s*on|spread\s*the\s*word|tell\s*your\s*friends)\s*:?\s*$/i;

function _stripSocialContainers(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  root.querySelectorAll('*').forEach(el => {
    const cls = el.getAttribute('class') || '';
    const id  = el.getAttribute('id') || '';
    if (_SHARE_CLASS_RE.test(cls) || _SHARE_CLASS_RE.test(id)) {
      el.remove();
    }
  });

  root.querySelectorAll('ul, ol').forEach(list => {
    const items = list.querySelectorAll(':scope > li');
    if (items.length === 0) return;
    const allSocial = Array.from(items).every(li => {
      const text = li.textContent.trim();
      const links = li.querySelectorAll('a');
      if (!text && links.length === 0) return true;
      if (links.length === 0) return false;
      return Array.from(links).every(a => {
        const href = a.getAttribute('href') || '';
        return SOCIAL_DOMAINS.some(d => href.includes(d));
      });
    });
    if (allSocial) list.remove();
  });

  root.querySelectorAll('h1, h2, h3, h4, h5, h6').forEach(h => {
    if (_SHARE_HEADING_RE.test(h.textContent)) {
      const next = h.nextElementSibling;
      if (next && !next.textContent.trim() && !next.querySelector('img, video, picture, canvas')) {
        next.remove();
      }
      h.remove();
    }
  });

  root.querySelectorAll('div, section, aside').forEach(el => {
    if (!el.textContent.trim() && !el.querySelector('img, video, picture, canvas, svg')) {
      el.remove();
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  const result = div.innerHTML;

  const textBefore = _textLen(html);
  const textAfter  = _textLen(result);
  const removed    = textBefore - textAfter;
  if (removed > 200) return html;

  return result;
}

function _textLen(html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  return (t.content.textContent || '').trim().length;
}

const _TRACKING_PARAMS = new Set([
  'utm_source','utm_medium','utm_campaign','utm_term','utm_content',
  'utm_id','utm_source_platform','utm_creative_format','utm_marketing_tactic',
  'gclid','gclsrc','dclid','gbraid','wbraid',
  'fbclid','fb_action_ids','fb_action_types','fb_ref','fb_source',
  'msclkid',
  '_hsenc','_hsmi','__hssc','__hstc','__hsfp',
  'mc_cid','mc_eid',
  'twclid','li_fat_id','s_cid','mkt_tok','vero_id','__s',
  'obOrigUrl','ob_click_id','taboolaclickid',
  'guccounter','guce_referrer','guce_referrer_sig',
]);
const _TRACKING_PREFIXES = ['utm_'];

function _isTrackingParam(name) {
  const lower = name.toLowerCase();
  if (_TRACKING_PARAMS.has(lower)) return true;
  return _TRACKING_PREFIXES.some(p => lower.startsWith(p));
}

function _stripTrackingParams(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;
  root.querySelectorAll('a[href]').forEach(a => {
    try {
      const u = new URL(a.getAttribute('href'), 'http://dummy');
      let changed = false;
      for (const key of [...u.searchParams.keys()]) {
        if (_isTrackingParam(key)) { u.searchParams.delete(key); changed = true; }
      }
      if (changed) {
        const orig = a.getAttribute('href');
        if (!/^https?:\/\//i.test(orig)) {
          a.setAttribute('href', u.pathname + u.search + u.hash);
        } else {
          a.setAttribute('href', u.href);
        }
      }
    } catch (_) { /* malformed URL */ }
  });
  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

function _stripEmptyElements(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  const _hasMedia = el => el.querySelector('img, video, picture, canvas, svg, audio, iframe');
  const _isEmpty = el => !el.textContent.replace(/[\s\u00A0]/g, '') && !_hasMedia(el);

  root.querySelectorAll('p, span, a, li, figcaption').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });
  root.querySelectorAll('figure').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });
  root.querySelectorAll('div, section, aside, header, footer, nav').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });
  root.querySelectorAll('ul, ol').forEach(el => {
    if (_isEmpty(el)) el.remove();
  });
  root.querySelectorAll('br').forEach(br => {
    let count = 1;
    while (br.nextSibling) {
      const sib = br.nextSibling;
      if (sib.nodeType === Node.ELEMENT_NODE && sib.tagName === 'BR') {
        count++;
        sib.remove();
      } else if (sib.nodeType === Node.TEXT_NODE && !sib.textContent.trim()) {
        sib.remove();
      } else {
        break;
      }
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

function _domSanitise(html) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  const root = tpl.content;

  const dangerousTags = 'script,style,iframe,object,embed,applet,form,base,meta,link,svg';
  root.querySelectorAll(dangerousTags).forEach(el => el.remove());

  root.querySelectorAll('*').forEach(el => {
    for (const attr of [...el.attributes]) {
      const name = attr.name.toLowerCase();
      if (name.startsWith('on')) {
        el.removeAttribute(attr.name);
      }
    }
    for (const attr of ['href', 'src', 'action', 'formaction', 'xlink:href']) {
      const val = el.getAttribute(attr);
      if (val) {
        const trimmed = val.replace(/[\s\x00-\x1f]/g, '').toLowerCase();
        if (/^(javascript|data|vbscript):/i.test(trimmed)) {
          el.removeAttribute(attr);
        }
      }
    }
  });

  const div = document.createElement('div');
  div.appendChild(root.cloneNode(true));
  return div.innerHTML;
}

function sanitise(html) {
  const cleaned = html
    .replace(/<script[\s\S]*?<\/script>/gi, '')
    .replace(/<style[\s\S]*?<\/style>/gi, '')
    .replace(/<iframe[\s\S]*?>/gi, '')
    .replace(/\son\w+=["'][^"']*["']/gi, '')
    .replace(_SOCIAL_RE, '');
  const deSocialed = _stripSocialContainers(cleaned);
  const deTracked = _stripTrackingParams(deSocialed);
  const deEmpty = _stripEmptyElements(deTracked);
  return _domSanitise(deEmpty);
}

describe('sanitise', () => {
  test('removes script tags', () => {
    const input = '<p>Hello</p><script>alert("xss")</script><p>World</p>';
    expect(sanitise(input)).toBe('<p>Hello</p><p>World</p>');
  });

  test('removes script tags with attributes', () => {
    const input = '<script type="text/javascript">evil()</script>';
    expect(sanitise(input)).toBe('');
  });

  test('removes style tags', () => {
    const input = '<style>body{display:none}</style><p>Visible</p>';
    expect(sanitise(input)).toBe('<p>Visible</p>');
  });

  test('removes iframe tags', () => {
    const input = '<iframe src="https://evil.com"></iframe><p>Safe</p>';
    // iframe opening tag is removed; closing tag may remain (harmless)
    const result = sanitise(input);
    expect(result).not.toContain('<iframe');
  });

  test('strips event handlers', () => {
    const input = '<img src="x" onerror="alert(1)" />';
    const result = sanitise(input);
    expect(result).not.toContain('onerror');
    expect(result).toContain('<img');
  });

  test('strips onclick handler', () => {
    const input = '<a href="#" onclick="evil()">Click</a>';
    const result = sanitise(input);
    expect(result).not.toContain('onclick');
    expect(result).toContain('Click');
  });

  test('preserves safe HTML', () => {
    const input = '<p>This is <strong>safe</strong> content with <a href="https://example.com">links</a>.</p>';
    expect(sanitise(input)).toBe(input);
  });

  test('handles empty string', () => {
    expect(sanitise('')).toBe('');
  });

  test('handles multiple script/style blocks', () => {
    const input = '<script>a()</script><p>Ok</p><style>.x{}</style><script>b()</script>';
    expect(sanitise(input)).toBe('<p>Ok</p>');
  });

  test('case insensitive removal', () => {
    const input = '<SCRIPT>alert(1)</SCRIPT><p>Safe</p>';
    expect(sanitise(input)).toBe('<p>Safe</p>');
  });

  test('removes Facebook share link with SVG', () => {
    const input = '<p>Article</p><a href="https://www.facebook.com/sharer.php?u=https%3A%2F%2Fexample.com"><svg viewBox="0 0 24 24"><path d="M9 8h-3v4h3"></path></svg></a>';
    expect(sanitise(input)).toBe('<p>Article</p>');
  });

  test('removes Twitter/X share link', () => {
    const input = '<p>Content</p><a href="https://twitter.com/intent/tweet?url=foo">Tweet</a><a href="https://x.com/share?url=bar">Post</a>';
    expect(sanitise(input)).toBe('<p>Content</p>');
  });

  test('removes LinkedIn share link', () => {
    const input = '<a href="https://www.linkedin.com/shareArticle?url=foo">Share</a><p>Text</p>';
    expect(sanitise(input)).toBe('<p>Text</p>');
  });

  test('removes multiple social links', () => {
    const input = '<p>Read more</p><a href="https://facebook.com/sharer">FB</a><a href="https://pinterest.com/pin/create">Pin</a><a href="https://reddit.com/submit">Reddit</a>';
    expect(sanitise(input)).toBe('<p>Read more</p>');
  });

  test('preserves non-social links', () => {
    const input = '<p>Visit <a href="https://example.com">our site</a></p>';
    expect(sanitise(input)).toBe(input);
  });

  // ── Container-level social removal ────────────────────────────────────────

  test('removes ul.social-icons container with social list items', () => {
    const input = '<p>Article text</p>'
      + '<div class="margin-bottom-2"><h2 class="heading-14">Share</h2></div>'
      + '<div class="padding-bottom-2">'
      + '<ul class="social-icons social-icons-round">'
      + '<li class="social-icon social-icon-x"><a href="https://x.com/share">X</a></li>'
      + '<li class="social-icon social-icon-facebook"><a href="https://facebook.com/sharer">FB</a></li>'
      + '</ul></div>';
    const result = sanitise(input);
    expect(result).not.toContain('social-icon');
    expect(result).not.toContain('Share');
    expect(result).toContain('Article text');
  });

  test('removes WordPress Jetpack sharing div', () => {
    const input = '<p>Post content</p>'
      + '<div class="sharedaddy sd-sharing-enabled">'
      + '<div class="sd-sharing"><span>Share this:</span></div></div>';
    const result = sanitise(input);
    expect(result).not.toContain('sharedaddy');
    expect(result).not.toContain('Share this');
    expect(result).toContain('Post content');
  });

  test('removes AddToAny sharing container', () => {
    const input = '<p>Good article</p>'
      + '<div class="a2a_kit a2a_default_style">'
      + '<a class="a2a_button_facebook">Facebook</a>'
      + '<a class="a2a_button_twitter">Twitter</a></div>';
    const result = sanitise(input);
    expect(result).not.toContain('a2a_kit');
    expect(result).toContain('Good article');
  });

  test('removes div with id containing "share"', () => {
    const input = '<p>Content</p><div id="post-share-buttons"><a href="#">Share</a></div>';
    const result = sanitise(input);
    expect(result).not.toContain('post-share-buttons');
    expect(result).toContain('Content');
  });

  test('removes "Share this" heading and empty sibling', () => {
    const input = '<p>Body</p><h3>Share this</h3><div class="empty-wrapper"></div>';
    const result = sanitise(input);
    expect(result).not.toContain('Share this');
    expect(result).toContain('Body');
  });

  test('removes social list where links already stripped by regex', () => {
    const input = '<p>Text</p>'
      + '<ul><li><a href="https://twitter.com/share">Tweet</a></li>'
      + '<li><a href="https://facebook.com/sharer">FB</a></li></ul>';
    const result = sanitise(input);
    expect(result).not.toContain('<ul>');
    expect(result).toContain('Text');
  });

  test('preserves non-social lists', () => {
    const input = '<ul><li><a href="https://example.com">Link 1</a></li>'
      + '<li><a href="https://other.com">Link 2</a></li></ul>';
    const result = sanitise(input);
    expect(result).toContain('Link 1');
    expect(result).toContain('Link 2');
  });

  test('preserves content-bearing divs', () => {
    const input = '<div class="article-body"><p>Important stuff</p></div>';
    expect(sanitise(input)).toContain('Important stuff');
  });

  test('bails out if container removal would strip too much text', () => {
    // A div with class "share" but containing a long paragraph of real content
    const longText = 'A'.repeat(250);
    const input = '<p>Intro</p><div class="share">' + longText + '</div>';
    const result = sanitise(input);
    // Safety guard should preserve the long text
    expect(result).toContain(longText);
  });

  test('still strips containers with little text', () => {
    const input = '<p>Article body</p><div class="share"><span>Share this</span></div>';
    const result = sanitise(input);
    expect(result).not.toContain('Share this');
    expect(result).toContain('Article body');
  });

  test('strips utm_source from link href', () => {
    const input = '<a href="https://example.com/page?utm_source=twitter&page=1">Link</a>';
    const result = sanitise(input);
    expect(result).not.toContain('utm_source');
    expect(result).toContain('page=1');
    expect(result).toContain('Link');
  });

  test('strips fbclid from link href', () => {
    const input = '<a href="https://example.com/?fbclid=abc123">Link</a>';
    const result = sanitise(input);
    expect(result).not.toContain('fbclid');
    expect(result).toContain('Link');
  });

  test('strips gclid from link href', () => {
    const input = '<a href="https://example.com/?gclid=xyz">Link</a>';
    const result = sanitise(input);
    expect(result).not.toContain('gclid');
  });

  test('preserves non-tracking query params', () => {
    const input = '<a href="https://example.com/search?q=hello&page=2">Search</a>';
    const result = sanitise(input);
    expect(result).toContain('q=hello');
    expect(result).toContain('page=2');
  });

  test('strips multiple tracking params at once', () => {
    const input = '<a href="https://example.com/?utm_source=x&utm_medium=y&fbclid=z&id=42">Link</a>';
    const result = sanitise(input);
    expect(result).not.toContain('utm_source');
    expect(result).not.toContain('utm_medium');
    expect(result).not.toContain('fbclid');
    expect(result).toContain('id=42');
  });

  test('leaves links without query params unchanged', () => {
    const input = '<a href="https://example.com/article">Read more</a>';
    const result = sanitise(input);
    expect(result).toContain('https://example.com/article');
    expect(result).toContain('Read more');
  });
});

// -- DOM-based XSS sanitiser tests (_domSanitise) --------------------------------

describe('_domSanitise', () => {
  test('removes SVG elements', () => {
    const input = '<p>Text</p><svg onload="alert(1)"><circle r="10"></circle></svg>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<svg');
    expect(result).toContain('Text');
  });

  test('removes object tags', () => {
    const input = '<p>Safe</p><object data="evil.swf"><param name="x" value="y"></object>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<object');
    expect(result).toContain('Safe');
  });

  test('removes embed tags', () => {
    const input = '<p>Content</p><embed src="evil.swf" type="application/x-shockwave-flash">';
    const result = _domSanitise(input);
    expect(result).not.toContain('<embed');
    expect(result).toContain('Content');
  });

  test('removes applet tags', () => {
    const input = '<p>Article</p><applet code="Evil.class"></applet>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<applet');
    expect(result).toContain('Article');
  });

  test('removes form elements', () => {
    const input = '<p>Text</p><form action="https://evil.com"><input type="hidden" name="csrf"></form>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<form');
    expect(result).not.toContain('<input');
    expect(result).toContain('Text');
  });

  test('removes base tags', () => {
    const input = '<base href="https://evil.com"><p>Content</p>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<base');
    expect(result).toContain('Content');
  });

  test('removes meta tags', () => {
    const input = '<meta http-equiv="refresh" content="0;url=https://evil.com"><p>Text</p>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<meta');
    expect(result).toContain('Text');
  });

  test('removes link tags', () => {
    const input = '<link rel="stylesheet" href="https://evil.com/style.css"><p>Safe</p>';
    const result = _domSanitise(input);
    expect(result).not.toContain('<link');
    expect(result).toContain('Safe');
  });

  test('strips all on* event handler attributes via DOM walk', () => {
    const input = '<div onclick="alert(1)" onmouseover="evil()" onfocus="bad()">Content</div>';
    const result = _domSanitise(input);
    expect(result).not.toContain('onclick');
    expect(result).not.toContain('onmouseover');
    expect(result).not.toContain('onfocus');
    expect(result).toContain('Content');
  });

  test('blocks javascript: URIs in href', () => {
    const input = '<a href="javascript:alert(document.cookie)">Click</a>';
    const result = _domSanitise(input);
    expect(result).not.toContain('javascript:');
    expect(result).toContain('Click');
  });

  test('blocks javascript: URIs with whitespace evasion', () => {
    const input = '<a href="java\tscript:alert(1)">Click</a>';
    const result = _domSanitise(input);
    expect(result).not.toContain('javascript:');
    expect(result).toContain('Click');
  });

  test('blocks data: URIs in src', () => {
    const input = '<img src="data:image/svg+xml,<svg onload=alert(1)>">';
    const result = _domSanitise(input);
    expect(result).not.toContain('data:');
  });

  test('blocks vbscript: URIs in href', () => {
    const input = '<a href="vbscript:MsgBox(1)">Click</a>';
    const result = _domSanitise(input);
    expect(result).not.toContain('vbscript:');
    expect(result).toContain('Click');
  });

  test('blocks javascript: in formaction', () => {
    const input = '<button formaction="javascript:alert(1)">Submit</button>';
    const result = _domSanitise(input);
    expect(result).not.toContain('javascript:');
    expect(result).toContain('Submit');
  });

  test('preserves safe content unchanged', () => {
    const input = '<p>Safe <a href="https://example.com">link</a> and <img src="https://example.com/img.png"></p>';
    const result = _domSanitise(input);
    expect(result).toContain('https://example.com');
    expect(result).toContain('Safe');
    expect(result).toContain('link');
  });

  test('handles empty string', () => {
    expect(_domSanitise('')).toBe('');
  });
});
