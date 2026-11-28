# Environment-aware admission

`--admission` makes the controller start less work while the machine, the network or the run itself is under pressure, and start it again when the pressure clears. It decides only which already-ready tasks may start right now. It never changes what a task is bound to, never touches the checkpoint, and never gates a commit, a cancellation or a status query. Without the flag nothing changes.

```
irrigator apply plan.json --run runs/r1 --admission balanced
irrigator resume runs/r1 --admission quiet      # replaces the saved choice
irrigator resume runs/r1                        # reuses RUN/admission.json
```

## Presets

| Preset | Behavior |
| --- | --- |
| `unbounded` | Default. No sensors; exactly the previous behavior. |
| `balanced` | Power and thermal, network, memory, disk, provider rate limits and a failure-rate breaker (see below), with an operator override. |
| `quiet` | Stricter limits on battery and under memory pressure, no remote work on a metered link, and it yields while you are using the machine. |
| `offpeak` | `balanced`, and outside 22:00-07:00 local time it admits one local and one remote task at a time. |

A config file works too: `--admission config.json` with the schema `irrigator.admission/v1`. Unknown fields are rejected, and the file is saved as `RUN/admission.json`. A slot limit of `-1` means no limit and `0` means none may start.

```json
{"schema":"irrigator.admission/v1","preset":"mine","exit_dwell_seconds":30,"ramp_throttle_seconds":10,
 "override":true,
 "power":{"battery_floor_percent":20,"on_battery_local_slots":2,"low_power_local_slots":1,
          "thermal_fair_local_slots":-1,"thermal_serious_local_slots":1,"thermal_critical_hold":true},
 "network":{"metered_remote_slots":1},
 "memory":{"warn_local_slots":2,"critical_hold":true,"swap_used_percent_limit":60,"swap_local_slots":1},
 "disk":{"low_free_mb":5120,"critical_free_mb":1024,"low_local_slots":1,"lock_wait_ms":0},
 "offpeak":{"window":"22:00-07:00","outside_local_slots":1,"outside_remote_slots":1}}
```

## Sensors

Each sensor reports a limit over the same fields, and the strictest wins: the minimum of the slot limits and of the model tier, the AND of "remote allowed", the longest poll interval and the strongest hold. The status names the sensor that bound each field.

| Sensor | Signal | Effect | If its probe fails |
| --- | --- | --- | --- |
| `power` | battery floor, battery vs AC, Low Power Mode, CPU speed limit (macOS `pmset`) | local slot limits; a hold below the battery floor or at critical thermal | open, shown degraded |
| `network` | no default route, or a phone-hotspot gateway (macOS `route`) | stops remote work offline; limits it on a metered link | open, shown degraded |
| `memory` | kernel memory pressure level and swap use (macOS `sysctl`), not CPU% | local slot limits; a hold at critical pressure | open, shown degraded |
| `disk` | free space on the run's filesystem | local limit when low; hold below the critical floor | **closed**: nothing new starts |
| `lock-wait` | how long the checkpoint lock takes to acquire (opt-in with `lock_wait_ms`) | limits both localities while contended | open, shown degraded |
| `presence` | seconds since input (macOS `ioreg` HIDIdleTime) | soft local limit while you are active | open, shown degraded |
| `offpeak` | time of day, from the run's clock | limits both localities outside the window | n/a |
| `offpeak-signal` | an external hook: `signal_file` in the override format | whatever the file says | open, shown degraded |
| `sizing` | the plan sizing gate's concurrency cap | shown so one status lists every constraint; sizing stays authoritative | n/a |
| `provider-limit` | a remote attempt that ended with rate-limit evidence | remote cooldown until the provider's stated reset, then additive recovery | n/a |
| `failure-rate` | exponentially weighted failure rate of recent attempts | sheds one slot per interval, then a hold | n/a |
| `override` | `RUN/admission-override.json` | whatever the file says | **closed** if present but unreadable |

Off macOS the host probes are not implemented and report a degraded probe, which fails open.

### Provider rate limits

When a remote attempt fails and its saved output shows rate-limit evidence (the same diagnosis as `irrigator provider-limit`), new remote admissions stop until the reset time the provider stated, or a default, never longer than `max_cooldown_seconds`. They then resume under an additive-increase multiplicative-decrease limit that cannot wind up behind another sensor's constraint. The sensor only lowers how many pending remote tasks may start. It never retries, resets or touches a failed cell.

### Failure-rate breaker

The breaker tracks the failure rate of recent attempts. Above the threshold it sheds one slot per interval, the interval shrinking as 1/sqrt(steps) as in CoDel, so a brief bad patch costs nothing and a sustained one is cut back quickly. When every slot is shed it holds for `open_seconds`, then lets one probe slot through, and closes when the rate is under half the threshold. The rate decays with `half_life_seconds` while no attempts finish, so old news cannot keep it tripped.

## How limits change

Limits tighten at once and relax slowly, so a one-second spike cannot make admission flap. After a limit relaxes it must stay relaxed for `exit_dwell_seconds`, then grows one slot at a time, no faster than one per `ramp_throttle_seconds`, so a newly admitted worker can claim its memory before the next one starts. A hold counts as zero slots, so lifting it ramps the same way.

Local work (mock and Ollama bindings) and remote work (the hosted `claude`, `codex` and `agy` CLIs, and the paid haiku backend) have separate limits, so a hot GPU does not starve remote tasks and an offline link does not starve local ones.

## A hold is not "blocked"

When work is ready but the environment is holding admission, the run stays alive and resumes by itself when the signal clears: it does not exit as `blocked`. A permanent hold (only an operator override can set one) does block the run.

## Model tiers

A tier is a ceiling on which declared models may start. It is strictly opt-in: declare a tier per managed profile in the config, and only tasks on those profiles are ever filtered. Nothing is inferred from model names, and nothing is rebound.

```json
{"profile_tiers":{"heavy":"full","fast":"minimal"},
 "power":{"thermal_serious_max_tier":"reduced","on_battery_max_tier":"reduced"}}
```

When a sensor lowers the ceiling to `reduced`, a task on a `full` profile waits and tasks on `reduced` or `minimal` profiles, and tasks on undeclared profiles, may start. The override file can set `"max_tier"` too.

## Operator override

With `override` enabled, write `RUN/admission-override.json` to throttle or hold a run by hand, and delete it to release. Absent fields mean no opinion.

```json
{"hold":"transient","reason":"recording a demo"}
{"max_local_slots":1,"allow_remote":false,"max_tier":"reduced"}
```

A file that exists but cannot be read or parsed holds admission, so a typo is visible and not silently ignored.

## Status and the dashboard

While a controller is running it writes `RUN/admission-status.json` on each change (not more often than every two seconds, except that a hold starting or ending is written at once). The terminal dashboard, `status` and `status --watch` show one line under the header while admission is limited or a probe is degraded, for example:

```
admission: local 2/6 (power: thermal serious); remote paused (network: offline) | degraded probes: memory
```

The line is shown only while the run's `controller.lock` is held. A finished or crashed run leaves the file behind and shows nothing, so a dead run never looks throttled.
