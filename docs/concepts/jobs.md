# Jobs

Background work runs as Jobs in the database-backed queue: Pulls (`pull`, `pull.file`), Mirror syncs, indexing and scanning, garbage collection, git compaction, dataset conversion and statistics, webhook deliveries, zip exports and purges.

- Workers claim Jobs with a lease, renewed while they run. If a worker dies, its Jobs go back to the queue when the lease expires, and they resume where they stopped.
- The same work is never queued twice: pulling the same files twice joins the existing Job.
- Pulls report bytes done, the total, the rate and an ETA, per file and overall.

See your Jobs under **Jobs** in the UI, or with the CLI:

```sh
hangar jobs list
hangar jobs watch <job-id>
hangar jobs cancel <job-id>
```

Admins see every Job. Processes in role `all` or `worker` run Jobs; `web` processes only queue them. See [Scaling](../operate/scaling.md).
