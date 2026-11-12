import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { appendFileSync, chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as agyModule from '../adapters/agy.mjs';
import { agyAdapter, claudeAdapter, geminiAdapter } from '../adapters/index.mjs';
import * as workerModule from '../worker.mjs';
import { AGY_MIN_VERSION, AGY_SETTINGS, CLAUDE_GATED_KINDS, DEFAULT_AGENT, GATED_KINDS, gatedKindsFor, checkAgyVersion, claudeSessionMeta, formatUsage, tokenSummary, configuredNotifier, isDenied, isolatedAgyDir, parseArgs, parseLink, parseSemver, readDecision, resolveAgentCommand, semverAtLeast, unaskedToolCall } from '../worker.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const worker = join(here, '..', 'worker.mjs');
const fakeIrrigator = join(here, 'fake-irrigator.mjs');
chmodSync(fakeIrrigator, 0o755);
const sh = (cwd, ...args) => execFileSync('git', ['-C', cwd, ...args], { encoding: 'utf8' }).trim();

function fixture() {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'acp-worker-')));
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  sh(repo, 'init', '-q', '-b', 'main');
  sh(repo, 'config', 'user.email', 't@example.com');
  sh(repo, 'config', 'user.name', 'T');
  sh(repo, 'commit', '-q', '--allow-empty', '-m', 'init');
  const runRoot = join(dir, 'run');
  mkdirSync(join(runRoot, 'approvals'), { recursive: true });
  return { dir, repo, runRoot, minted: join(dir, 'minted.jsonl'), log: join(dir, 'agent.jsonl'), ledger: join(runRoot, 'approvals', 'decisions.jsonl') };
}

const readLines = (p) => (existsSync(p) ? readFileSync(p, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)) : []);

// start launches the worker against the fake agent and returns { done }.
function start(fx, scenario, { timeout = 5, env = {}, args = [] } = {}) {
  const req = { protocol: 'swarm.worker/v1', request_id: 'req-1', run_id: 'abcdef123456', attempt: 1, kind: 'task', max_cost_usd: 5, timeout_ms: 120000, task: { id: 'cell1', prompt: 'do it' } };
  const child = spawn(process.execPath, [worker, '--repo', fx.repo, '--worktrees', join(fx.dir, 'wt'), '--agent', join(here, 'fake-agent.mjs'),
    '--irrigator', fakeIrrigator, '--run-root', fx.runRoot, '--permission-timeout', String(timeout), '--poll-ms', '20', '--no-tty', ...args], {
    cwd: fx.dir, stdio: ['pipe', 'pipe', 'pipe'],
    env: { ...process.env, FAKE_SCENARIO: scenario, FAKE_MINTED: fx.minted, FAKE_LOG: fx.log, IRRIGATOR_NTFY_TOPIC: '', ...env },
  });
  let out = '';
  let err = '';
  child.stdout.on('data', (d) => (out += d));
  child.stderr.on('data', (d) => (err += d));
  child.stdin.end(JSON.stringify(req));
  const done = new Promise((resolve) => child.on('close', () => resolve({ result: out ? JSON.parse(out) : null, stderr: err })));
  return { done };
}

async function minted(fx, n) {
  for (let i = 0; i < 500; i++) {
    const rows = readLines(fx.minted);
    if (rows.length >= n) return rows;
    await new Promise((r) => setTimeout(r, 20));
  }
  throw new Error('links were never minted');
}

function decide(fx, rows, action, request = 'cell1') {
  const row = rows.find((r) => r.action === action);
  appendFileSync(fx.ledger, JSON.stringify({ schema: 'irrigator.approval/v1', run_id: 'r', request, decision: action, nonce: row.nonce, at: 1 }) + '\n');
}

const outcomeOf = (fx) => readLines(fx.log).find((l) => l.outcome).outcome;

