#!/usr/bin/env node
// Stands in for `irrigator approvals link` and `approvals notify --json`: prints
// output shaped like the real commands and records the arguments and nonces in
// $FAKE_MINTED (one row per minted link). FAKE_NOTIFY_FAIL=1 makes notify fail.
import { appendFileSync } from 'node:fs';
import { randomBytes } from 'node:crypto';

const argv = process.argv.slice(2);
const mint = (action) => {
  const nonce = randomBytes(8).toString('hex');
  appendFileSync(process.env.FAKE_MINTED, JSON.stringify({ argv, action, nonce }) + '\n');
  return { action, nonce, claims: Buffer.from(JSON.stringify({ a: action, n: nonce })).toString('base64url') };
};

if (argv[1] === 'notify') {
  if (process.env.FAKE_NOTIFY_FAIL) {
    console.error('notification via ntfy failed: server answered 500');
    process.exit(1);
  }
  const approve = mint('approve');
  const deny = mint('deny');
  console.log(JSON.stringify({
    approve_url: `https://fake.example/approve/${approve.claims}.sig`,
    deny_url: `https://fake.example/deny/${deny.claims}.sig`,
    approve_nonce: approve.nonce,
    deny_nonce: deny.nonce,
    expires_at: '2030-01-01T00:00:00Z',
  }));
} else {
  const link = mint(argv[argv.indexOf('--action') + 1]);
  console.log(`/${link.action}/${link.claims}.sig`);
}
