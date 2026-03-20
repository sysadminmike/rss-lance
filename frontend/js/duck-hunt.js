/**
 * RSS-Lance - Shoot the Duck easter egg
 *
 * A DuckDB-themed duck hunt mini-game. The duck flies across the screen
 * and the player clicks to shoot it. Logs hilarious messages to the
 * server log system. Rate-limited to one attempt every 3 minutes.
 */
import { apiFetch } from './app.js';
import { hideStatusPage, isStatusVisible } from './status.js';
import { hideSettingsPage, isSettingsPageVisible } from './settings-page.js';
import { hideLogsPage, isLogsVisible } from './logs-page.js';
import { hideTableViewerPage, isTableViewerVisible } from './table-viewer.js';
import { hideServerStatusPage, isServerStatusVisible } from './server-status-page.js';

let _visible = false;
const COOLDOWN_MS = 3 * 60 * 1000; // 3 minutes
const COOLDOWN_KEY = 'rss-lance-duck-hunt-last';

export async function showDuckHuntPage() {
  if (isStatusVisible()) hideStatusPage();
  if (isSettingsPageVisible()) hideSettingsPage();
  if (isLogsVisible()) hideLogsPage();
  if (isTableViewerVisible()) hideTableViewerPage();
  if (isServerStatusVisible()) hideServerStatusPage();

  const app = document.getElementById('app');
  document.getElementById('article-list-pane').classList.add('hidden');
  document.getElementById('reader-pane').classList.add('hidden');
  document.getElementById('divider-sidebar').classList.add('hidden');
  document.getElementById('divider-list').classList.add('hidden');

  let container = document.getElementById('duck-hunt-page');
  if (!container) {
    container = document.createElement('div');
    container.id = 'duck-hunt-page';
    app.appendChild(container);
  }
  container.classList.remove('hidden');
  _visible = true;

  // Check cooldown
  const lastShot = parseInt(localStorage.getItem(COOLDOWN_KEY) || '0', 10);
  const elapsed = Date.now() - lastShot;
  if (lastShot && elapsed < COOLDOWN_MS) {
    const remaining = Math.ceil((COOLDOWN_MS - elapsed) / 1000);
    const mins = Math.floor(remaining / 60);
    const secs = remaining % 60;
    renderCooldown(container, mins, secs);
    return;
  }

  renderGame(container);
}

export function hideDuckHuntPage() {
  _visible = false;
  const container = document.getElementById('duck-hunt-page');
  if (container) container.classList.add('hidden');

  document.getElementById('article-list-pane').classList.remove('hidden');
  document.getElementById('reader-pane').classList.remove('hidden');
  document.getElementById('divider-sidebar').classList.remove('hidden');
  document.getElementById('divider-list').classList.remove('hidden');

  if (localStorage.getItem('rss-lance-middle-pane') === 'hidden') {
    document.getElementById('app').classList.add('hide-middle-pane');
  }
}

export function isDuckHuntVisible() { return _visible; }

// ── Cooldown screen ───────────────────────────────────────────────────────────

function renderCooldown(container, mins, secs) {
  container.innerHTML = `
    <div class="duck-hunt-inner">
      <h1 class="duck-hunt-title">Shoot the Duck</h1>
      <div class="duck-hunt-cooldown">
        <div class="duck-hunt-cooldown-icon">&#x1F6AB;</div>
        <p>The duck needs time to respawn!</p>
        <p class="duck-hunt-timer">Try again in <strong>${mins}m ${String(secs).padStart(2, '0')}s</strong></p>
        <p class="duck-hunt-hint">DuckDB is recovering from your last attempt...</p>
      </div>
    </div>`;
}

// ── Game rendering ────────────────────────────────────────────────────────────

function renderGame(container) {
  container.innerHTML = `
    <div class="duck-hunt-inner">
      <h1 class="duck-hunt-title">Shoot the Duck</h1>
      <p class="duck-hunt-subtitle">The DuckDB duck has escaped! Click to shoot it before it flies away!</p>
      <div class="duck-hunt-arena" id="duck-hunt-arena">
        <div class="duck-hunt-sky">
          <div class="duck-hunt-cloud" style="top:15%;left:10%"></div>
          <div class="duck-hunt-cloud" style="top:25%;left:55%"></div>
          <div class="duck-hunt-cloud" style="top:10%;left:80%"></div>
        </div>
        <canvas id="duck-hunt-canvas"></canvas>
        <div class="duck-hunt-ground"></div>
      </div>
      <div id="duck-hunt-result" class="duck-hunt-result hidden"></div>
      <p class="duck-hunt-footer">Like rss-lance? Please give it a ⭐ on <a href="https://github.com/sysadminmike/rss-lance" target="_blank" rel="noopener">GitHub</a>!</p>
    </div>`;

  const canvas = document.getElementById('duck-hunt-canvas');
  const arena = document.getElementById('duck-hunt-arena');
  initGame(canvas, arena);
}

