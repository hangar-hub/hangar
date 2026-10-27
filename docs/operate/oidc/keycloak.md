---
title: Single sign-on with Keycloak (OIDC)
description: "Set up sign-in to Hangar with Keycloak as the OpenID Connect provider, and try it locally with the Compose example's ready realm."
---

# OIDC with Keycloak

Try it locally first with the [Keycloak Compose example](../../get-started/compose.md#oidc-with-keycloak), which imports a ready realm.

1. In your realm, open **Clients**, **Create client**:
   - Client type **OpenID Connect**, client ID `hangar`.
   - **Client authentication** on; **Standard flow** on; other flows off.
   - **Valid redirect URIs**: `https://hangar.example.com/auth/oidc/keycloak/callback`.
2. Copy the client secret from the **Credentials** tab.
3. Add a groups claim: **Client scopes**, **Create client scope** named `groups`, then **Mappers**, **By configuration**, **Group Membership**, token claim name `groups`, **Full group path** off. Add the scope to the `hangar` client as a default scope.
4. Configure Hangar:

```yaml
oidc:
  providers:
    - id: keycloak
      name: Keycloak
      issuer: https://sso.example.com/realms/example
      client_id: hangar
      scopes: [openid, profile, email, groups]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [hangar-admins]
      auto_create_users: true
```

and set the client secret in a secret, such as `HANGAR_OIDC_PROVIDERS` as JSON.

Hangar and the browser must both reach the issuer URL exactly as configured, because the issuer in tokens has to match it.
