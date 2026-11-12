import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { agyAdapter, checkVersion, isolatedSettingsDir, parseSemver, resolveCommand, semverAtLeast, AGY_GATED_KINDS, AGY_MIN_VERSION, AGY_SETTINGS } from '../adapters/agy.mjs';

test('agy adapter: metadata and gated kinds', () => {
  assert.equal(agyAdapter.name, 'agy');
  assert.equal(agyAdapter.minVersion, '1.5.3');
  assert.ok(AGY_GATED_KINDS.has('execute'));
  assert.ok(AGY_GATED_KINDS.has('edit'));
  assert.ok(AGY_GATED_KINDS.has('delete'));
  assert.ok(AGY_GATED_KINDS.has('move'));
  assert.ok(AGY_GATED_KINDS.has('fetch'));
});

test('agy adapter: semver comparison', () => {
  assert.deepEqual(parseSemver('1.5.3'), [1, 5, 3]);
  assert.deepEqual(parseSemver('v2.0.0'), [2, 0, 0]);
  assert.ok(semverAtLeast('1.5.3', '1.5.3'));
  assert.ok(semverAtLeast('1.5.4', '1.5.3'));
  assert.ok(semverAtLeast('2.0.0', '1.5.3'));
  assert.ok(!semverAtLeast('1.5.2', '1.5.3'));
});

test('agy adapter: resolveCommand defaults to agy acp', () => {
  const cmd = resolveCommand();
  assert.equal(cmd.command, 'agy');
  assert.deepEqual(cmd.args, ['acp']);

  const withDir = resolveCommand({ geminiDir: '/tmp/settings' });
  assert.deepEqual(withDir.args, ['acp', '--gemini-dir', '/tmp/settings']);
});

test('agy adapter: isolatedSettingsDir writes request-review settings', () => {
  const dir = isolatedSettingsDir();
  try {
    assert.ok(dir);
    const settingsPath = join(dir, 'antigravity-cli', 'settings.json');
    assert.ok(existsSync(settingsPath));
    const content = JSON.parse(readFileSync(settingsPath, 'utf8'));
    assert.deepEqual(content, AGY_SETTINGS);
  } finally {
    if (dir) rmSync(dir, { recursive: true, force: true });
  }
});