test('approved permission via the ledger lets the edit happen and is committed', async () => {
  const fx = fixture();
  const { done } = start(fx, 'edit');
  const rows = await minted(fx, 2);
  assert.deepEqual(rows.map((r) => r.action).sort(), ['approve', 'deny']);
  const argv = rows[0].argv;
  assert.deepEqual(argv.slice(0, 2), ['approvals', 'link']);
  // A public URL may be known while `approvals serve --expose` runs; the worker
  // needs the bare path either way.
  assert.ok(argv.includes('--path-only'), 'links are requested path-only');
  assert.equal(argv[argv.indexOf('--run') + 1], fx.runRoot);
  assert.equal(argv[argv.indexOf('--request') + 1], 'cell1');
  decide(fx, rows, 'approve');
  const { result } = await done;
  assert.equal(result.outcome, 'settled', JSON.stringify(result));
  assert.equal(result.protocol, 'swarm.worker/v1');
  assert.equal(result.request_id, 'req-1');
  assert.equal(result.output.branch, 'irrigator/abcdef12/cell1');
  assert.match(result.output.diffstat, /1 file changed/);
  assert.equal(result.output.summary, 'all done');
  assert.equal(readLines(fx.log).find((l) => l.setMode).setMode, 'default');
  assert.equal(outcomeOf(fx).optionId, 'allow');
  assert.equal(sh(fx.repo, 'show', 'irrigator/abcdef12/cell1:hello.txt'), 'hello');
});

test('denied permission answers reject_once and nothing changes', async () => {
  const fx = fixture();
  const { done } = start(fx, 'edit');
  decide(fx, await minted(fx, 2), 'deny');
  const { result } = await done;
  assert.equal(outcomeOf(fx).optionId, 'reject');
  assert.equal(result.outcome, 'settled');
  assert.equal(result.error, 'agent finished without changing anything');
  assert.equal(result.output, undefined);
});

test('no decision before the timeout rejects, never approves', async () => {
  const fx = fixture();
  const t0 = Date.now();
  const { done } = start(fx, 'edit', { timeout: 1 });
  const { result } = await done;
  assert.ok(Date.now() - t0 >= 1000);
  assert.equal(outcomeOf(fx).optionId, 'reject');
  assert.equal(result.error, 'agent finished without changing anything');
});

test('a decision for another request or link does not count', async () => {
  const fx = fixture();
  const { done } = start(fx, 'edit', { timeout: 1 });
  const rows = await minted(fx, 2);
  decide(fx, rows, 'approve', 'other-cell');
  appendFileSync(fx.ledger, JSON.stringify({ request: 'cell1', decision: 'approve', nonce: 'not-ours' }) + '\n');
  await done;
  assert.equal(outcomeOf(fx).optionId, 'reject');
});

test('cost comes from the last usage_update', async () => {
  const fx = fixture();
  const { done } = start(fx, 'edit');
  decide(fx, await minted(fx, 2), 'approve');
  const { result } = await done;
  assert.equal(result.cost_usd, 0.25);
});

test('agent crash is uncertain and keeps the cost seen so far', async () => {
  const fx = fixture();
  const { result } = await start(fx, 'crash').done;
  assert.equal(result.outcome, 'uncertain', JSON.stringify(result));
  assert.match(result.error, /ended without a result/);
  assert.equal(result.cost_usd, 0.42);
});

test('retry continues the cell branch and removes earlier attempt worktree', async () => {
  const fx = fixture();
  const res1 = await start(fx, 'crash', { req: { kind: 'task', run_id: 'abcdef123456', attempt: 1, task: { id: 'cell1', prompt: 'hi' } } }).done;
  assert.equal(res1.result.outcome, 'uncertain');

  const { done } = start(fx, 'edit', { req: { kind: 'task', run_id: 'abcdef123456', attempt: 2, task: { id: 'cell1', prompt: 'hi' } } });
  decide(fx, await minted(fx, 2), 'approve');
  const res2 = await done;
  assert.equal(res2.result.outcome, 'settled');
  assert.equal(res2.result.output.branch, 'irrigator/abcdef12/cell1');
});

test('denied commands are rejected without minting approval links', async () => {
  const fx = fixture();
  const { result } = await start(fx, 'denycmd').done;
  assert.equal(outcomeOf(fx).optionId, 'reject');
  assert.equal(readLines(fx.minted).length, 0);
  assert.equal(result.outcome, 'settled');
});

