# Permissions

## Users, Organisations and Roles

Every User has a personal Namespace. Organisations are Namespaces that Users join as Members with a Role:

| Role | Can |
|---|---|
| `read` | See the Organisation's private Repos and download from them. |
| `write` | Also create Repos, upload, push and start Pulls into the Organisation. |
| `admin` | Also manage Members, settings, webhooks, and rename, transfer and delete Repos. |

A Repo can also have collaborators with `read` or `write`, for people outside its Namespace. Instance admins can do everything; the first one is created from configuration (`security.bootstrap_admin`).

## Tokens

A Token has one scope, `read`, `write` or `admin`, which caps what it can do on top of the user's own permissions. It can be limited to some Namespaces or Repos, and given an expiry. Clients send it as `Authorization: Bearer hgr_...` (HTTP) or as the git password. Hangar stores only a hash, and shows when each Token was last used.

## Private Repos stay hidden

Requests for a private Repo you cannot read get the same answer as a Repo that does not exist (404), as on huggingface.co, so Repo names do not leak.

## Gated Repos

The owner of a Repo can require people to ask for access, with a form. Admins of the Repo accept or reject requests, automatically or by hand. See [Gated models](../guides/gated.md).

## Ollama keys

Ollama signs its registry requests with its own key pair. Register `~/.ollama/id_ed25519.pub` under **Settings**, **Ollama keys** (or `hangar ollama key register`) so `ollama pull` works for Repos that are not public. See [Ollama](../guides/ollama.md).

## Audit log

Security-relevant actions, such as sign-ins, Token changes, Membership changes and visibility changes, are recorded in the audit log under **Admin**, **Audit log**, and exported as OpenTelemetry log records.
