export const GEMINI_GATED_KINDS = new Set(['execute', 'edit', 'delete', 'move', 'fetch']);

export function resolveCommand(opts = {}) {
  const agent = String(opts.agent || 'gemini');
  const extra = Array.isArray(opts.agentArgs) ? opts.agentArgs.map(String) : [];
  if (extra.length === 0) {
    return { command: agent, args: ['--acp'] };
  }
  return { command: agent, args: [...extra] };
}

// The model is chosen through the standard ACP model config option in the
// worker, so Gemini needs no session metadata.
export const geminiAdapter = {
  name: 'gemini',
  gatedKinds: GEMINI_GATED_KINDS,
  resolveCommand,
};

export default geminiAdapter;
