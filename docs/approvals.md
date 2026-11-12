# Approvals from a phone

`irrigator approvals` lets a person approve or deny a blocked request by opening
a link on a phone. There is no Irrigator server: `approvals serve` is a small
local HTTP listener, optionally put behind a tunnel by a built-in adapter, and
every decision lands in the run directory.

```sh
irrigator approvals serve --run RUN --expose cloudflare     # or tailscale
irrigator approvals notify --run RUN --request TASK_ID --via ntfy
irrigator approvals link --run RUN --request TASK_ID --action approve --ttl 5m
# prints https://<public host>/approve/<token> (or /approve/<token> with no public URL)
```

`link` creates the per-run key on first use, and `serve` creates it too, so
either can run first. The request id is a task id of the run. `--ttl` defaults
to 5 minutes and is capped at 24 hours.

While `serve` runs with an exposure it records the public base URL in
`approvals/public_url` (mode 0600) and removes it on exit. `link` and `notify`
read that file; there is no other state.

`link` prints the full URL when that file exists, and otherwise the bare path.
`--path-only` always prints `/action/<token>` for scripts. On a terminal,
`IRRIGATOR_APPROVALS_URL` still wins: the printed path is then an OSC 8 link to
that base. Piped output is the bare path byte for byte unless a public URL file
exists.

## Behavior

- `serve` handles only `/approve/<token>` and `/deny/<token>`; everything else is 404.
- It refuses any `--addr` that is not a loopback IP (or `localhost`).
- `GET`/`HEAD` show a minimal confirm page (`Cache-Control: no-store`) and never
  decide, so link previewers, mail scanners and chat unfurlers cannot approve.
  Only `POST` decides.
- Responses never reveal more than a short reason: wrong action, bad signature,
  expired, already used, unknown request.

## Tokens

A token is `base64url(claims).base64url(HMAC-SHA256(key, claims))`. The claims
bind the run id, request id, action, expiry and a random nonce. The key is 32
random bytes in `approvals/key` inside the run directory, created exclusively
with mode 0600; a key with looser permissions or the wrong length is refused.

A token is single use. Consuming it appends the nonce to `approvals/used.jsonl`
(append-only, one write, fsync) under an exclusive file lock before the decision
is recorded, so a restart cannot reuse it and concurrent POSTs decide at most
once. If recording the decision then fails, the link stays spent and a new one
must be minted: a failure can lose a decision but never produce two. An approve
token cannot be replayed as deny, and editing any claim invalidates the MAC.

## Where the decision goes

The choice depends on the run:

- **Run supports the durable mailbox** (see
  [durable-mailbox.md](durable-mailbox.md)): the decision is enqueued as a
  `signal` message addressed to the request's current task identity
  (operation id, generation, attempt), with key `approval<nonce>` and payload
  `approval decision: approve|deny request ID`. The mailbox already provides
  atomic, ordered, idempotent, stale-checked records, so approvals reuse that
  path instead of adding a second one. A `signal` records a receipt only; it does
  not resume a cell, admit work, or replay anything. A decision that arrives
  after a retry or revision becomes a durable `stale` receipt. Linking fails for
  a request that is not a task of the run.
- **Run without a mailbox**: the decision is appended to the run-local ledger
  `approvals/decisions.jsonl`, one JSON line per decision, append-only with fsync.

In both cases the decision is a durable record for the run or a person to act on.
It does not by itself unblock, re-run or replay interrupted work. Likewise, a
review approval binds a workflow decision to the head that was reviewed; it is not
a security boundary and it never merges or applies anything.

## Security model

A link is a bearer capability: whoever holds an unexpired, unused link can make
that one decision for that one request. There is no login. Mitigations are the
short TTL, single use, the confirm-then-POST flow, and binding to one run,
request and action. Treat links like passwords: send them only to the approver,
and prefer a channel that does not unfurl or log URLs. The listener is plain
HTTP on loopback; confidentiality in transit comes from the tunnel. Anyone with
write access to the run directory can read the key and mint tokens, so the key
protects against the network, not against local users. If the key leaks, delete
`approvals/key`; outstanding links die with it.

## Exposure

`serve` always listens on loopback. `--expose` starts one tunnel in front of it
and records its public URL; the tunnel is stopped when `serve` exits.

```sh
irrigator approvals serve --run RUN                        # loopback only (--expose none)
irrigator approvals serve --run RUN --expose cloudflare    # quick tunnel
irrigator approvals serve --run RUN --expose cloudflare --cf-tunnel NAME --cf-hostname HOST
irrigator approvals serve --run RUN --expose tailscale
irrigator approvals serve --run RUN --public-url https://approve.example.com
```

**Cloudflare quick tunnel** (no account, throwaway URL) runs
`cloudflared tunnel --no-autoupdate --url http://127.0.0.1:PORT` and reads the
`https://*.trycloudflare.com` URL from its output. If none appears within 30
seconds, or `cloudflared` exits first, `serve` stops the tunnel and fails with
the tail of its output. The URL is unauthenticated and public while the tunnel
runs.