test('validators are not_started', async () => {
  const fx = fixture();
  const child = spawn(process.execPath, [worker, '--repo', fx.repo, '--no-tty'], { stdio: ['pipe', 'pipe', 'inherit'] });
  let out = '';
  child.stdout.on('data', (d) => (out += d));
  child.stdin.end(JSON.stringify({ request_id: 'r', kind: 'validator', task: { id: 'v' } }));
  await new Promise((r) => child.on('close', r));
  assert.equal(JSON.parse(out).outcome, 'not_started');
});

test('a repo without the base branch is not_started', async () => {
  const fx = fixture();
  const { result } = await start(fx, 'edit', { args: ['--base', 'nope'] }).done;
  assert.equal(result.outcome, 'not_started');
  assert.match(result.error, /worktree setup failed/);
});

test('ntfy notification carries the title, tool title and Approve/Deny POST buttons', async () => {
  const fx = fixture();
  const seen = [];
  const server = createServer((rq, rs) => {
    let body = '';
    rq.on('data', (d) => (body += d));
    rq.on('end', () => { seen.push({ url: rq.url, headers: rq.headers, body }); rs.end('ok'); });
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const { done } = start(fx, 'edit', {
    timeout: 1,
    env: {
      IRRIGATOR_NTFY_TOPIC: 'secret-topic',
      IRRIGATOR_NTFY_SERVER: `http://127.0.0.1:${server.address().port}`,
      IRRIGATOR_APPROVALS_BASE_URL: 'https://tunnel.example/',
    },
  });
  const { stderr } = await done;
  server.close();
  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, '/secret-topic');
  assert.equal(seen[0].body, 'Write hello.txt');
  assert.match(seen[0].headers.title, /cell1/);
  assert.match(seen[0].headers.actions, /http, Approve, https:\/\/tunnel\.example\/approve\/.+, method=POST; http, Deny, https:\/\/tunnel\.example\/deny\//);
  assert.ok(!stderr.includes('/approve/'), 'links must not be logged');
});

test('a configured notifier replaces the own ntfy code and its nonces are awaited', async () => {
  const fx = fixture();
  const seen = [];
  const server = createServer((rq, rs) => { seen.push(rq.url); rs.end('ok'); });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const { done } = start(fx, 'edit', {
    env: {
      IRRIGATOR_NOTIFY_VIA: 'ntfy',
      IRRIGATOR_NTFY_TOPIC: 'secret-topic',
      IRRIGATOR_NTFY_SERVER: `http://127.0.0.1:${server.address().port}`,
      IRRIGATOR_APPROVALS_BASE_URL: 'https://tunnel.example/',
    },
  });
  const rows = await minted(fx, 2);
  const argv = rows[0].argv;
  assert.deepEqual(argv.slice(0, 2), ['approvals', 'notify']);
  assert.ok(argv.includes('--json'));
  assert.equal(argv[argv.indexOf('--via') + 1], 'ntfy');
  assert.equal(argv[argv.indexOf('--request') + 1], 'cell1');
  assert.equal(argv[argv.indexOf('--public-url') + 1], 'https://tunnel.example/');
  assert.equal(argv[argv.indexOf('--message') + 1], 'Write hello.txt');
  assert.ok(!argv.some((a) => a.includes('secret-topic')), 'topic is not passed in argv');
  decide(fx, rows, 'approve');
  const { result, stderr } = await done;
  server.close();
  assert.equal(result.outcome, 'settled', JSON.stringify(result));
  assert.deepEqual(seen, [], 'the worker no longer posts to ntfy itself');
  assert.ok(!stderr.includes('/approve/'), 'links must not be logged');
});

test('the notifier recorded in the run is used, and a failing notify falls back to plain links', async () => {
  const fx = fixture();
  writeFileSync(join(fx.runRoot, 'notify.json'), JSON.stringify({ via: 'webhook' }));
  const { done } = start(fx, 'edit', { env: { FAKE_NOTIFY_FAIL: '1' } });
  const rows = await minted(fx, 2);
  assert.deepEqual(rows[0].argv.slice(0, 2), ['approvals', 'link']);
  decide(fx, rows, 'deny');
  const { stderr } = await done;
  assert.match(stderr, /notification via webhook failed, waiting without it/);
  assert.ok(!readLines(fx.minted).some((r) => r.argv[1] === 'notify'), 'failed notify minted nothing');
});

test('helpers: isDenied, parseLink, readDecision, resolveAgentCommand', () => {
  assert.equal(configuredNotifier('/nonexistent'), '');
  assert.equal(isDenied({ title: 'Bash', rawInput: { command: 'cd a && git push origin x' } }), 'git push');
  assert.equal(isDenied({ title: 'run_command', rawInput: { CommandLine: 'cd a && git push origin x' } }), 'git push');
  assert.equal(isDenied({ title: 'Bash', rawInput: { command: 'rm -rf x' } }), 'rm');
  assert.equal(isDenied({ title: 'WebFetch https://x' }), 'WebFetch');
  assert.equal(isDenied({ title: 'search_web', toolName: 'search_web' }), 'search_web');
  assert.equal(isDenied({ title: 'google_web_search', name: 'google_web_search' }), 'google_web_search');
  assert.equal(isDenied({ title: 'Bash', rawInput: { command: 'go test ./... | grep rm' } }), '');
  assert.equal(isDenied({ title: 'Write a.txt', kind: 'edit' }), '');
  assert.deepEqual(resolveAgentCommand(parseArgs(['--repo', '/tmp/r', '--agent', 'agy'])), { command: 'agy', args: ['acp'] });
  assert.deepEqual(resolveAgentCommand({ ...parseArgs(['--repo', '/tmp/r', '--agent', 'agy']), geminiDir: '/tmp/g' }), { command: 'agy', args: ['acp', '--gemini-dir', '/tmp/g'] });
  assert.equal(isDenied({ title: 'open_browser_url', name: 'open_browser_url' }), 'open_browser_url');
  assert.deepEqual(resolveAgentCommand(parseArgs(['--repo', '/tmp/r', '--agent', '/usr/local/bin/gemini'])), { command: '/usr/local/bin/gemini', args: ['--acp'] });
  assert.deepEqual(resolveAgentCommand(parseArgs(['--repo', '/tmp/r', '--agent', 'agy', '--agent-arg', 'acp', '--agent-arg', '--verbose'])), { command: 'agy', args: ['acp', '--verbose'] });
  assert.deepEqual(resolveAgentCommand(parseArgs(['--repo', '/tmp/r', '--agent', '/tmp/fake.mjs'])), { command: process.execPath, args: ['/tmp/fake.mjs'] });
  const tok = Buffer.from(JSON.stringify({ n: 'abc' })).toString('base64url');
  assert.deepEqual(parseLink(`/approve/${tok}.mac\n`), { path: `/approve/${tok}.mac`, nonce: 'abc' });
  assert.throws(() => parseLink('error: nope'));
  assert.equal(readDecision('/nonexistent', 'c', new Set(['a'])), '');
});

test('non-JS executable ACP agent binary (such as agy acp or gemini --acp) launches directly with default subcommand', async () => {
  const fx = fixture();
  const wrapper = join(fx.dir, 'agy');
  writeFileSync(wrapper, `#!/bin/sh\nif [ "$1" = "--version" ]; then echo "1.5.3"; exit 0; fi\nif [ "$1" != "acp" ]; then exit 2; fi\nexec "${process.execPath}" "${join(here, 'fake-agent.mjs')}"\n`);
  chmodSync(wrapper, 0o755);
  const { done } = start(fx, 'edit', { args: ['--agent', wrapper] });
  decide(fx, await minted(fx, 2), 'approve');
  const { result } = await done;
  assert.equal(result.outcome, 'settled', JSON.stringify(result));
  assert.equal(sh(fx.repo, 'show', 'irrigator/abcdef12/cell1:hello.txt'), 'hello');
});

test('agy older than minimum version answers not_started with found and required versions', async () => {
  const fx = fixture();
  const wrapper = join(fx.dir, 'agy');
  writeFileSync(wrapper, `#!/bin/sh\nif [ "$1" = "--version" ]; then echo "1.5.2"; exit 0; fi\nexit 2\n`);
  chmodSync(wrapper, 0o755);
  const { done } = start(fx, 'edit', { args: ['--agent', wrapper] });
  const { result } = await done;
  assert.equal(result.outcome, 'not_started');
  assert.match(result.error, /too old/);
  assert.match(result.error, /1\.5\.2/);
  assert.match(result.error, new RegExp(AGY_MIN_VERSION.replace('.', '\\.')));
});

test('agy with unreadable version answers not_started with found and required versions', async () => {
  const fx = fixture();
  const wrapper = join(fx.dir, 'agy');
  writeFileSync(wrapper, `#!/bin/sh\nif [ "$1" = "--version" ]; then echo "invalid-output"; exit 0; fi\nexit 2\n`);
  chmodSync(wrapper, 0o755);
  const { done } = start(fx, 'edit', { args: ['--agent', wrapper] });
  const { result } = await done;
  assert.equal(result.outcome, 'not_started');
  assert.match(result.error, /unreadable/);
  assert.match(result.error, /invalid-output/);
  assert.match(result.error, new RegExp(AGY_MIN_VERSION.replace('.', '\\.')));
});

test('agy version helper parses and checks semver correctly', () => {
  assert.equal(AGY_MIN_VERSION, '1.5.3');
  assert.deepEqual(parseSemver('1.5.3'), [1, 5, 3]);
  assert.deepEqual(parseSemver('agy 1.5.3 (abcdef)'), [1, 5, 3]);
  assert.equal(parseSemver('garbage'), null);
  assert.equal(semverAtLeast('1.5.3', '1.5.3'), true);
  assert.equal(semverAtLeast('1.6.0', '1.5.3'), true);
  assert.equal(semverAtLeast('1.5.2', '1.5.3'), false);
  assert.equal(semverAtLeast('1.4.99', '1.5.3'), false);
});

test('worker re-exports AGY gating from the adapter instead of duplicating it', async () => {
  assert.equal(workerModule.AGY_SETTINGS, agyModule.AGY_SETTINGS);
  assert.equal(workerModule.AGY_MIN_VERSION, agyModule.AGY_MIN_VERSION);
  assert.equal(workerModule.parseSemver, agyModule.parseSemver);
  assert.equal(workerModule.semverAtLeast, agyModule.semverAtLeast);
  assert.equal(agyAdapter.minVersion, AGY_MIN_VERSION);
  assert.equal(GATED_KINDS, agyAdapter.gatedKinds);
  assert.equal(CLAUDE_GATED_KINDS, claudeAdapter.gatedKinds);
  assert.equal(DEFAULT_AGENT, claudeAdapter.defaultAgent);
  // checkAgyVersion delegates to the adapter: same verdict, same wording.
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'agy-ver-')));
  const old = join(dir, 'agy');
  writeFileSync(old, '#!/bin/sh\necho "agy 1.2.0"\n');
  chmodSync(old, 0o755);
  assert.deepEqual(await checkAgyVersion(old), await agyAdapter.checkVersion(old));
  assert.equal((await checkAgyVersion(old)).ok, false);
});

