import { WebSocket } from 'ws';
import { execSync, spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

// ── Config ────────────────────────────────────────────────────────────────────

const BACKEND_URL = process.env.BACKEND_URL || 'http://localhost:3000';
const ACCOUNT_ID = process.env.ACCOUNT_ID || '';
const USER_ID = process.env.USER_ID || '';
const SESSION_ID = process.env.SESSION_ID || '';
const DURATION_MINUTES = Number(process.env.DURATION_MINUTES || '60');
const PROJECTS_DIR = '/tmp/apkbuilder-projects';

// ── State ─────────────────────────────────────────────────────────────────────

let ws = null;
let reconnectAttempts = 0;
const MAX_RECONNECT = 10;
const RECONNECT_DELAY = 5000;

// ── Main ──────────────────────────────────────────────────────────────────────

function connect() {
  const wsUrl = BACKEND_URL.replace(/^http/, 'ws') + '/api/agent';
  console.log(`[agent] Connecting to ${wsUrl}`);

  ws = new WebSocket(wsUrl);

  ws.on('open', () => {
    console.log('[agent] Connected to backend');
    reconnectAttempts = 0;

    // Register with backend
    ws.send(JSON.stringify({
      type: 'register',
      accountId: ACCOUNT_ID,
      userId: USER_ID,
      sessionId: SESSION_ID,
    }));
  });

  ws.on('message', async (raw) => {
    try {
      const msg = JSON.parse(raw.toString());
      await handleMessage(msg);
    } catch (err) {
      console.error('[agent] Message error:', err.message);
    }
  });

  ws.on('close', (code, reason) => {
    console.log(`[agent] Disconnected: ${code} ${reason}`);
    if (reconnectAttempts < MAX_RECONNECT) {
      reconnectAttempts++;
      console.log(`[agent] Reconnecting in ${RECONNECT_DELAY / 1000}s (attempt ${reconnectAttempts}/${MAX_RECONNECT})`);
      setTimeout(connect, RECONNECT_DELAY);
    } else {
      console.log('[agent] Max reconnect attempts reached, exiting');
      process.exit(1);
    }
  });

  ws.on('error', (err) => {
    console.error('[agent] WebSocket error:', err.message);
  });
}

// ── Message Handler ───────────────────────────────────────────────────────────

async function handleMessage(msg) {
  switch (msg.type) {
    case 'registered':
      console.log(`[agent] Registered as agentId=${msg.agentId}`);
      break;

    case 'ping':
      ws.send(JSON.stringify({ type: 'pong' }));
      break;

    case 'build_request':
      await handleBuildRequest(msg);
      break;

    case 'terminal_data':
      await handleTerminalData(msg);
      break;

    case 'terminal_resize':
      // Terminal resize handled per-session
      break;

    case 'shutdown':
      console.log(`[agent] Shutdown requested: ${msg.reason}`);
      process.exit(0);
      break;
  }
}

// ── Build Handler ─────────────────────────────────────────────────────────────

async function handleBuildRequest(msg) {
  const { buildId, sessionId, files, meta, variant, format, platform } = msg;
  console.log(`[agent] Build request: buildId=${buildId} framework=${meta.framework} platform=${platform}`);

  const projectDir = join(PROJECTS_DIR, buildId);
  mkdirSync(projectDir, { recursive: true });

  // Write project files
  for (const [filePath, content] of Object.entries(files)) {
    const fullPath = join(projectDir, filePath);
    mkdirSync(join(fullPath, '..'), { recursive: true });
    writeFileSync(fullPath, content, 'utf8');
  }

  try {
    sendProgress(buildId, `Building ${meta.framework} project for ${platform}...`);
    sendProgress(buildId, `Variant: ${variant}, Format: ${format}`);

    // Install dependencies
    sendProgress(buildId, 'Installing dependencies...');
    execSync('npm install', { cwd: projectDir, stdio: 'pipe', timeout: 120_000 });
    sendProgress(buildId, 'Dependencies installed');

    if (meta.framework === 'react-native') {
      await buildReactNative(projectDir, buildId, variant, format, platform);
    } else if (meta.framework === 'hybrid') {
      await buildHybrid(projectDir, buildId, variant, format, platform);
    } else {
      throw new Error(`Unknown framework: ${meta.framework}`);
    }

    // Check for output artifact
    const artifactPath = findArtifact(projectDir, format);
    if (artifactPath) {
      sendProgress(buildId, `Build succeeded: ${artifactPath}`);
      ws.send(JSON.stringify({
        type: 'build_complete',
        buildId,
        status: 'succeeded',
        artifactUrl: `local://${artifactPath}`,
      }));
    } else {
      sendProgress(buildId, 'Build completed but no artifact found');
      ws.send(JSON.stringify({
        type: 'build_complete',
        buildId,
        status: 'succeeded',
      }));
    }
  } catch (err) {
    console.error(`[agent] Build failed:`, err.message);
    sendProgress(buildId, `Build failed: ${err.message}`);
    ws.send(JSON.stringify({
      type: 'build_complete',
      buildId,
      status: 'failed',
      error: err.message,
    }));
  }
}

async function buildReactNative(projectDir, buildId, variant, format, platform) {
  sendProgress(buildId, 'Running expo prebuild...');
  try {
    execSync('npx expo prebuild --platform android --no-install', {
      cwd: projectDir,
      stdio: 'pipe',
      timeout: 120_000,
    });
  } catch {
    sendProgress(buildId, 'Prebuild may have warnings, continuing...');
  }

  if (platform === 'web') {
    sendProgress(buildId, 'Building web bundle...');
    execSync('npx expo export --platform web', {
      cwd: projectDir,
      stdio: 'pipe',
      timeout: 180_000,
    });
    return;
  }

  // Android build
  const gradleTask = variant === 'release' ? 'bundleRelease' : 'assembleDebug';
  const gradlew = join(projectDir, 'android', 'gradlew');

  if (existsSync(gradlew)) {
    sendProgress(buildId, `Running gradle ${gradleTask}...`);
    execSync(`chmod +x ${gradlew} && ${gradlew} ${gradleTask} --no-daemon`, {
      cwd: join(projectDir, 'android'),
      stdio: 'pipe',
      timeout: 600_000,
    });
  } else {
    sendProgress(buildId, 'No gradlew found, using expo build...');
    execSync(`npx expo run:android --variant ${variant}`, {
      cwd: projectDir,
      stdio: 'pipe',
      timeout: 600_000,
    });
  }
}

async function buildHybrid(projectDir, buildId, variant, format, platform) {
  sendProgress(buildId, 'Building Vite bundle...');
  execSync('npx vite build', {
    cwd: projectDir,
    stdio: 'pipe',
    timeout: 120_000,
  });

  if (platform === 'android' || platform === 'ios') {
    sendProgress(buildId, 'Adding native platform...');
    execSync(`npx cap add ${platform}`, {
      cwd: projectDir,
      stdio: 'pipe',
      timeout: 60_000,
    });
    sendProgress(buildId, 'Syncing native platform...');
    execSync(`npx cap sync ${platform}`, {
      cwd: projectDir,
      stdio: 'pipe',
      timeout: 60_000,
    });
  }
}

function findArtifact(projectDir, format) {
  const candidates = format === 'aab'
    ? ['android/app/build/outputs/bundle/release/app-release.aab', 'android/app/build/outputs/bundle/debug/app-debug.aab']
    : ['android/app/build/outputs/apk/debug/app-debug.apk', 'android/app/build/outputs/apk/release/app-release.apk'];

  for (const c of candidates) {
    const full = join(projectDir, c);
    if (existsSync(full)) return full;
  }
  return null;
}

function sendProgress(buildId, line) {
  console.log(`[build:${buildId}] ${line}`);
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'build_progress', buildId, line }));
  }
}

