---
description: "Sign in to Hangar for the first time as hangar-admin, create a Token for your clients, and add people to your Instance."
---

# Sign in for the first time

1. Open Hangar's public URL, such as <http://localhost:8080>.
2. Sign in as `hangar-admin` with the password Hangar generated on first start, or the one you set in `HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD`. Hangar saves a generated password as `bootstrap_admin_password` in `secrets.yaml` in `server.data_dir`; with Docker, read it with `docker cp hangar:/var/lib/hangar/secrets.yaml - | tar -xO | grep bootstrap_admin_password`.
3. Choose your own password: open **Settings**, **Profile**, enter the generated password as the current password, then your new password twice, and select **Change password**. The new password needs at least 10 characters and cannot be a common password. Your other sessions are signed out; this one stays signed in. On the same page you can set your display name and email.

## Create a Token

Clients send a Token as a bearer token (Hub API and native API) or as the git password. Only git and git-lfs accept a Token as a password.

1. Open **Settings**, **Tokens**, and select **Create Token**.
2. Choose a scope: `read` to download, `write` to also upload and start Pulls, or `admin` for every other kind of administration. [Permissions](../concepts/permissions.md#tokens) lists what each scope allows. You can limit a Token to some Namespaces or Repos, and give it an expiry.
3. Copy the secret, which starts with `hgr_`. It is shown once.

Or let the CLI create one with a device-code sign-in:

```sh
hangar login --url http://localhost:8080
```

## Add people

Self-registration is off by default (`security.registration: closed`). As an admin, open **Admin**, **Users** to create invite links, or connect an OIDC provider so people sign in with accounts they already have; see [OIDC](../operate/oidc/index.md).

Next: [Pull your first model](first-pull.md).
