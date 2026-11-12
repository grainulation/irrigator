import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
export const DEFAULT_AGENT = join(here, '..', 'node_modules', '@agentclientprotocol', 'claude-agent-acp', 'dist', 'index.js');

// Claude runs read-only shell commands without asking; edits, deletes, moves, and fetches require review.
export const CLAUDE_GATED_KINDS = new Set(['edit', 'delete', 'move', 'fetch']);

export function resolveCommand(opts = {}) {
  const agent = String(opts.agent || DEFAULT_AGENT);
  const extra = Array.isArray(opts.agentArgs) ? opts.agentArgs.map(String) : [];
  if (/\.(?:mjs|cjs|js)$/i.test(agent)) {
    return { command: process.execPath, args: [agent, ...extra] };
  }
  return { command: agent, args: [...extra] };
}

// claudeSessionMeta makes the bundled Claude adapter ignore user settings files.
// Other agents return {} because their isolation is handled independently.
export function sessionMeta(opts = {}) {
  if (String(opts.agent || DEFAULT_AGENT) !== DEFAULT_AGENT) return {};
  const options = { settingSources: [] };
  if (opts.model) options.model = String(opts.model);
  return { _meta: { claudeCode: { options } } };
}

export const claudeAdapter = {
  name: 'claude',
  defaultAgent: DEFAULT_AGENT,
  gatedKinds: CLAUDE_GATED_KINDS,
  resolveCommand,
  sessionMeta,
};

export default claudeAdapter;
