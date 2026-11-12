# Cross-run admission campaigns

Campaigns reserve allowances across multiple local managed runs. They are not
vendor-enforced invoice caps. Existing CLI logins can report unknown USD; such
runs retain their full campaign allowance until accounting can be reconciled.

```sh
irrigator campaign init ./campaign --budget 10
irrigator campaign admit ./campaign ./plan-v4.json --run ./run-one
irrigator resume ./run-one
irrigator campaign status ./campaign
irrigator campaign close ./run-one
```

Admission accepts an unbound `swarm/v4` or `swarm/v5` plan and derives a new
immutable `swarm/v5` plan/checkpoint4. It pins the campaign identity, grant,
canonical run directory, complete derived plan hash and run ceiling. The input
plan is unchanged. The full run ceiling is reserved before the run is created.
Parallel admissions share one locked durable ledger; even an unused prepared run
consumes its allowance. No provider is invoked by init/admit/status/close.

The two stores are deliberately conservative: a crash after reservation and
before run creation can leave a stranded grant. There is no automatic refund or
retry that grants the same authority twice. Inspect it rather than deleting the
ledger. The campaign is local to cooperating processes and supported local
filesystems; it is not a distributed coordinator or protection from its owner
deliberately rewriting private data.

Every run mutation validates its bound authority. A copied checkpoint at another
path, missing ledger, changed plan or closed grant cannot resume or retry. Moving
the run therefore requires a future explicit relocation protocol; changing a
path by hand is not supported.

`close` requires an idle controller, idle cell leases, terminal tasks, resolved
process evidence, settled operation reservations, a drained mailbox and acknowledged
outbox receipts. It permanently closes the
grant and charges the known micro-dollar subtotal, releasing unused allowance.
Closing twice is idempotent. Closed runs remain readable/exportable, but cannot
be retried, resumed, revised or receive new mailbox messages. Use a fresh run for
new work. Unknown cost, unresolved effects or reported overrun retain the grant.
There is no manual price guessing or unknown-cost settlement command.

Limits: 1,024 retained grants per campaign; per-run ceilings still apply; a
campaign does not grant extra workers, tools, helpers or model capabilities.
The ledger is private mode0600 in a mode0700 directory. Go 1.26.7+ (the module
minimum) is required for the confined storage implementation.

Workflow compilation, campaign admission and backup restore produce new
artifacts with explicit provenance; restored evidence remains quarantined and
cannot become another spending grant.
