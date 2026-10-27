---
title: OIDC single sign-on for a self-hosted model hub
description: "Sign in to Hangar with any OpenID Connect provider, with PKCE, groups mapped to admins and Organisations, alongside local accounts."
---

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

People who already have a local account can link a provider under **Settings**, **Profile**, **Sign-in providers**.

## Names, emails and passwords

Hangar copies the display name and email from the provider's claims only when it creates a User, on the first sign-in, and treats the email as confirmed only when the claims mark it verified (`email_verified`). After that they belong to the User, who can change them under **Settings**, **Profile**; later sign-ins do not overwrite them. A new email a user types waits for the confirmation link an admin sends to it, like anyone's.

A first sign-in whose verified email belongs to an existing User is refused until that User signs in and links the provider. Only a verified email counts: one a user typed and never confirmed blocks nobody. Admin groups and Organisation mappings are the only things Hangar takes from the provider at every sign-in.

A User that OIDC created has no Hangar password, so **Settings**, **Profile** tells them to change their password with the provider, and `POST /api/hangar/v1/me/password` answers 403. A local User who links a provider keeps their Hangar password and can still change it.

## Providers

[Keycloak](keycloak.md), [Authentik](authentik.md), [Microsoft Entra ID](entra-id.md), [Google](google.md), [Okta](okta.md), [GitLab](gitlab.md) and [UniFi Identity](unifi.md). Any other OpenID Connect provider works the same way.