// ── Sound effects (Web Audio API -- no external files) ────────────────────────

let _audioCtx = null;

function getAudioCtx() {
  if (!_audioCtx) _audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  if (_audioCtx.state === 'suspended') _audioCtx.resume();
  return _audioCtx;
}

function playGunshot() {
  try {
    const ctx = getAudioCtx();
    const t = ctx.currentTime;

    // White noise burst for the bang
    const bufLen = Math.floor(ctx.sampleRate * 0.15);
    const buf = ctx.createBuffer(1, bufLen, ctx.sampleRate);
    const data = buf.getChannelData(0);
    for (let i = 0; i < bufLen; i++) {
      data[i] = (Math.random() * 2 - 1) * Math.pow(1 - i / bufLen, 3);
    }
    const noise = ctx.createBufferSource();
    noise.buffer = buf;

    // Low-pass filter for a bassy shot sound
    const filter = ctx.createBiquadFilter();
    filter.type = 'lowpass';
    filter.frequency.setValueAtTime(3000, t);
    filter.frequency.exponentialRampToValueAtTime(300, t + 0.12);

    const gain = ctx.createGain();
    gain.gain.setValueAtTime(0.6, t);
    gain.gain.exponentialRampToValueAtTime(0.01, t + 0.15);

    noise.connect(filter);
    filter.connect(gain);
    gain.connect(ctx.destination);
    noise.start(t);
    noise.stop(t + 0.15);

    // Low thud layer
    const osc = ctx.createOscillator();
    osc.type = 'sine';
    osc.frequency.setValueAtTime(150, t);
    osc.frequency.exponentialRampToValueAtTime(30, t + 0.1);
    const oscGain = ctx.createGain();
    oscGain.gain.setValueAtTime(0.5, t);
    oscGain.gain.exponentialRampToValueAtTime(0.01, t + 0.1);
    osc.connect(oscGain);
    oscGain.connect(ctx.destination);
    osc.start(t);
    osc.stop(t + 0.12);
  } catch (_) { /* audio not supported */ }
}

export function playQuack(pitch, duration) {
  try {
    const ctx = getAudioCtx();
    const t = ctx.currentTime;
    const dur = duration || 0.18;
    const base = pitch || 800;

    // Main quack tone -- sawtooth with frequency sweep downward
    const osc = ctx.createOscillator();
    osc.type = 'sawtooth';
    osc.frequency.setValueAtTime(base, t);
    osc.frequency.exponentialRampToValueAtTime(base * 0.55, t + dur * 0.6);
    osc.frequency.setValueAtTime(base * 0.7, t + dur * 0.6);
    osc.frequency.exponentialRampToValueAtTime(base * 0.4, t + dur);

    // Nasal bandpass filter
    const filter = ctx.createBiquadFilter();
    filter.type = 'bandpass';
    filter.frequency.setValueAtTime(base * 1.2, t);
    filter.Q.setValueAtTime(3, t);

    // Volume envelope
    const gain = ctx.createGain();
    gain.gain.setValueAtTime(0, t);
    gain.gain.linearRampToValueAtTime(0.25, t + 0.01);
    gain.gain.setValueAtTime(0.25, t + dur * 0.3);
    gain.gain.linearRampToValueAtTime(0.15, t + dur * 0.7);
    gain.gain.exponentialRampToValueAtTime(0.01, t + dur);

    osc.connect(filter);
    filter.connect(gain);
    gain.connect(ctx.destination);
    osc.start(t);
    osc.stop(t + dur + 0.01);
  } catch (_) { /* audio not supported */ }
}

function playMockingQuack() {
  // Four rapid quacks rising in pitch -- mocking laughter as duck escapes
  playQuack(700, 0.12);
  setTimeout(() => playQuack(850, 0.12), 150);
  setTimeout(() => playQuack(1000, 0.15), 300);
  setTimeout(() => playQuack(1100, 0.2), 480);
}

