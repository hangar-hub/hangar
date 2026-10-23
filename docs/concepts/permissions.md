---
description: "How Hangar controls access: Users, Organisations and Roles, Tokens, private and gated Repos, Ollama keys and the audit log."
---

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

A Token has one scope, which caps what it can do on top of the user's own permissions:

| Scope | Can |
|---|---|
| `read` | Download files and read metadata and history. |
| `write` | Also upload, push and start Pulls. On Repos the user administers, also what a write Token does on huggingface.co through `huggingface_hub`: `delete_repo`, `update_repo_settings` (visibility and gating) and `move_repo` within the same Namespace. |
| `admin` | Also everything else that administers a Repo, such as transfers to another Namespace, webhooks, collaborators, access requests and every setting in the native API and the UI; managing Organisation Members; and, for Instance admins, the Instance. |

A Token can be limited to some Namespaces or Repos, and given an expiry. Clients send it as `Authorization: Bearer hgr_...` over HTTP, or as the password of HTTP Basic credentials, which only git and git-lfs routes accept. Hangar stores only a hash of each Token, and shows when it was last used.

## Private Repos stay hidden

Requests for a private Repo you cannot read get the same answer as a Repo that does not exist (404), as on huggingface.co, so Repo names do not leak.

## Large files you add by their hash

Hangar stores each LFS object once, however many Repos hold it. A Commit can still add a large file by its SHA-256 alone only if you uploaded that file before or can read a Repo that holds it. Otherwise Hangar asks for the bytes, as if it had never seen the file: `huggingface_hub`, `hf` and git-lfs send them without you doing anything, and Hangar checks them without storing a second copy. So knowing a file's hash is not enough to get a copy of a file in a private Repo.

## Gated Repos

An admin of a Repo can require people to ask for access, with a form. Admins of the Repo accept or reject requests, automatically or by hand. See [Gated models](../guides/gated.md).

## Ollama keys

Ollama signs its registry requests with its own key pair. Register `~/.ollama/id_ed25519.pub` under **Settings**, **Ollama keys** (or `hangar ollama key register`) so `ollama pull` works for Repos that are not public. See [Ollama](../guides/ollama.md).

## Your account

Under **Settings**, **Profile** you can change your display name and email, and your password. A new email takes effect when you open its confirmation link: Hangar sends no mail, so ask an admin to create the link (**Admin**, **Users**, **Email link**) and send it to the new address. Until then your old email stays. To change the password you enter the current one; the new one needs at least 10 characters and cannot be a common password. Changing it signs out your other sessions, keeps this one, and leaves your Tokens working. A Token cannot change a password, but a `write` Token can change your display name and email (`PATCH /api/hangar/v1/me`). If you sign in with an OIDC provider and have no Hangar password, change your password with the provider. If you forget your password, ask an admin for a reset link.

## Audit log

Hangar records security-relevant actions, such as sign-ins, password and profile changes, Token changes, Membership changes and visibility changes, in the audit log under **Admin**, **Audit log**, and exports them as OpenTelemetry log records.