test('gated tool kinds come from the adapter for the selected agent', () => {
  assert.equal(gatedKindsFor({}), claudeAdapter.gatedKinds);
  assert.equal(gatedKindsFor({ agent: DEFAULT_AGENT }), claudeAdapter.gatedKinds);
  assert.equal(gatedKindsFor({ agent: '/opt/bin/agy' }), agyAdapter.gatedKinds);
  assert.equal(gatedKindsFor({ agent: 'gemini' }), geminiAdapter.gatedKinds);
  // An unrecognised agent binary never inherits the Claude adapter's relaxed set.
  const custom = gatedKindsFor({ agent: '/usr/local/bin/other-agent' });
  assert.notEqual(custom, claudeAdapter.gatedKinds);
  assert.ok(custom.has('execute'));
});

test('agy runs with its own ask-first settings directory', () => {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'agy-dir-')));
  assert.equal(isolatedAgyDir({ agent: 'claude' }, root), '');
  assert.equal(isolatedAgyDir({ agent: 'agy', agentArgs: ['acp'] }, root), '', 'custom agent args are the caller\'s choice');
  const dir = isolatedAgyDir({ agent: '/opt/bin/agy', agentArgs: [] }, root);
  assert.deepEqual(JSON.parse(readFileSync(join(dir, 'antigravity-cli', 'settings.json'), 'utf8')), AGY_SETTINGS);
  assert.equal(AGY_SETTINGS.toolPermission, 'request-review');
});

