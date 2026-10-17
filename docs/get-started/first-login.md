# Sign in for the first time

1. Open Hangar's public URL, such as <http://localhost:8080>.
2. Sign in as `hangar-admin` with the password Hangar printed on first start (`docker logs hangar`), or the one you set in `HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD`.
3. Change the password under **Settings**.

## Create a Token

Clients send a Token as a bearer token (Hub API and native API) or as the git password. Only git and git-lfs accept a Token as a password.

1. Open **Settings**, **Tokens**, and select **Create token**.
2. Choose a scope: `read` downloads; `write` also uploads and starts Pulls, and lets `huggingface_hub` delete, rename and change the visibility or gating of Repos you are an admin of; `admin` also does every other Repo administration, such as webhooks, collaborators and transfers, and manages the Members of Organisations you administer. You can limit a Token to some Namespaces or Repos, and give it an expiry.
3. Copy the secret, which starts with `hgr_`. It is shown once.

Or let the CLI create one with a device-code sign-in:

```sh
hangar login --url http://localhost:8080
```

## Add people

Self-registration is off by default (`security.registration: closed`). As an admin, open **Admin**, **Users** to create invite links, or connect an OIDC provider so people sign in with accounts they already have; see [OIDC](../operate/oidc/index.md).

Next: [Pull your first model](first-pull.md).
