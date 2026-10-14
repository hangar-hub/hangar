# OIDC with Authentik

1. In Authentik, open **Applications**, **Providers**, **Create**, **OAuth2/OpenID Provider**:
   - Client type **Confidential**.
   - Redirect URI (strict): `https://hangar.example.com/auth/oidc/authentik/callback`.
   - Scopes: `openid`, `email`, `profile`. Authentik includes a `groups` claim in the `profile` scope.
2. Create an **Application** named Hangar with the slug `hangar` that uses the provider.
3. Configure Hangar with the client ID and secret:

```yaml
oidc:
  providers:
    - id: authentik
      name: Authentik
      issuer: https://auth.example.com/application/o/hangar/
      client_id: <client ID>
      scopes: [openid, profile, email]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [hangar-admins]
      auto_create_users: true
```

The issuer is the application's **OpenID Configuration Issuer**, with the trailing slash.
