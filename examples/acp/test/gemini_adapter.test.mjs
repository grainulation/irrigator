import { test } from 'node:test';
import assert from 'node:assert/strict';
import { geminiAdapter, resolveCommand, GEMINI_GATED_KINDS } from '../adapters/gemini.mjs';

test('Gemini adapter: metadata and gated kinds', () => {
  assert.equal(geminiAdapter.name, 'gemini');
  assert.ok(GEMINI_GATED_KINDS.has('execute'));
  assert.ok(GEMINI_GATED_KINDS.has('edit'));
  assert.ok(GEMINI_GATED_KINDS.has('delete'));
  assert.ok(GEMINI_GATED_KINDS.has('move'));
  assert.ok(GEMINI_GATED_KINDS.has('fetch'));
});

test('Gemini adapter: resolveCommand defaults to gemini --acp', () => {
  const cmd = resolveCommand();
  assert.equal(cmd.command, 'gemini');
  assert.deepEqual(cmd.args, ['--acp']);
});

test('Gemini adapter: the model is not passed as session metadata', () => {
  assert.equal(geminiAdapter.sessionMeta, undefined);
});
