import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFile } from 'node:child_process';

export const AGY_SETTINGS = { toolPermission: 'request-review', agentMode: 'default' };
export const AGY_MIN_VERSION = '1.5.3';
export const AGY_GATED_KINDS = new Set(['execute', 'edit', 'delete', 'move', 'fetch']);

export function parseSemver(v) {
  const m = /(\d+)\.(\d+)(?:\.(\d+))?/.exec(String(v || '').trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3] || 0)];
}

export function semverAtLeast(have, min) {
  const h = parseSemver(have);
  const m = parseSemver(min);
  if (!h || !m) return false;
  for (let i = 0; i < 3; i++) {
    if (h[i] !== m[i]) return h[i] > m[i];
  }
  return true;
}

export async function checkVersion(command) {
  return new Promise((resolve) => {
    execFile(command, ['--version'], { encoding: 'utf8' }, (err, stdout) => {
      if (err) {
        const msg = String(err.message || '').split('\n')[0];
        return resolve({ ok: false, found: msg || 'unreadable', required: AGY_MIN_VERSION, error: `agy version unreadable (${msg || 'command failed'}), need >= ${AGY_MIN_VERSION} (agy acp, --gemini-dir, ACP shutdown fix)` });
      }
      const parsed = parseSemver(stdout);
      if (!parsed) {
        const foundStr = JSON.stringify(stdout.trim());
        return resolve({ ok: false, found: stdout.trim() || 'empty', required: AGY_MIN_VERSION, error: `agy version unreadable (found ${foundStr}), need >= ${AGY_MIN_VERSION} (agy acp, --gemini-dir, ACP shutdown fix)` });
      }
      const found = `${parsed[0]}.${parsed[1]}.${parsed[2]}`;
      if (!semverAtLeast(found, AGY_MIN_VERSION)) {
        return resolve({ ok: false, found, required: AGY_MIN_VERSION, error: `agy is too old: found ${found}, need >= ${AGY_MIN_VERSION} (agy acp, --gemini-dir, ACP shutdown fix)` });
      }
      return resolve({ ok: true, found, required: AGY_MIN_VERSION });
    });
  });
}

export function resolveCommand(opts = {}) {
  const agent = String(opts.agent || 'agy');
  const extra = Array.isArray(opts.agentArgs) ? opts.agentArgs.map(String) : [];
  if (extra.length === 0) {
    return { command: agent, args: opts.geminiDir ? ['acp', '--gemini-dir', opts.geminiDir] : ['acp'] };
  }
  return { command: agent, args: [...extra] };
}

export function isolatedSettingsDir(opts = {}, root = tmpdir()) {
  const extra = Array.isArray(opts.agentArgs) ? opts.agentArgs : [];
  if (extra.length) return '';
  const dir = mkdtempSync(join(root, 'irrigator-agy-'));
  mkdirSync(join(dir, 'antigravity-cli'), { recursive: true });
  writeFileSync(join(dir, 'antigravity-cli', 'settings.json'), JSON.stringify(AGY_SETTINGS), { mode: 0o600 });
  return dir;
}

export const agyAdapter = {
  name: 'agy',
  minVersion: AGY_MIN_VERSION,
  gatedKinds: AGY_GATED_KINDS,
  checkVersion,
  resolveCommand,
  isolatedSettingsDir,
};

export default agyAdapter;