test('unaskedToolCall: agy sequence with a permission request is fine; a completed gated call that never asked is caught', () => {
  const kinds = new Map();
  const asked = new Set();
  // Recorded from agy 1.5.3 under request-review: read, then a command that asks and is denied.
  const seq = [
    { sessionUpdate: 'tool_call', toolCallId: 'r', kind: 'read', status: 'in_progress' },
    { sessionUpdate: 'tool_call_update', toolCallId: 'r', kind: 'read', status: 'completed' },
    { sessionUpdate: 'tool_call', toolCallId: 'c', kind: 'execute', status: 'in_progress' },
    { sessionUpdate: 'tool_call_update', toolCallId: 'c', kind: 'execute', status: 'pending' },
  ];
  for (const u of seq) assert.equal(unaskedToolCall(u, kinds, asked), '');
  asked.add('c');
  assert.equal(unaskedToolCall({ sessionUpdate: 'tool_call_update', toolCallId: 'c', status: 'completed' }, kinds, asked), '');
  // always-proceed: the same call kind completes with no request.
  assert.equal(unaskedToolCall({ sessionUpdate: 'tool_call', toolCallId: 'x', kind: 'execute', title: 'touch f', status: 'in_progress' }, kinds, asked), '');
  assert.equal(unaskedToolCall({ sessionUpdate: 'tool_call_update', toolCallId: 'x', status: 'completed' }, kinds, asked), 'execute call "touch f"');
  assert.equal(unaskedToolCall({ sessionUpdate: 'agent_message_chunk' }, kinds, asked), '');
});

