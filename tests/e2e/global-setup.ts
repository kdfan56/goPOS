import { execSync, spawn, type ChildProcess } from 'child_process';
import fs from 'fs';
import path from 'path';
import https from 'https';

const ROOT = path.resolve(__dirname, '../../');
const GO_BIN = '/usr/local/go/bin';
const PID_FILE = path.join(__dirname, '.test-server-pid');
const DB_PATH = path.join(ROOT, 'pos.db');
const DB_BACKUP = path.join(ROOT, 'pos.db.testbak');

const SERVER_ENV = {
  GOPOS_USER: 'testadmin',
  GOPOS_PASS: 'testpass',
  GOPOS_CASHIER: 'testcashier',
  GOPOS_CASHIER_PASS: 'testcashpass',
};

function log(msg: string) {
  process.stdout.write(`[global-setup] ${msg}\n`);
}

async function waitForServer(url: string, timeoutMs = 20000): Promise<void> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      await new Promise<void>((resolve, reject) => {
        const req = https.get(url, { rejectUnauthorized: false }, (res) => {
          // Any response (even 401) means the server is up
          res.resume();
          resolve();
        });
        req.on('error', reject);
        req.setTimeout(2000, () => { req.destroy(); reject(new Error('timeout')); });
      });
      return;
    } catch {
      await new Promise(r => setTimeout(r, 500));
    }
  }
  throw new Error(`Server did not start within ${timeoutMs}ms`);
}

export default async function setup(): Promise<void> {
  log('Building goPOS-test binary...');
  const buildEnv = { ...process.env, PATH: `${GO_BIN}:${process.env.PATH || ''}` };
  execSync('go build -o goPOS-test .', { cwd: ROOT, stdio: 'pipe', env: buildEnv });

  // Generate self-signed cert if missing
  const certPath = path.join(ROOT, 'cert.pem');
  const keyPath = path.join(ROOT, 'key.pem');
  if (!fs.existsSync(certPath) || !fs.existsSync(keyPath)) {
    log('Generating self-signed cert...');
    execSync(
      'openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes -subj "/CN=localhost"',
      { cwd: ROOT, stdio: 'pipe' },
    );
  }

  // Backup existing dev database
  if (fs.existsSync(DB_PATH)) {
    log('Backing up existing pos.db → pos.db.testbak');
    fs.renameSync(DB_PATH, DB_BACKUP);
    // Write marker so teardown knows to restore
    fs.writeFileSync(path.join(__dirname, '.test-db-backup'), '1');
  } else {
    fs.writeFileSync(path.join(__dirname, '.test-db-backup'), '0');
  }

  // Start server with test credentials
  log('Starting test server on :8443...');
  const server: ChildProcess = spawn('./goPOS-test', [], {
    cwd: ROOT,
    env: { ...process.env, ...SERVER_ENV },
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  server.stdout?.on('data', (d: Buffer) => process.stdout.write(`[server] ${d}`));
  server.stderr?.on('data', (d: Buffer) => process.stderr.write(`[server] ${d}`));

  server.on('exit', (code, signal) => {
    if (code !== null && code !== 0) {
      log(`Server exited with code ${code} during startup — check output above`);
    }
  });

  // Wait for the server to be ready
  await waitForServer('https://localhost:8443');
  log('Server is ready.');

  // Store PID for teardown
  if (server.pid !== undefined) {
    fs.writeFileSync(PID_FILE, String(server.pid));
  }
}
