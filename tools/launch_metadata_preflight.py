#!/usr/bin/env python3
"""Workflow preflight only: validate public observer metadata without changing a plan."""
import argparse, hashlib, json, pathlib, subprocess, sys
p = argparse.ArgumentParser()
p.add_argument('plan', type=pathlib.Path)
p.add_argument('--binary', required=True)
p.add_argument('--allow-unknown-capacity', metavar='REASON')
a = p.parse_args()
raw = a.plan.read_bytes()
plan = json.loads(raw)
errors = []
def text(obj, key, limit, where):
    value = obj.get(key)
    if not isinstance(value, str) or not value.strip():
        errors.append(f'{where}.{key} must contain intentional public text')
    elif len(value) > limit:
        errors.append(f'{where}.{key} exceeds {limit} Unicode characters')
d = plan.get('display') or {}
if d.get('version') != 1 or d.get('publish') is not True:
    errors.append('display.version=1 and display.publish=true are required for this observer-enabled workflow')
text(d, 'title', 60, 'display')
text(d, 'purpose', 140, 'display')
for t in plan.get('tasks', []):
    td = t.get('display') or {}
    label = 'task ' + str(t.get('id'))
    for k, n in [('title', 48), ('why_spawned', 100), ('done_when', 140)]:
        text(td, k, n, label)
    field = td.get('outcome_field')
    if not isinstance(field, str) or not field or field not in t.get('requires', []):
        errors.append(f'{label}.outcome_field must name an explicit required public string result')
shape = plan.get('observer_shape')
turn_shape = plan.get('turn_shape')
# Turn plans prohibit legacy observer_shape. Their explicit execution shape is
# also their truthful capacity declaration; the runtime validates its semantics.
turn_explicit = (
    plan.get('api_version') in ('swarm/v2', 'swarm/v3', 'swarm/v4', 'swarm/v5', 'swarm/v6')
    and isinstance(turn_shape, dict)
    and all(type(turn_shape.get(k)) is int for k in
            ('root_agents', 'max_turns_per_agent', 'helpers_per_turn'))
    and turn_shape['root_agents'] > 0
    and turn_shape['max_turns_per_agent'] > 0
    and turn_shape['helpers_per_turn'] >= 0
)
explicit_capacity = shape is not None or turn_explicit
if not explicit_capacity and not (a.allow_unknown_capacity or '').strip():
    errors.append('explicit observer_shape or turn_shape is absent; provide a validated shape or an explicit --allow-unknown-capacity reason')
check = subprocess.run([a.binary, 'plan', str(a.plan.resolve())], capture_output=True, text=True)
if check.returncode:
    errors.append('runtime plan validation failed: ' + (check.stderr or check.stdout).strip())
result = {'plan_sha256': hashlib.sha256(raw).hexdigest(), 'ok': not errors,
          'errors': errors, 'capacity': 'explicit' if explicit_capacity else 'unknown',
          'capacity_source': 'turn_shape' if turn_explicit else ('observer_shape' if shape is not None else None),
          'unknown_capacity_reason': a.allow_unknown_capacity if not explicit_capacity else None,
          'scope': 'workflow metadata checks plus runtime plan validation; no launch, mutation or human-quality claim'}
print(json.dumps(result, indent=2))
sys.exit(0 if not errors else 1)