// ── Game logic ────────────────────────────────────────────────────────────────

function initGame(canvas, arena) {
  const rect = arena.getBoundingClientRect();
  canvas.width = rect.width;
  canvas.height = rect.height - 60; // leave room for ground
  const ctx = canvas.getContext('2d');

  // Duck state
  const duck = {
    x: -60,
    y: canvas.height * 0.3 + Math.random() * canvas.height * 0.3,
    w: 56,
    h: 48,
    speed: 2.5 + Math.random() * 1.5,
    alive: true,
    wingPhase: 0,
    yBase: 0,
    bobPhase: Math.random() * Math.PI * 2,
  };
  duck.yBase = duck.y;

  let gameOver = false;
  let shotFired = false;
  let animFrame = null;
  let hasQuacked = false;
  // Pick a random x position (20-70% of canvas) where the duck will quack
  const quackAtX = canvas.width * (0.2 + Math.random() * 0.5);
  let quackTextTimer = 0;

  // Crosshair cursor
  canvas.style.cursor = 'crosshair';

  function drawDuck(ctx, x, y, w, h, wingPhase, alive) {
    ctx.save();
    ctx.translate(x, y);

    if (!alive) {
      // Falling dead duck - rotate
      ctx.rotate(0.4);
    }

    // Body (yellow DuckDB duck)
    ctx.fillStyle = '#FFC107';
    ctx.beginPath();
    ctx.ellipse(w * 0.45, h * 0.55, w * 0.35, h * 0.3, 0, 0, Math.PI * 2);
    ctx.fill();

    // Head
    ctx.fillStyle = '#FFD54F';
    ctx.beginPath();
    ctx.arc(w * 0.78, h * 0.35, h * 0.22, 0, Math.PI * 2);
    ctx.fill();

    // Eye
    ctx.fillStyle = alive ? '#333' : '#999';
    ctx.beginPath();
    ctx.arc(w * 0.85, h * 0.3, 3, 0, Math.PI * 2);
    ctx.fill();

    if (!alive) {
      // X eyes
      ctx.strokeStyle = '#c00';
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(w * 0.82, h * 0.26);
      ctx.lineTo(w * 0.88, h * 0.34);
      ctx.moveTo(w * 0.88, h * 0.26);
      ctx.lineTo(w * 0.82, h * 0.34);
      ctx.stroke();
    }

    // Beak
    ctx.fillStyle = '#FF8F00';
    ctx.beginPath();
    ctx.moveTo(w * 0.95, h * 0.38);
    ctx.lineTo(w + 8, h * 0.42);
    ctx.lineTo(w * 0.95, h * 0.48);
    ctx.fill();

    // Wing
    const wingAngle = Math.sin(wingPhase) * 0.6;
    ctx.save();
    ctx.translate(w * 0.4, h * 0.4);
    ctx.rotate(wingAngle);
    ctx.fillStyle = '#FFB300';
    ctx.beginPath();
    ctx.ellipse(0, -h * 0.15, w * 0.22, h * 0.18, -0.3, 0, Math.PI * 2);
    ctx.fill();
    ctx.restore();

    // "DB" label on body
    ctx.fillStyle = '#5D4037';
    ctx.font = 'bold 11px monospace';
    ctx.fillText('DB', w * 0.3, h * 0.62);

    ctx.restore();
  }

  function drawExplosion(ctx, x, y) {
    ctx.save();
    ctx.fillStyle = '#ff6600';
    ctx.font = 'bold 32px sans-serif';
    ctx.fillText('BANG!', x - 30, y - 10);

    // Starburst
    const spikes = 8;
    for (let i = 0; i < spikes; i++) {
      const angle = (i / spikes) * Math.PI * 2;
      const len = 20 + Math.random() * 15;
      ctx.strokeStyle = i % 2 === 0 ? '#ff4400' : '#ffaa00';
      ctx.lineWidth = 3;
      ctx.beginPath();
      ctx.moveTo(x + duck.w / 2, y + duck.h / 2);
      ctx.lineTo(
        x + duck.w / 2 + Math.cos(angle) * len,
        y + duck.h / 2 + Math.sin(angle) * len
      );
      ctx.stroke();
    }
    ctx.restore();
  }

  let fallY = 0;
  let missTimeout = null;

  function drawQuackText(ctx, x, y) {
    if (quackTextTimer <= 0) return;
    quackTextTimer--;
    ctx.save();
    ctx.fillStyle = '#fff';
    ctx.font = 'bold 16px sans-serif';
    ctx.fillText('QUACK!', x + 40, y - 8);
    ctx.restore();
  }

  function frame() {
    ctx.clearRect(0, 0, canvas.width, canvas.height);

    if (duck.alive && !gameOver) {
      // Duck flies across the screen with a bobbing motion
      duck.x += duck.speed;
      duck.bobPhase += 0.06;
      duck.y = duck.yBase + Math.sin(duck.bobPhase) * 20;
      duck.wingPhase += 0.25;

      // Random quack when duck crosses the trigger point
      if (!hasQuacked && duck.x >= quackAtX) {
        hasQuacked = true;
        playQuack(800 + Math.random() * 200, 0.2);
        quackTextTimer = 40; // show text for ~40 frames
      }

      drawDuck(ctx, duck.x, duck.y, duck.w, duck.h, duck.wingPhase, true);
      drawQuackText(ctx, duck.x, duck.y);

      // Duck escaped!
      if (duck.x > canvas.width + 80) {
        gameOver = true;
        playMockingQuack();
        onMiss();
        return;
      }
    } else if (!duck.alive) {
      // Dead duck falls
      fallY += 3;
      drawExplosion(ctx, duck.x, duck.y);
      drawDuck(ctx, duck.x, duck.y + fallY, duck.w, duck.h, 0, false);

      if (duck.y + fallY > canvas.height + 60) {
        // Done falling
        return;
      }
    }

    animFrame = requestAnimationFrame(frame);
  }

  function onShot(e) {
    if (gameOver || shotFired) return;

    const canvasRect = canvas.getBoundingClientRect();
    const mx = e.clientX - canvasRect.left;
    const my = e.clientY - canvasRect.top;

    // Play gunshot sound on every click
    playGunshot();

    // Check hit
    const hit = (
      mx >= duck.x && mx <= duck.x + duck.w &&
      my >= duck.y && my <= duck.y + duck.h
    );

    shotFired = true;
    localStorage.setItem(COOLDOWN_KEY, String(Date.now()));

    if (hit) {
      duck.alive = false;
      canvas.style.cursor = 'default';
      onHit();
    }
    // If miss, they used their one click -- let the duck keep flying and escape
  }

  canvas.addEventListener('click', onShot);

  function onHit() {
    gameOver = true;
    const resultDiv = document.getElementById('duck-hunt-result');
    resultDiv.classList.remove('hidden');
    resultDiv.innerHTML = `
      <div class="duck-hunt-hit">
        <div class="duck-hunt-result-icon">&#x1F4A5;</div>
        <h2>DIRECT HIT!</h2>
        <p>The DuckDB duck has been vanquished!</p>
        <p class="duck-hunt-quip">Your queries will run 10x faster now... just kidding.</p>
        <p class="duck-hunt-quip">SELECT * FROM duck WHERE alive = false; -- 1 row returned</p>
      </div>`;
    logShot(true);
  }

  function onMiss() {
    const resultDiv = document.getElementById('duck-hunt-result');
    resultDiv.classList.remove('hidden');
    resultDiv.innerHTML = `
      <div class="duck-hunt-miss">
        <div class="duck-hunt-result-icon">&#x1F4A8;</div>
        <h2>MISSED!</h2>
        <p>The duck got away! Better luck next time, hunter.</p>
        <p class="duck-hunt-quip">The duck mocks you: "QUACK! Can't catch me!"</p>
        <p class="duck-hunt-quip">DuckDB survives to process another day.</p>
      </div>`;
    logShot(false);
  }

  async function logShot(hit) {
    try {
      await apiFetch('/api/duck-hunt', {
        method: 'POST',
        body: JSON.stringify({ hit: hit }),
      });
    } catch (_) { /* duck logging is best-effort */ }
  }

  // Start the animation
  animFrame = requestAnimationFrame(frame);

  // Timeout: if no click in 10 seconds, the duck is getting away quicker than normal
  missTimeout = setTimeout(() => {
    if (!shotFired && duck.alive) {
      duck.speed = 6; // speed up to escape faster
    }
  }, 6000);
}