**Cloudflare named tunnel** (`--cf-tunnel NAME --cf-hostname HOST`, both
required) runs `cloudflared tunnel run NAME` and uses `https://HOST`. The
tunnel's own configuration must route HOST to the `serve` address; Irrigator
cannot verify that. Put a Cloudflare Access policy in front for teams, so
approvers sign in with your identity provider before a request reaches `serve`.
`serve` records the `Cf-Connecting-Ip` header as `via` in the ledger record for
audit only, never for authorization. Only Cloudflare sets it reliably; without
a Cloudflare tunnel any client can send its own value, so treat `via` as a hint.

**Tailscale** is reachable only from your tailnet, so the phone needs
Tailscale. `serve` runs `tailscale serve --bg --https=443 http://127.0.0.1:PORT`
and derives `https://<Self.DNSName>` from `tailscale status --json`. It refuses
to start if an https:443 handler already exists, rather than replace it. On
exit it removes only that handler with `tailscale serve --https=443 off`; if
that fails it prints the exact command for you to run, and never resets your
serve config. The CLI arguments are constants at the top of
`approvals_expose.go`, so they are easy to adjust if the Tailscale CLI changes.
Avoid `tailscale funnel` unless you want the link public.

**`--public-url URL`** records an origin you exposed yourself (any reverse
proxy or tunnel). It must be `https://host[:port]` with no path, query or
credentials. It cannot be combined with `--expose`.

If `serve` is killed without cleanup, a stale `approvals/public_url` can remain
until the next `serve` start, which clears it; delete it by hand if links point
at a dead tunnel. If the tunnel process dies while serving, `serve` exits with
an error.

### Manual fallback

The adapters only automate these commands; use them directly when you need
something else. Keep `serve` on loopback and put exactly one in front, then
append the link path to the public URL yourself (or pass `--public-url`).

```sh
cloudflared tunnel --url http://127.0.0.1:8787     # quick tunnel
tailscale serve --bg 8787                          # tailnet only
```

## Notifications

`approvals notify` mints an approve link and a deny link for one request and
sends them through a notifier. It needs a public URL, from `approvals/public_url`
or `--public-url`. The links are never printed; stdout only reports the send.

```sh
irrigator approvals notify --run RUN --request TASK_ID --via ntfy [--ttl 5m] [--title T] [--message M]
irrigator approvals notify --run RUN --request TASK_ID --via webhook --url https://hooks.example.com/x
```

`--json` prints `{"approve_url", "deny_url", "approve_nonce", "deny_nonce",
"expires_at"}` on stdout after a successful send. The URLs are live
capabilities, so use it only from a script you trust; the ACP worker uses the
nonces to wait on the decision ledger.

To notify about run status (blocked, complete, a cell uncertain or failed)
rather than a single approval, use `irrigator apply|resume --notify
ntfy|webhook|off` with `--notify-topic` and `--notify-url`; see
[Notifications for unattended runs](monitoring.md#notifications-for-unattended-runs).
Those messages carry no links.

If the send fails the command exits non-zero and says so, but it never
consumes or invalidates anything: tokens are stateless and single use, so the
undelivered links simply stay valid until they expire. Requests time out after
10 seconds and redirects are not followed. Error messages omit the topic and
webhook URL, which can themselves be secrets.

**ntfy**: server from `IRRIGATOR_NTFY_SERVER` (default `https://ntfy.sh`), topic
from `IRRIGATOR_NTFY_TOPIC` or `--topic`. A bearer token is read only from
`IRRIGATOR_NTFY_TOKEN`; there is deliberately no flag, so it never lands in
argv. The notification carries an `Actions` header with an **Approve** and a
**Deny** button that POST to the links (one tap decides, no confirm page) and an
**Open** action that opens the approve confirm page. Plain `http` is accepted
only for a loopback server. ntfy topics are readable by anyone who knows the
name, so `notify` warns when the topic is shorter than 20 characters; use a
long random topic or an authenticated self-hosted server. The tokens in the
notification are live capabilities. Once one button is used the other remains
valid until expiry, so approve and deny are independent; use a short TTL.

**webhook**: POSTs JSON to `--url` or `IRRIGATOR_APPROVALS_WEBHOOK`:

```json
{"run": "RUN_ID", "request": "TASK_ID", "approve_url": "...", "deny_url": "...",
 "expires_at": "2026-01-01T00:05:00Z", "title": "...", "message": "..."}
```

Any 2xx is success. The endpoint receives both live links, so use an HTTPS
endpoint you control.

### Manual ntfy fallback

```sh
curl -H "Actions: http, Approve, https://HOST/approve/TOKEN, method=POST; \
http, Deny, https://HOST/deny/TOKEN, method=POST" \
  -d "Request TASK_ID is blocked" https://ntfy.sh/YOUR_PRIVATE_TOPIC
```

Mint both links with `approvals link --path-only` first.
