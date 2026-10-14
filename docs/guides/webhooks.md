# Webhooks

Webhooks send an HTTP `POST` to your URL when something happens in a Namespace or Repo: pushes and Commits, Pulls, Mirror syncs, indexing and scans, discussions and pull requests. The event list and payloads are in [Webhook events](../reference/webhooks.md).

Create one under the Namespace's or Repo's **Settings**, **Webhooks**, with the URL, a secret and the events you want. Admins of the Namespace or Repo can manage them, see each delivery with its status code and duration, redeliver it, and send a test event.

## Verify the signature

Every delivery carries `X-Hangar-Signature: sha256=<hex>`, an HMAC-SHA256 of the raw body with your secret. Check it before you trust the payload:

```python
import hashlib, hmac

def verify(secret: bytes, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

`X-Hangar-Event` names the event and `X-Hangar-Delivery` identifies the delivery, so you can ignore repeats.

## Retries

A delivery that does not answer 2xx within 10 seconds is retried after 1 minute, 5 minutes, 30 minutes, 2 hours and 12 hours, then marked failed.

## Private addresses

Webhooks may target private addresses (for example a service on your LAN) while the Instance setting `webhooks.allow_private_targets` is on, which is the default for self-hosting. Turn it off on Instances shared by people who should not reach your internal network through Hangar.
