import { basename } from 'node:path';
import { claudeAdapter } from './claude.mjs';
import { agyAdapter } from './agy.mjs';
import { geminiAdapter } from './gemini.mjs';

export { claudeAdapter } from './claude.mjs';
export { agyAdapter } from './agy.mjs';
export { geminiAdapter } from './gemini.mjs';

export function getAdapter(opts = {}) {
  const agent = String(opts.agent || '');
  const base = basename(agent);
  if (base === 'agy') return agyAdapter;
  if (base === 'gemini') return geminiAdapter;
  return claudeAdapter;
}

export default getAdapter;
