import { test } from 'node:test';
import assert from 'node:assert/strict';
import { claudeAdapter, resolveCommand, sessionMeta, CLAUDE_GATED_KINDS, DEFAULT_AGENT } from '../adapters/claude.mjs';

test('Claude adapter: metadata and gated kinds', () => {
  assert.equal(claudeAdapter.name, 'claude');
  assert.equal(claudeAdapter.defaultAgent, DEFAULT_AGENT);
  assert.ok(CLAUDE_GATED_KINDS.has('edit'));
  assert.ok(CLAUDE_GATED_KINDS.has('delete'));
  assert.ok(CLAUDE_GATED_KINDS.has('move'));
  assert.ok(CLAUDE_GATED_KINDS.has('fetch'));
  // Claude adapter intentionally does not gate 'execute' shell commands globally
  assert.ok(!CLAUDE_GATED_KINDS.has('execute'));
});

test('Claude adapter: resolveCommand defaults to bundled JS agent', () => {
  const cmd = resolveCommand();
  assert.equal(cmd.command, process.execPath);
  assert.deepEqual(cmd.args, [DEFAULT_AGENT]);
});

test('Claude adapter: sessionMeta isolates settings and applies model', () => {
  const meta = sessionMeta({ model: 'claude-3-7-sonnet' });
  assert.deepEqual(meta, {
    _meta: {
      claudeCode: {
        options: {
          settingSources: [],
          model: 'claude-3-7-sonnet',
        },
      },
    },
  });
});
