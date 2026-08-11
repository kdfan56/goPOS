import { execSync, spawn, type ChildProcess } from 'child_process';
import fs from 'fs';
import net from 'net';
import path from 'path';
import https from 'https';

const ROOT = path.resolve(__dirname, '../../');
const GO_BIN = '/usr/local/go/bin';
const PID_FILE = path.join(__dirname, '.test-server-pid');
const DB_PATH = path.join(ROOT, 'pos.db');
const DB_BACKUP = path.join(ROOT, 'pos.db.testbak');
const BACKUP_MARKER = path.join(__dirname, '.test-db-backup');

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

// portInUse answers whether anything already listens on :8443.
function portInUse(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host: '127.0.0.1', port });
    const done = (busy: boolean) => { socket.destroy(); resolve(busy); };
    socket.once('connect', () => done(true));
    socket.once('error', () => done(false));
    socket.setTimeout(1500, () => done(false));
  });
}

export default async function setup(): Promise<void> {
  // GUARD 1: a marker left behind by a crashed run describes THAT run, not
  // this one. Teardown treats the marker as proof that the pos.db on disk is a
  // throwaway it may delete. Clear it before anything else, so a stale marker
  // can never authorise deleting a real database.
  fs.rmSync(BACKUP_MARKER, { force: true });

  // GUARD 2: a leftover backup means an earlier run did not finish, and the
  // REAL database is the one sitting in pos.db.testbak. The rename below would
  // overwrite it with that run's throwaway seed database, and the restore at
  // the end would then hand back the seed. That is how a full history gets
  // lost with no copy left anywhere. Refuse, and let a human decide.
  if (fs.existsSync(DB_BACKUP)) {
    throw new Error(
      'pos.db.testbak exists, so an earlier run did not finish. It is probably your REAL ' +
      'database. Restore it by hand (mv pos.db.testbak pos.db) before running the suite.',
    );
  }

  // GUARD 3: refuse to start when :8443 is already taken.
  //
  // This is not politeness. `waitForServer` below accepts ANY response, so a
  // dev server on the same port makes setup report success while the test
  // server is dead. The tests then run against the dev server, which is still
  // holding the pos.db that the backup step just renamed away.
  //
  // Two test runs at the same time are worse. The second run backs up the
  // FIRST run's throwaway database over the real backup, and the two teardowns
  // then delete what is left. That destroyed a development database one time.
  // Stop the other server, or wait for the other run to finish.
  if (await portInUse(8443)) {
    throw new Error(
      'Port 8443 is already in use. Stop the dev server (or the other test run) ' +
      'before running the e2e suite — the suite renames pos.db and needs the port to itself.',
    );
  }

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
    fs.writeFileSync(BACKUP_MARKER, '1');
  } else {
    fs.writeFileSync(BACKUP_MARKER, '0');
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
