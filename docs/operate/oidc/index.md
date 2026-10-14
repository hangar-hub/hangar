# Sign in with OIDC

Hangar signs people in through any OpenID Connect provider, with PKCE, alongside or instead of local accounts. You can configure more than one provider.

## What every provider needs

1. In the provider, create a confidential web application (client) with:
   - the redirect URI `<public_url>/auth/oidc/<id>/callback`, where `<id>` is the provider ID you choose in Hangar, such as `https://hangar.example.com/auth/oidc/keycloak/callback`;
   - the `openid`, `profile` and `email` scopes, and a groups claim if you want to map groups.
2. Add the provider to Hangar's configuration:

```yaml
oidc:
  providers:
    - id: keycloak                    # in the callback URL; keep it stable
      name: Keycloak                  # on the sign-in button
      issuer: https://sso.example.com/realms/example
      client_id: hangar
      client_secret: ""               # better: set it in a secret, see below
      scopes: [openid, profile, email, groups]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [hangar-admins]   # members become Instance admins
      org_mappings:
        - {group: ml-team, org: ml, role: write}
      auto_create_users: true         # create a User on first sign-in
```

As an environment variable, `oidc.providers` is a JSON array:

```sh
HANGAR_OIDC_PROVIDERS='[{"id":"keycloak","name":"Keycloak","issuer":"https://sso.example.com/realms/example","client_id":"hangar","client_secret":"...","scopes":["openid","profile","email","groups"],"auto_create_users":true}]'
```

Keep the variable in a secret, because it holds the client secret.

## Groups, admins and Organisations

- `admin_groups`: members of these groups become Instance admins on every sign-in, and stop being admins when they leave the group.
- `org_mappings`: members of a group become Members of an Organisation with a Role. Hangar creates the Organisation if it does not exist.
- `claims.groups`: the claim that lists groups. Some providers need a mapper or extra scope to include it; the provider pages say how.

## Local accounts alongside OIDC

With OIDC on, you can turn off password sign-in (`security.local_login: false`). The bootstrap admin can still sign in with a password, as a break-glass account if the provider is down.

People who already have a local account can link a provider under **Settings**, **Sign-in methods**.

## Providers

[Keycloak](keycloak.md), [Authentik](authentik.md), [Microsoft Entra ID](entra-id.md), [Google](google.md), [Okta](okta.md), [GitLab](gitlab.md) and [UniFi Identity](unifi.md). Any other OpenID Connect provider works the same way.
