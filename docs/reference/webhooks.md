# Webhook events

Every delivery is an HTTP `POST` with a JSON body:

```json
{
  "event": "repo.updated",
  "id": "0192f5c6-5d1a-7c3e-9b61-2f0d4f8a1c22",
  "occurred_at": "2026-10-13T12:34:56Z",
  "actor": {"username": "alice"},
  "repo": {"id": "acme/my-model", "type": "model"},
  "data": {}
}
```

| Header | Value |
|---|---|
| `X-Hangar-Event` | The event name. |
| `X-Hangar-Delivery` | A unique ID for this delivery. |
| `X-Hangar-Signature` | `sha256=<hex>`: the HMAC-SHA256 of the body with the webhook's secret. See [Webhooks](../guides/webhooks.md#verify-the-signature). |

| Event | When |
|---|---|
| `repo.created` | A Repo is created. |
| `repo.updated` | A push or Commit changes refs; `data` lists the refs changed. |
| `repo.deleted` | A Repo is deleted. |
| `repo.restored` | A deleted Repo is restored. |
| `repo.renamed` | A Repo is renamed or moved to another Namespace. |
| `repo.visibility_changed` | A Repo's visibility changes. |
| `pull.started` | A Pull starts. |
| `pull.completed` | A Pull finishes, including `partial` Pulls. |
| `pull.failed` | A Pull fails. |
| `mirror.synced` | A tracking Mirror finds and pulls new Commits. |
| `index.completed` | A Commit's index is ready. |
| `scan.completed` | A Commit's security scan is done; `data` has the status. |
| `discussion.created` | A discussion or pull request is opened. |
| `discussion.commented` | Someone comments. |
| `discussion.status_changed` | A discussion or pull request is closed or reopened. |
| `pr.merged` | A pull request is merged. |

Deliveries time out after 10 seconds and are retried after 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours.
