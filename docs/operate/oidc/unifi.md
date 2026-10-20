---
description: "Set up sign-in to Hangar with UniFi Identity Enterprise as the OpenID Connect provider."
---

# OIDC with UniFi Identity

UniFi Identity Enterprise can act as an OpenID Connect provider for internal applications.

1. In UniFi Identity Enterprise, add an application with the **OIDC** sign-on method:
   - Redirect URI: `https://hangar.example.com/auth/oidc/unifi/callback`.
   - Copy the client ID, client secret and issuer URL that it shows.
2. Assign the users or groups who may use Hangar.
3. Configure Hangar:

```yaml
oidc:
  providers:
    - id: unifi
      name: UniFi
      issuer: <issuer URL from the application>
      client_id: <client ID>
      scopes: [openid, profile, email, groups]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [hangar-admins]
      auto_create_users: true
```