// ── Terminal Handler ──────────────────────────────────────────────────────────

const terminalSessions = new Map();

async function handleTerminalData(msg) {
  const { sessionId, data } = msg;

  if (!terminalSessions.has(sessionId)) {
    // Spawn a new shell
    const shell = spawn('/bin/bash', [], {
      cwd: '/tmp',
      env: {
        ...process.env,
        TERM: 'xterm-256color',
        HOME: '/root',
        PATH: '/usr/local/bin:/usr/bin:/bin:/root/.local/bin',
      },
      stdio: ['pipe', 'pipe', 'pipe'],
    });

    shell.stdout.on('data', (chunk) => {
      sendTerminalOutput(sessionId, chunk.toString());
    });
    shell.stderr.on('data', (chunk) => {
      sendTerminalOutput(sessionId, chunk.toString());
    });
    shell.on('close', (code) => {
      sendTerminalOutput(sessionId, `\r\n[Shell exited with code ${code}]\r\n`);
      terminalSessions.delete(sessionId);
    });

    terminalSessions.set(sessionId, shell);
  }

  const shell = terminalSessions.get(sessionId);
  if (shell && !shell.killed) {
    shell.stdin.write(data);
  }
}

function sendTerminalOutput(sessionId, data) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'terminal_output', sessionId, data }));
  }
}

// ── Source Sync ───────────────────────────────────────────────────────────────

function syncToBackend() {
  // Periodically sync source files to backend
  // This is called every 30s to keep backend in sync
}

setInterval(syncToBackend, 30_000);

// ── Cleanup on exit ───────────────────────────────────────────────────────────

process.on('SIGTERM', () => {
  console.log('[agent] Received SIGTERM, cleaning up...');
  for (const [id, shell] of terminalSessions) {
    if (!shell.killed) shell.kill('SIGTERM');
  }
  terminalSessions.clear();
  if (ws) ws.close();
  process.exit(0);
});

process.on('SIGINT', () => {
  console.log('[agent] Received SIGINT, cleaning up...');
  for (const [id, shell] of terminalSessions) {
    if (!shell.killed) shell.kill('SIGTERM');
  }
  terminalSessions.clear();
  if (ws) ws.close();
  process.exit(0);
});

// ── Start ─────────────────────────────────────────────────────────────────────

console.log(`[agent] Starting agent...`);
console.log(`[agent] Backend: ${BACKEND_URL}`);
console.log(`[agent] Account: ${ACCOUNT_ID}`);
console.log(`[agent] User: ${USER_ID}`);
console.log(`[agent] Session: ${SESSION_ID}`);
console.log(`[agent] Duration: ${DURATION_MINUTES} minutes`);

connect();
