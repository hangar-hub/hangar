# Pull windows and bandwidth

Pulls are queued, resumable and rate-limited, so a large Pull never saturates a home connection. Set these in the configuration (or `HANGAR_PULL_*` variables):

```yaml
pull:
  bandwidth_limit: 20MiB     # bytes per second across the whole Instance; 0 means no limit
  concurrency: 4             # files transferring at once across the Instance
  windows: ["01:00-07:00"]   # only start file transfers in these windows
server:
  timezone: Europe/London    # the time zone windows are in
```

- The bandwidth limit holds across every worker: each worker leases a share of the budget from the database every few seconds.
- Outside a window, Pulls are planned straight away (so the history is browsable) but file transfers wait. Transfers already running finish their current part, then wait. The UI shows "Waiting for pull window (01:00-07:00)".
- Windows can span midnight, such as `22:00-06:00`.
- A user with `write` can start a Pull outside the windows with `hangar pull --now`, which still keeps to the bandwidth limit, while the Instance setting **Let writers pull now** (`pull.allow_override_windows`, under **Admin**, **Pull settings**) is on, as it is by default.
- Admins can also change the bandwidth limit, concurrency and windows under **Admin**, **Pull settings**.

Time spent waiting for the bandwidth limit is exported as the `hangar.pull.rate_limited.duration` metric.
