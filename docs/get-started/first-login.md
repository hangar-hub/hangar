# Sign in for the first time

1. Open Hangar's public URL, such as <http://localhost:8080>.
2. Sign in as `hangar-admin` with the password Hangar printed on first start (`docker logs hangar`), or the one you set in `HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD`.
3. Choose your own password: open **Admin**, **Users**, select **Reset link** next to `hangar-admin`, open the link and save a new password. Hangar has no password change under **Settings**; every local password changes through a reset link that an admin creates.

## Create a Token

Clients send a Token as a bearer token (Hub API and native API) or as the git password.

1. Open **Settings**, **Tokens**, and select **Create Token**.
2. Choose a scope: `read` downloads, `write` also uploads, starts Pulls and administers Repos you are an admin of (settings, rename, delete), `admin` also manages the Members of Organisations you administer. You can limit a Token to some Namespaces or Repos, and give it an expiry.
3. Copy the secret, which starts with `hgr_`. It is shown once.

Or let the CLI create one with a device-code sign-in:

```sh
hangar login --url http://localhost:8080
```

## Add people

Self-registration is off by default (`security.registration: closed`). As an admin, open **Admin**, **Users** to create invite links, or connect an OIDC provider so people sign in with accounts they already have; see [OIDC](../operate/oidc/index.md).

Next: [Pull your first model](first-pull.md).
