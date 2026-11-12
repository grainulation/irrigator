// Fake ACP agent for worker tests: no network, no model. Behavior comes from
// FAKE_SCENARIO; what it saw goes to FAKE_LOG as JSON lines.
import { appendFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { Readable, Writable } from 'node:stream';
import { agent, methods, ndJsonStream, PROTOCOL_VERSION } from '@agentclientprotocol/sdk/unstable';

const scenario = process.env.FAKE_SCENARIO || 'edit';
const log = (o) => process.env.FAKE_LOG && appendFileSync(process.env.FAKE_LOG, JSON.stringify(o) + '\n');
const options = [
  { optionId: 'allow', name: 'Allow', kind: 'allow_once' },
  { optionId: 'always', name: 'Always allow', kind: 'allow_always' },
  { optionId: 'reject', name: 'Reject', kind: 'reject_once' },
];
const usage = (client, sessionId, amount) =>
  client.notify(methods.client.session.update, {
    sessionId,
    update: { sessionUpdate: 'usage_update', used: 10, size: 1000, cost: { amount, currency: 'USD' } },
  });

const app = agent({ name: 'fake-agent' })
  .onRequest(methods.agent.initialize, () => ({ protocolVersion: PROTOCOL_VERSION, agentCapabilities: {} }))
  .onRequest(methods.agent.session.new, () => ({
    sessionId: 's1',
    configOptions: [{ id: 'model', name: 'Model', category: 'model', type: 'select', currentValue: 'fast',
      options: [{ value: 'fast', name: 'Fast' }, { value: 'deep', name: 'Deep' }] }],
    modes: {
      currentModeId: 'bypassPermissions',
      availableModes: [
        { id: 'default', name: 'Default' },
        { id: 'plan', name: 'Plan' },
        { id: 'bypassPermissions', name: 'Bypass' },
      ],
    },
  }))
  .onRequest(methods.agent.session.setConfigOption, ({ params }) => {
    log({ setConfig: [params.configId, params.value] });
    return { configOptions: [] };
  })
  .onRequest(methods.agent.session.setMode, ({ params }) => {
    log({ setMode: params.modeId });
    return {};
  })
  .onRequest(methods.agent.session.prompt, async ({ params, client }) => {
    const sessionId = params.sessionId;
    await usage(client, sessionId, 0.1);
    if (scenario === 'crash') {
      await usage(client, sessionId, 0.42);
      await new Promise((r) => setTimeout(r, 50));
      process.exit(3);
    }
    const toolUpdate = (update) => client.notify(methods.client.session.update, { sessionId, update });
    if (scenario === 'unasked') {
      // An agent whose own settings auto-approve: it runs a command without asking.
      await toolUpdate({ sessionUpdate: 'tool_call', toolCallId: 'u1', title: 'touch sneaky.txt', kind: 'execute', status: 'in_progress' });
      writeFileSync(join(process.cwd(), 'sneaky.txt'), 'x\n');
      await toolUpdate({ sessionUpdate: 'tool_call_update', toolCallId: 'u1', status: 'completed' });
      await new Promise((r) => setTimeout(r, 500));
      return { stopReason: 'end_turn' };
    }
    // Reads never ask; agy reports a gated call as in_progress before asking.
    await toolUpdate({ sessionUpdate: 'tool_call', toolCallId: 'r1', title: 'read notes', kind: 'read', status: 'in_progress' });
    await toolUpdate({ sessionUpdate: 'tool_call_update', toolCallId: 'r1', status: 'completed' });
    await toolUpdate({ sessionUpdate: 'tool_call', toolCallId: 't1', title: 'pending call', kind: scenario === 'denycmd' ? 'execute' : 'edit', status: 'in_progress' });
    const command = scenario === 'denycmd' ? 'cd x && git push origin HEAD' : undefined;
    const res = await client.request(methods.client.session.requestPermission, {
      sessionId,
      toolCall: command
        ? { toolCallId: 't1', title: 'Bash', kind: 'execute', rawInput: { command } }
        : { toolCallId: 't1', title: 'Write hello.txt', kind: 'edit', content: [{ type: 'diff', path: 'hello.txt', oldText: null, newText: 'hello\n' }] },
      options,
    });
    log({ outcome: res.outcome });
    if (res.outcome.outcome === 'selected' && res.outcome.optionId === 'allow') {
      writeFileSync(join(process.cwd(), 'hello.txt'), 'hello\n');
      await toolUpdate({ sessionUpdate: 'tool_call_update', toolCallId: 't1', status: 'completed' });
    } else {
      await toolUpdate({ sessionUpdate: 'tool_call_update', toolCallId: 't1', status: 'failed' });
    }
    await usage(client, sessionId, 0.25);
    await client.notify(methods.client.session.update, {
      sessionId,
      update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: 'all done' } },
    });
    return { stopReason: 'end_turn' };
  });
app.connect(ndJsonStream(Writable.toWeb(process.stdout), Readable.toWeb(process.stdin)));