test('an agent that runs a command without asking leaves the cell uncertain', async () => {
  const fx = fixture();
  const { done } = start(fx, 'unasked');
  const { result, stderr } = await done;
  assert.equal(result.outcome, 'uncertain', JSON.stringify(result));
  assert.match(result.error, /without asking for permission/);
  assert.match(stderr, /without asking; stopping/);
});

test('tokenSummary formats ACP usage and ignores missing usage', () => {
  assert.equal(tokenSummary(null), '');
  assert.equal(tokenSummary({ totalTokens: 0 }), '');
  // Shape recorded from agy 1.5.3's session/prompt result.
  assert.equal(tokenSummary({ inputTokens: 7031, outputTokens: 71, cachedReadTokens: 0, cachedWriteTokens: 0, thoughtTokens: 60, totalTokens: 7102 }),
    '7102 tokens (7031 in, 71 out, 60 thinking, 0 cached)');
});

test('the bundled Claude adapter ignores the user\'s settings; other agents are untouched', () => {
  assert.deepEqual(claudeSessionMeta({}), { _meta: { claudeCode: { options: { settingSources: [] } } } });
  assert.deepEqual(claudeSessionMeta({ model: 'opus' }), { _meta: { claudeCode: { options: { settingSources: [], model: 'opus' } } } });
  assert.deepEqual(claudeSessionMeta({ agent: 'agy' }), {});
  assert.deepEqual(claudeSessionMeta({ agent: '/tmp/fake.mjs' }), {});
});

