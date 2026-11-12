#!/usr/bin/env node
// swarm.worker/v1 command worker that drives one ACP agent (Claude Code via
// @agentclientprotocol/claude-agent-acp) in a git worktree.
//
// Same worktree layout, branch naming and outcomes as examples/coder/worker.py:
// branch irrigator/<run8>/<cell>, one worktree per cell and attempt. The agent
// runs in "default" mode, so every tool call that is not denied outright pauses
// on session/request_permission. The worker mints approve/deny links with
// `irrigator approvals link`, optionally pushes them to ntfy, then waits for the
// decision in the run's approvals ledger. No decision before the timeout means
// reject. Nothing is ever approved automatically.
//
// Outcomes: not_started when nothing was dispatched (wrong request kind, git
// setup failed, agent could not be launched or configured); uncertain when the
// agent timed out, crashed or ended without a clean stop, or git failed after it
// ran; settled otherwise.
import { spawn, execFile } from 'node:child_process';
import { existsSync, openSync, readFileSync, writeSync, closeSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { Readable, Writable } from 'node:stream';
import { fileURLToPath } from 'node:url';
import { client, methods, ndJsonStream, PROTOCOL_VERSION } from '@agentclientprotocol/sdk/unstable';
import { getAdapter, claudeAdapter, agyAdapter } from './adapters/index.mjs';
import { AGY_SETTINGS, AGY_MIN_VERSION, parseSemver, semverAtLeast } from './adapters/agy.mjs';

// The adapters under ./adapters are the single source for agent-specific
// behavior. These re-exports keep the worker's historical names working.
export { AGY_SETTINGS, AGY_MIN_VERSION, parseSemver, semverAtLeast };

export const PROTOCOL = 'swarm.worker/v1';
export const DENIED_COMMANDS = ['git push', 'git reset', 'git checkout', 'git switch', 'rm'];
export const DENIED_TOOLS = ['WebFetch', 'WebSearch', 'search_web', 'read_url_content', 'web_fetch', 'google_web_search',
  'open_browser_url', 'read_browser_page', 'click_browser_pixel', 'type_browser_text'];
// Tool kinds that must be put to a person before they run. A completed call of
// one of these kinds that never asked means the agent's own settings bypass
// Irrigator's approvals (agy with toolPermission "always-proceed", for example).
export const CLAUDE_GATED_KINDS = claudeAdapter.gatedKinds;
export const DEFAULT_AGENT = claudeAdapter.defaultAgent;
// Strict set used for every agent that is not the bundled Claude adapter.
export const GATED_KINDS = agyAdapter.gatedKinds;
export const checkAgyVersion = (command) => agyAdapter.checkVersion(command);

// gatedKindsFor returns the tool kinds that must be put to a person for this
// agent. Only the bundled Claude adapter may run read-only commands unasked;
// agy, gemini and any other agent use their adapter's set, or the strict
// default for an unrecognised agent binary.
export function gatedKindsFor(opts = {}) {
  if (String(opts.agent || DEFAULT_AGENT) === DEFAULT_AGENT) return CLAUDE_GATED_KINDS;
  const base = basename(String(opts.agent || ''));
  if (base === 'agy' || base === 'gemini') return getAdapter(opts).gatedKinds;
  return GATED_KINDS;
}

export function resolveAgentCommand(opts = {}) {
  const adapter = getAdapter(opts);
  return adapter.resolveCommand(opts);
}

export function isolatedAgyDir(opts = {}, root = tmpdir()) {
  if (basename(String(opts.agent || '')) !== 'agy') return '';
  return agyAdapter.isolatedSettingsDir(opts, root);
}

export function claudeSessionMeta(opts = {}) {
  return claudeAdapter.sessionMeta(opts);
}

// unaskedToolCall tracks tool calls and reports one of a gated kind that
// completed without a permission request. It returns a description or ''.
export function unaskedToolCall(update, kinds, asked, gated = GATED_KINDS) {
  if (update?.sessionUpdate !== 'tool_call' && update?.sessionUpdate !== 'tool_call_update') return '';
  const id = String(update.toolCallId || '');
  // Later updates often omit kind and title: remember them per call.
  const seen = kinds.get(id) || { kind: '', title: '' };
  if (update.kind) seen.kind = update.kind;
  if (update.title || update.name) seen.title = String(update.title || update.name);
  kinds.set(id, seen);
  if (update.status !== 'completed' || !gated.has(seen.kind) || asked.has(id)) return '';
  return `${seen.kind} call "${(seen.title || id).slice(0, 120)}"`;
}

// tokenSummary formats an ACP prompt result's usage (agy reports tokens but no
// USD cost), or returns '' when there is none.
export function tokenSummary(usage) {
  if (!usage || typeof usage !== 'object') return '';
  const n = (k) => (Number.isFinite(Number(usage[k])) ? Number(usage[k]) : 0);
  const total = n('totalTokens') || n('inputTokens') + n('outputTokens') + n('thoughtTokens');
  if (!total) return '';
  return `${total} tokens (${n('inputTokens')} in, ${n('outputTokens')} out, ${n('thoughtTokens')} thinking, ${n('cachedReadTokens')} cached)`;
}

function run(cmd, args, opts = {}) {
  return new Promise((resolveRun, reject) => {
    execFile(cmd, args, { encoding: 'utf8', maxBuffer: 16 << 20, ...opts }, (err, stdout, stderr) => {
      if (err) return reject(Object.assign(new Error(`${cmd} ${args.join(' ')}: ${(stderr || err.message).trim()}`), { cause: err }));
      resolveRun(stdout.trim());
    });
  });
}
const git = (repo, ...args) => run('git', ['-C', repo, ...args]);

export function parseArgs(argv, env = process.env) {
  const o = {
    repo: '', base: 'main', worktrees: '', noTty: false, agent: DEFAULT_AGENT, agentArgs: [], model: '',
    irrigator: env.IRRIGATOR_BIN || 'irrigator', runRoot: env.IRRIGATOR_RUN_ROOT || '',
    permissionTimeout: Number(env.IRRIGATOR_ACP_PERMISSION_TIMEOUT_S || 300), pollMs: 500,
  };
  const need = (i) => { if (i >= argv.length) throw new Error(`${argv[i - 1]} needs a value`); return argv[i]; };
  for (let i = 0; i < argv.length; i++) {
    switch (argv[i]) {
      case '--repo': o.repo = need(++i); break;
      case '--base': o.base = need(++i); break;
      case '--worktrees': o.worktrees = need(++i); break;
      case '--agent': o.agent = need(++i); break;
      case '--agent-arg': o.agentArgs.push(need(++i)); break;
      case '--model': o.model = need(++i); break;
      case '--irrigator': o.irrigator = need(++i); break;
      case '--run-root': o.runRoot = need(++i); break;
      case '--permission-timeout': o.permissionTimeout = Number(need(++i)); break;
      case '--poll-ms': o.pollMs = Number(need(++i)); break;
      case '--no-tty': o.noTty = true; break;
      default: throw new Error(`unknown argument ${argv[i]}`);
    }
  }
  if (!o.repo) throw new Error('--repo is required');
  if (!(o.permissionTimeout > 0) || !(o.pollMs > 0)) throw new Error('timeouts must be positive numbers');
  return o;
}

// visibleText makes agent-controlled text safe for a terminal. Tab, newline and
// carriage return become spaces (progress is one line); every other Unicode control
// or format character (Cc, Cf: C0/C1 including ESC so OSC 52 and other escape
// sequences cannot start, bidi marks and overrides, zero-width and joiner characters)
// and the line and paragraph separators (Zl, Zp) is shown as a visible <U+XXXX>
// marker, so nothing is dropped silently and legitimate text is otherwise unchanged.
export const visibleText = (s) => String(s)
  .replace(/[\t\n\r]/g, ' ')
  .replace(/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu, (c) => `<U+${c.codePointAt(0).toString(16).toUpperCase().padStart(4, '0')}>`);

class Progress {
  constructor(enabled) {
    this.fd = null;
    if (enabled) {
      try { this.fd = openSync('/dev/tty', 'w'); } catch { /* no terminal */ }
    }
  }
  say(cell, text) {
    const line = visibleText(`[${cell}] ${text}`);
    process.stderr.write(line + '\n');
    if (this.fd !== null) {
      try { writeSync(this.fd, line.slice(0, 220) + '\n'); } catch { this.close(); }
    }
  }
  close() {
    if (this.fd !== null) { try { closeSync(this.fd); } catch { /* ignore */ } this.fd = null; }
  }
}

// formatUsage maps an ACP prompt result usage object to the swarm.worker/v1
// canonical usage structure.
export function formatUsage(usage, model) {
  if (!usage || typeof usage !== 'object') return undefined;
  const n = (k) => {
    const v = usage[k];
    const num = Number(v);
    return Number.isFinite(num) && num >= 0 ? Math.floor(num) : 0;
  };
  const input_tokens = n('inputTokens') || n('input_tokens');
  const output_tokens = n('outputTokens') || n('output_tokens');
  const thinking_tokens = n('thoughtTokens') || n('thinkingTokens') || n('thinking_tokens');
  const cache_read_tokens = n('cachedReadTokens') || n('cacheReadTokens') || n('cache_read_tokens');
  const cache_write_tokens = n('cachedWriteTokens') || n('cacheWriteTokens') || n('cache_write_tokens');
  let total_tokens = n('totalTokens') || n('total_tokens');
  if (!total_tokens) {
    total_tokens = input_tokens + output_tokens + thinking_tokens + cache_read_tokens + cache_write_tokens;
  }
  const out = {
    input_tokens,
    output_tokens,
    thinking_tokens,
    cache_read_tokens,
    cache_write_tokens,
    total_tokens,
  };
  const m = model || (typeof usage.model === 'string' ? usage.model : undefined);
  if (m) out.model = String(m);
  return out;
}

export function respond(req, outcome, { output, error = '', cost, usage, model } = {}) {
  const msg = { protocol: PROTOCOL, request_id: req.request_id || '', outcome };
  if (output) msg.output = output;
  if (error) msg.error = error;
  if (cost !== undefined && cost !== null) msg.cost_usd = Math.round(Number(cost) * 1e6) / 1e6;
  if (usage) msg.usage = usage;
  if (model) msg.model = model;
  process.stdout.write(JSON.stringify(msg));
}

export function promptFor(req, worktree, branch, base) {
  const parts = [(req.task || {}).prompt || ''];
  for (const [name, value] of Object.entries(req.context || {}).sort(([a], [b]) => (a < b ? -1 : 1))) {
    parts.push(`\n## Context: ${name}\n${value}`);
  }
  for (const [dep, fields] of Object.entries(req.dependencies || {}).sort(([a], [b]) => (a < b ? -1 : 1))) {
    parts.push(`\n## Output of dependency ${dep}\n${JSON.stringify(fields, null, 2)}`);
  }
  parts.push(`
## Working rules
- You are in a git worktree at ${worktree} on branch ${branch}, created from ${base}.
  Only change files inside it. Do not push, reset, or switch branches.
- Read and follow the repository's CLAUDE.md, AGENTS.md and CONTRIBUTING.md if present.
- Keep the change focused on this task. Add or update tests for behavior you change.
- Every edit and command needs a person's approval; keep requests few and focused.
- Commit your work with a clear message. Do not add any Co-Authored-By trailer.
- Finish with a short summary: what changed, which tests ran and their result, and anything left undone.`);
  return parts.join('\n');
}

// isDenied reports why a tool call is refused without asking anyone, mirroring
// the coder worker's disallowed tools. Returns '' when the call may be put to a person.
export function isDenied(toolCall) {
  const title = String(toolCall?.title || '');
  const toolName = String(toolCall?.toolName || toolCall?.name || toolCall?._meta?.claudeCode?.toolName || '');
  const raw = toolCall?.rawInput && typeof toolCall.rawInput === 'object' ? toolCall.rawInput : {};
  const command = String(raw.command || raw.CommandLine || raw.cmd || '').trim();
  for (const tool of DENIED_TOOLS) {
    if (title === tool || title.startsWith(tool + ' ') || toolName === tool) return tool;
  }
  if (toolCall?.kind === 'fetch') return 'network fetch';
  // Each shell segment is checked, so "cd x && git push" is still caught.
  for (const segment of command.split(/&&|\|\||;|\||\n/)) {
    const words = segment.trim().split(/\s+/);
    for (const denied of DENIED_COMMANDS) {
      const want = denied.split(' ');
      if (want.every((w, i) => words[i] === w)) return denied;
    }
  }
  return '';
}

const b64urlJSON = (s) => JSON.parse(Buffer.from(s, 'base64url').toString('utf8'));

// parseLink extracts the path and the single-use nonce from `approvals link` output.
export function parseLink(stdout) {
  const link = stdout.trim().split('\n').pop().trim();
  const m = /^\/(approve|deny)\/([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)$/.exec(link);
  if (!m) throw new Error('unexpected output from irrigator approvals link');
  const nonce = b64urlJSON(m[2]).n;
  if (!nonce) throw new Error('approval link carries no nonce');
  return { path: link, nonce };
}

// readDecision looks for a ledger record answering one of the minted links.
// A deny anywhere wins over an approve.
export function readDecision(ledgerPath, request, nonces) {
  if (!existsSync(ledgerPath)) return '';
  let decision = '';
  for (const line of readFileSync(ledgerPath, 'utf8').split('\n')) {
    if (!line.trim()) continue;
    let rec;
    try { rec = JSON.parse(line); } catch { continue; }
    if (rec.request !== request || !nonces.has(rec.nonce)) continue;
    if (rec.decision === 'deny') return 'deny';
    if (rec.decision === 'approve') decision = 'approve';
  }
  return decision;
}

function sleep(ms, signal) {
  return new Promise((done) => {
    if (signal?.aborted) return done();
    const t = setTimeout(done, ms);
    signal?.addEventListener('abort', () => { clearTimeout(t); done(); }, { once: true });
  });
}

const cleanText = (s) => String(s).replace(/[^\x20-\x7e]/g, ' ').slice(0, 200);

// configuredNotifier names the run's notifier (ntfy or webhook) when one is
// set up: IRRIGATOR_NOTIFY_VIA, else the choice recorded in RUN/notify.json.
export function configuredNotifier(runRoot) {
  const ok = (v) => (v === 'ntfy' || v === 'webhook' ? v : '');
  const env = process.env.IRRIGATOR_NOTIFY_VIA || '';
  if (env) return ok(env);
  try {
    return ok(JSON.parse(readFileSync(join(runRoot, 'notify.json'), 'utf8')).via);
  } catch {
    return '';
  }
}

// ntfyServer applies the Go notifier's destination rules to IRRIGATOR_NTFY_SERVER:
// http(s) only, no credentials in the URL, plain http only for loopback. It returns
// the normalised base URL, or '' when the value is not acceptable.
export function ntfyServer(raw) {
  let u;
  try { u = new URL(raw); } catch { return ''; }
  if ((u.protocol !== 'https:' && u.protocol !== 'http:') || u.username || u.password) return '';
  if (u.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(u.hostname)) return '';
  return u.toString().replace(/\/+$/, '');
}

async function notify(ctx, title, body, links) {
  const topic = process.env.IRRIGATOR_NTFY_TOPIC;
  const base = (process.env.IRRIGATOR_APPROVALS_BASE_URL || '').replace(/\/+$/, '');
  if (!topic || !base) return;
  const server = ntfyServer(process.env.IRRIGATOR_NTFY_SERVER || 'https://ntfy.sh');
  if (!server) { ctx.say('notification skipped (IRRIGATOR_NTFY_SERVER must be https, or http on loopback, without credentials)'); return; }
  const clean = cleanText;
  try {
    const res = await fetch(`${server}/${encodeURIComponent(topic)}`, {
      method: 'POST',
      headers: {
        Title: clean(title),
        Actions: `http, Approve, ${base}${links.approve}, method=POST; http, Deny, ${base}${links.deny}, method=POST`,
      },
      body: clean(body),
      redirect: 'error', // approval links must not follow a redirect to another host
      signal: AbortSignal.timeout(10000),
    });
    if (!res.ok) ctx.say(`notification failed (HTTP ${res.status})`);
  } catch (e) {
    ctx.say(`notification failed (${e.name})`);
  }
}

// decidePermission answers one session/request_permission. It returns the option
// to select, or null to cancel. Every failure path rejects.
export async function decidePermission(params, env) {
  const { cell, say, opts, runRoot, signal } = env;
  const options = params.options || [];
  const reject = options.find((o) => o.kind === 'reject_once') || options.find((o) => String(o.kind).startsWith('reject'));
  const allow = options.find((o) => o.kind === 'allow_once');
  const title = String(params.toolCall?.title || 'tool call');
  const denied = isDenied(params.toolCall);
  if (denied) {
    say(`denied without asking: ${denied}`);
    return reject || null;
  }
  const request = env.request;
  const ttl = `${Math.ceil(opts.permissionTimeout) + 30}s`;
  let links;
  const via = configuredNotifier(runRoot);
  if (via) {
    // The run's notifier mints the links and sends them; its --json answer
    // carries the nonces to wait on. If it fails, fall back to plain links.
    try {
      const args = ['approvals', 'notify', '--json', '--run', runRoot, '--request', request, '--via', via, '--ttl', ttl,
        '--title', cleanText(`Irrigator approval: ${cell}`), '--message', cleanText(title)];
      try {
        const notifRec = JSON.parse(readFileSync(join(runRoot, 'notify.json'), 'utf8'));
        if (via === 'webhook' && notifRec.url) args.push('--url', notifRec.url);
        if (via === 'ntfy' && notifRec.topic) args.push('--topic', notifRec.topic);
      } catch { /* ignore */ }
      const base = process.env.IRRIGATOR_APPROVALS_BASE_URL;
      if (base) args.push('--public-url', base);
      const sent = JSON.parse(await run(opts.irrigator, args));
      if (!sent.approve_nonce || !sent.deny_nonce) throw new Error('approvals notify returned no nonces');
      links = { approve: { nonce: sent.approve_nonce }, deny: { nonce: sent.deny_nonce } };
    } catch (e) {
      say(`notification via ${via} failed, waiting without it: ${e.message.split('\n')[0].slice(0, 160)}`);
    }
  }
  if (!links) {
    try {
      links = {};
      for (const action of ['approve', 'deny']) {
        const out = await run(opts.irrigator, ['approvals', 'link', '--path-only', '--run', runRoot, '--request', request, '--action', action, '--ttl', ttl]);
        links[action] = parseLink(out);
      }
    } catch (e) {
      say(`cannot mint approval links, rejecting: ${e.message.split('\n')[0].slice(0, 160)}`);
      return reject || null;
    }
    say(`waiting for approval: ${title}`);
    if (!via) await notify({ say }, `Irrigator approval: ${cell}`, title, { approve: links.approve.path, deny: links.deny.path });
  } else {
    say(`waiting for approval: ${title}`);
  }
  const nonces = new Set([links.approve.nonce, links.deny.nonce]);
  const ledger = join(runRoot, 'approvals', 'decisions.jsonl');
  const deadline = Date.now() + opts.permissionTimeout * 1000;
  let decision = '';
  while (!signal.aborted) {
    decision = readDecision(ledger, request, nonces);
    if (decision || Date.now() >= deadline) break;
    await sleep(Math.min(opts.pollMs, Math.max(1, deadline - Date.now())), signal);
  }
  if (decision === 'approve' && allow) {
    say(`approved: ${title}`);
    return allow;
  }
  say(decision === 'approve' ? 'approved but the agent offered no allow_once option; rejecting' : decision === 'deny' ? `denied: ${title}` : `no decision in time, rejecting: ${title}`);
  return reject || null;
}

function describe(update, worktree) {
  if (update.sessionUpdate === 'tool_call') {
    return `▸ ${String(update.title || update.kind || 'tool').replace(join(worktree, ''), '')}`;
  }
  return null;
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  let raw = '';
  for await (const chunk of process.stdin) raw += chunk;
  const req = JSON.parse(raw);
  const cell = (req.task || {}).id || 'cell';
  if (req.kind !== 'task') return respond(req, 'not_started', { error: 'ACP worker only runs tasks, not validators' });

  const progress = new Progress(!opts.noTty);
  const say = (text) => progress.say(cell, text);
  try {
    await runCell(opts, req, cell, say);
  } finally {
    progress.close();
  }
}

// branchWorktrees returns paths of worktrees that have `branch` checked out.
export async function branchWorktrees(repo, branch) {
  const paths = [];
  let current = null;
  const list = await git(repo, 'worktree', 'list', '--porcelain');
  for (const line of list.split('\n')) {
    if (line.startsWith('worktree ')) {
      current = line.slice('worktree '.length).trim();
    } else if (line === `branch refs/heads/${branch}` && current) {
      paths.push(current);
    }
  }
  return paths;
}

// retireEarlierAttempts frees the branch from earlier attempts' worktrees so
// a retry can check it out in a new worktree.
export async function retireEarlierAttempts(repo, branch) {
  await git(repo, 'worktree', 'prune');
  const realRepo = await git(repo, 'rev-parse', '--show-toplevel');
  for (const p of await branchWorktrees(repo, branch)) {
    let realPath;
    try { realPath = await git(p, 'rev-parse', '--show-toplevel'); } catch { continue; }
    if (realPath === realRepo) {
      throw new Error(`branch ${branch} is checked out in the main repository at ${p}`);
    }
    const status = await git(p, 'status', '--porcelain');
    if (status) {
      // Keep the earlier attempt's work on the branch. If it cannot be
      // committed, stop: removing the worktree would lose it.
      try {
        await git(p, 'add', '-A');
        await git(p, 'commit', '-q', '-m', `irrigator cell ${branch}: uncommitted work from earlier attempt`);
      } catch (e) {
        throw new Error(`earlier attempt left uncommitted work in ${p} that could not be committed (${e.message}); inspect it before retrying`);
      }
    }
    await git(repo, 'worktree', 'remove', p);
  }
}

async function runCell(opts, req, cell, say) {
  const run8 = (req.run_id || 'run').slice(0, 8);
  const branch = `irrigator/${run8}/${cell}`;
  const root = opts.worktrees || join(dirname(resolve(opts.repo)), 'irrigator-worktrees');
  const worktree = join(root, run8, `${cell}-a${req.attempt || 1}`);
  let baseSha;
  try {
    baseSha = await git(opts.repo, 'rev-parse', '--verify', opts.base + '^{commit}');
    if (await git(opts.repo, 'branch', '--list', branch)) {
      await retireEarlierAttempts(opts.repo, branch);
      await git(opts.repo, 'worktree', 'add', worktree, branch); // a retry continues the cell's branch
    } else {
      await git(opts.repo, 'worktree', 'add', '-b', branch, worktree, baseSha);
    }
  } catch (e) {
    return respond(req, 'not_started', { error: `worktree setup failed: ${e.message}` });
  }

  const budget = Math.max(0.01, Number(req.max_cost_usd) || 1);
  const seconds = Math.max(1, (req.timeout_ms || 1800000) / 1000 - 30);
  // The run root is two directories above the worker's cwd (<run>/<x>/<y>).
  const runRoot = opts.runRoot || resolve(process.cwd(), '..', '..');

  const state = { cost: null, text: '', timedOut: false, overBudget: false, started: false, sessionId: '', unasked: '' };
  // Every agent must ask before gated calls. The bundled Claude adapter runs
  // with no user, project or local settings (see claudeSessionMeta), so the
  // user's own allow rules cannot approve anything on Irrigator's behalf.
  const enforceAsk = true;
  const gated = gatedKindsFor(opts);
  const asked = new Set();
  const kinds = new Map();
  if (basename(String(opts.agent || '')) === 'agy') {
    const v = await agyAdapter.checkVersion(opts.agent || 'agy');
    if (!v.ok) {
      return respond(req, 'not_started', { error: v.error });
    }
  }
  let geminiDir = '';
  try {
    geminiDir = isolatedAgyDir(opts);
  } catch (e) {
    return respond(req, 'not_started', { error: `could not prepare agy settings: ${e.message}` });
  }
  const launch = resolveAgentCommand({ ...opts, geminiDir });
  const child = spawn(launch.command, launch.args, { cwd: worktree, stdio: ['pipe', 'pipe', 'ignore'] });
  const launched = await new Promise((done) => {
    child.once('spawn', () => done(null));
    child.once('error', done);
  });
  if (launched) return respond(req, 'not_started', { error: `could not launch ACP agent: ${launched.message}` });
  child.on('error', () => {});
  const timer = setTimeout(() => { state.timedOut = true; child.kill('SIGKILL'); }, seconds * 1000);
  const stream = ndJsonStream(Writable.toWeb(child.stdin), Readable.toWeb(child.stdout));

  const app = client({ name: 'irrigator-acp-worker' })
    .onRequest(methods.client.session.requestPermission, async ({ params, signal }) => {
      if (params.toolCall?.toolCallId) asked.add(String(params.toolCall.toolCallId));
      const chosen = await decidePermission(params, { cell, say, opts, runRoot, signal, request: cell });
      return { outcome: chosen ? { outcome: 'selected', optionId: chosen.optionId } : { outcome: 'cancelled' } };
    })
    .onNotification(methods.client.session.update, ({ params, agent }) => {
      const u = params.update;
      if (u.sessionUpdate === 'usage_update' && u.cost && u.cost.currency === 'USD' && Number.isFinite(u.cost.amount)) {
        state.cost = u.cost.amount;
        if (u.cost.amount > budget && !state.overBudget) {
          state.overBudget = true;
          say(`over the soft budget of $${budget.toFixed(2)}, cancelling`);
          agent.notify(methods.agent.session.cancel, { sessionId: params.sessionId }).catch(() => {});
        }
      }
      if (u.sessionUpdate === 'agent_message_chunk' && u.content?.type === 'text') state.text += u.content.text;
      const unasked = enforceAsk && !state.unasked ? unaskedToolCall(u, kinds, asked, gated) : '';
      if (unasked) {
        state.unasked = unasked;
        say(`agent ran the ${unasked} without asking; stopping`);
        agent.notify(methods.agent.session.cancel, { sessionId: params.sessionId }).catch(() => {});
        child.kill('SIGTERM');
      }
      const line = describe(u, worktree);
      if (line) say(line);
    });

  let stop = null;
  let failure = null;
  try {
    stop = await app.connectWith(stream, async (ctx) => {
      await ctx.request(methods.agent.initialize, {
        protocolVersion: PROTOCOL_VERSION,
        clientCapabilities: { fs: { readTextFile: false, writeTextFile: false }, terminal: false },
      });
      const session = await ctx.request(methods.agent.session.new, { cwd: worktree, mcpServers: [], ...claudeSessionMeta(opts) });
      const modes = session.modes;
      if (modes) {
        if (!modes.availableModes?.some((m) => m.id === 'default')) {
          throw Object.assign(new Error('agent offers no default (ask-first) permission mode'), { notStarted: true });
        }
        if (modes.currentModeId !== 'default') {
          await ctx.request(methods.agent.session.setMode, { sessionId: session.sessionId, modeId: 'default' });
        }
      }
      if (opts.model && !Object.keys(claudeSessionMeta(opts)).length) {
        // Other agents choose a model through the standard ACP config option.
        const option = (session.configOptions || []).find((o) => o.id === 'model' || o.category === 'model');
        if (!option) throw Object.assign(new Error(`agent offers no model option; cannot use --model ${opts.model}`), { notStarted: true });
        const values = (option.options || []).map((o) => o.value ?? o.id).filter(Boolean);
        if (values.length && !values.includes(opts.model)) {
          throw Object.assign(new Error(`model ${opts.model} is not offered; choose one of: ${values.join(', ')}`), { notStarted: true });
        }
        await ctx.request(methods.agent.session.setConfigOption, { sessionId: session.sessionId, configId: option.id, value: opts.model });
      }
      state.sessionId = session.sessionId;
      say(`start · ${branch} · soft budget $${budget.toFixed(2)}`);
      state.started = true;
      const res = await ctx.request(methods.agent.session.prompt, {
        sessionId: session.sessionId,
        prompt: [{ type: 'text', text: promptFor(req, worktree, branch, opts.base) }],
      });
      state.usage = res.usage || null;
      return res.stopReason;
    });
  } catch (e) {
    failure = e;
  } finally {
    clearTimeout(timer);
    const exited = child.exitCode !== null || child.signalCode !== null
      ? Promise.resolve()
      : new Promise((done) => child.once('exit', done));
    child.kill('SIGTERM');
    if (geminiDir) {
      // agy keeps writing into its settings directory until it exits, so wait
      // briefly; removal is best effort and never changes the cell's outcome.
      await Promise.race([exited, new Promise((done) => setTimeout(done, 3000))]);
      try {
        rmSync(geminiDir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
      } catch (e) {
        say(`could not remove agy settings directory ${geminiDir}: ${e.code || e.message}`);
      }
    }
  }
  const cost = state.cost;
  const tokens = tokenSummary(state.usage);
  const usage = formatUsage(state.usage, opts.model);
  const model = (usage && usage.model) || opts.model || undefined;
  if (state.started && cost === null) say(`agent reported no cost; the soft budget could not be enforced${tokens ? ` · ${tokens}` : ''}`);
  if (state.unasked) {
    // It already ran: the effect is real but unapproved, so never settle it.
    return respond(req, 'uncertain', { error: `agent ran the ${state.unasked} without asking for permission; its own permission settings bypass Irrigator's approvals`, cost, usage, model });
  }

  if (failure && !state.started) {
    return respond(req, 'not_started', { error: `ACP setup failed: ${String(failure.message).slice(0, 300)}`, cost, usage, model });
  }
  if (state.timedOut || failure || stop === null) {
    // The agent ran and may have changed files; never claim a clean outcome.
    say(state.timedOut ? 'timed out' : 'agent ended without a result');
    const why = state.timedOut ? 'agent timed out' : `agent ended without a result${failure ? `: ${String(failure.message).slice(0, 200)}` : ''}`;
    return respond(req, 'uncertain', { error: why, cost, usage, model });
  }

  let output;
  let commits;
  try {
    if (await git(worktree, 'status', '--porcelain')) {
      await git(worktree, 'add', '-A');
      await git(worktree, 'commit', '-q', '-m', `irrigator cell ${cell}: uncommitted agent changes`);
      say('committed leftover changes');
    }
    commits = Number((await git(worktree, 'rev-list', '--count', `${baseSha}..HEAD`)) || 0);
    output = {
      branch,
      commit: await git(worktree, 'rev-parse', 'HEAD'),
      diffstat: (await git(worktree, 'diff', '--shortstat', `${baseSha}...HEAD`)) || 'no changes',
      summary: (state.text.trim().slice(0, 4000) || '(no summary)') + (cost === null && tokens ? `\n\nUsage: ${tokens}; no USD cost was reported.` : ''),
    };
  } catch (e) {
    return respond(req, 'uncertain', { error: `agent finished but git failed: ${e.message}`, cost, usage, model });
  }
  const wanted = (req.task || {}).requires || Object.keys(output);
  output = Object.fromEntries(wanted.map((k) => [k, output[k] ?? output.summary]));
  const spent = cost === null ? (tokens || 'cost unknown') : `$${cost.toFixed(2)}`;
  if (state.overBudget) return respond(req, 'settled', { error: `agent stopped: soft budget of $${budget.toFixed(2)} exceeded`, cost, usage, model });
  if (stop !== 'end_turn') {
    say(`failed · ${stop} · ${spent}`);
    return respond(req, 'settled', { error: `agent stopped: ${stop}`, cost, usage, model });
  }
  if (commits === 0) {
    say(`no changes · ${spent}`);
    return respond(req, 'settled', { error: 'agent finished without changing anything', cost, usage, model });
  }
  say(`done · ${commits} commit(s) · ${output.diffstat || ''} · ${spent}`);
  respond(req, 'settled', { output, cost, usage, model });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((e) => {
    process.stderr.write(`acp worker: ${visibleText(e.message)}\n`);
    process.exitCode = 1;
  });
}
