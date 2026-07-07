import fs from 'fs';
import path from 'path';

const ROOT = path.resolve(__dirname, '../../');
const PID_FILE = path.join(__dirname, '.test-server-pid');
const DB_PATH = path.join(ROOT, 'pos.db');
const DB_BACKUP = path.join(ROOT, 'pos.db.testbak');
const BACKUP_MARKER = path.join(__dirname, '.test-db-backup');

function log(msg: string) {
  process.stdout.write(`[global-teardown] ${msg}\n`);
}

export default async function teardown(): Promise<void> {
  // Kill the test server
  if (fs.existsSync(PID_FILE)) {
    const pid = parseInt(fs.readFileSync(PID_FILE, 'utf8').trim(), 10);
    try {
      process.kill(pid, 'SIGTERM');
      log(`Sent SIGTERM to server (pid ${pid})`);
    } catch {
      log(`Server process ${pid} already gone`);
    }
    fs.unlinkSync(PID_FILE);
  }

  // Remove the test database
  if (fs.existsSync(DB_PATH)) {
    fs.unlinkSync(DB_PATH);
    log('Removed test pos.db');
  }

  // Restore the original database if we backed one up
  if (fs.existsSync(BACKUP_MARKER)) {
    const backedUp = fs.readFileSync(BACKUP_MARKER, 'utf8').trim();
    if (backedUp === '1' && fs.existsSync(DB_BACKUP)) {
      fs.renameSync(DB_BACKUP, DB_PATH);
      log('Restored pos.db from backup');
    }
    fs.unlinkSync(BACKUP_MARKER);
  }
}
