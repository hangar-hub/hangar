---
description: "Get an HTTP POST from Hangar when Repos change, Pulls finish or scans complete, and verify each delivery's signature."
---

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

By default, webhooks cannot target private addresses: loopback, RFC 1918, carrier-grade NAT and unique local addresses. A delivery to one fails with "webhook targets a private address, which this Instance does not allow". To deliver to a service on your LAN, an Instance admin turns on **Allow webhooks to private addresses** under **Admin**, **Settings**, or sets `webhooks.allow_private_targets: true` in the [configuration](../reference/configuration.md#webhooks). Only do this when everyone who can add a webhook may reach your internal network through Hangar: every user administers their own Namespace, so every user can add one.

Earlier releases allowed private targets by default. If your webhooks deliver to private addresses, an admin must now turn the setting on.

Whatever the setting, Hangar never delivers to link-local addresses (which include the cloud metadata service at 169.254.169.254), other cloud metadata addresses, `0.0.0.0` or multicast addresses. Hangar checks the address it connects to after resolving the host name, and does not follow redirects.