test('for the Claude adapter, read-only shell commands that run without asking are not a violation; edits are', () => {
  const kinds = new Map();
  const asked = new Set();
  const claude = new Set(['edit', 'delete', 'move', 'fetch']);
  // Recorded from claude-agent-acp in the ask-first mode: `ls` runs without a request.
  unaskedToolCall({ sessionUpdate: 'tool_call', toolCallId: 'ls', kind: 'execute', title: 'ls', status: 'pending' }, kinds, asked, claude);
  assert.equal(unaskedToolCall({ sessionUpdate: 'tool_call_update', toolCallId: 'ls', status: 'completed' }, kinds, asked, claude), '');
  unaskedToolCall({ sessionUpdate: 'tool_call', toolCallId: 'w', kind: 'edit', title: 'Write a.txt', status: 'pending' }, kinds, asked, claude);
  assert.equal(unaskedToolCall({ sessionUpdate: 'tool_call_update', toolCallId: 'w', status: 'completed' }, kinds, asked, claude), 'edit call "Write a.txt"');
});

test('--model picks a model through the ACP config option, and an unknown model is not_started', async () => {
  const fx = fixture();
  const { done } = start(fx, 'edit', { args: ['--model', 'deep'] });
  decide(fx, await minted(fx, 2), 'approve');
  const { result } = await done;
  assert.equal(result.outcome, 'settled', JSON.stringify(result));
  assert.ok(readLines(fx.log).some((l) => JSON.stringify(l.setConfig) === JSON.stringify(['model', 'deep'])));
  const fx2 = fixture();
  const bad = await start(fx2, 'edit', { args: ['--model', 'nope'] }).done;
  assert.equal(bad.result.outcome, 'not_started');
  assert.match(bad.result.error, /choose one of: fast, deep/);
});

test('formatUsage maps ACP usage to canonical swarm usage object', () => {
  assert.equal(formatUsage(null), undefined);
  assert.deepEqual(formatUsage({
    inputTokens: 7031,
    outputTokens: 71,
    thoughtTokens: 60,
    cachedReadTokens: 10,
    cachedWriteTokens: 5,
    totalTokens: 7177,
  }, 'claude-3-7-sonnet'), {
    input_tokens: 7031,
    output_tokens: 71,
    thinking_tokens: 60,
    cache_read_tokens: 10,
    cache_write_tokens: 5,
    total_tokens: 7177,
    model: 'claude-3-7-sonnet',
  });
});


test('visibleText removes terminal control sequences from agent-controlled progress text', () => {
  const osc52 = '\u001b]52;c;cHJvYmU=\u0007';
  const out = workerModule.visibleText(`title ${osc52} \u009b31m \u202eevil\u200b \u0000end`);
  assert.doesNotMatch(out, /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/u);
  assert.equal(workerModule.visibleText('a\u2060b\u00adc\u061cd\u2028e'), 'a<U+2060>b<U+00AD>c<U+061C>d<U+2028>e');
  assert.equal(workerModule.visibleText('line1\nline2\tend'), 'line1 line2 end');
  assert.equal(workerModule.visibleText('emoji \ud83d\udc69\u200d\ud83d\udcbb'), 'emoji \ud83d\udc69<U+200D>\ud83d\udcbb');
  assert.match(out, /title/);
  assert.equal(workerModule.visibleText('plain ascii and unicode é ✓'), 'plain ascii and unicode é ✓');
});

test('ntfyServer applies the Go notifier destination rules', () => {
  assert.equal(workerModule.ntfyServer('https://ntfy.sh'), 'https://ntfy.sh');
  assert.equal(workerModule.ntfyServer('https://ntfy.example.test:8443/base/'), 'https://ntfy.example.test:8443/base');
  assert.equal(workerModule.ntfyServer('http://127.0.0.1:8080'), 'http://127.0.0.1:8080');
  assert.equal(workerModule.ntfyServer('http://localhost:8080'), 'http://localhost:8080');
  for (const bad of ['http://attacker.invalid/topic', 'https://user:pass@ntfy.example.test', 'ftp://ntfy.example.test', 'not a url', '']) {
    assert.equal(workerModule.ntfyServer(bad), '', bad);
  }
});
