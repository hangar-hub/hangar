# OIDC with Okta

1. In the Okta admin console, open **Applications**, **Create App Integration**, **OIDC - OpenID Connect**, **Web Application**:
   - Sign-in redirect URI: `https://hangar.example.com/auth/oidc/okta/callback`.
   - Assign the people or groups who may use Hangar.
2. To send groups, open the authorisation server's **Claims** (or the app's **Sign On**, **OpenID Connect ID Token**) and add a `groups` claim with a filter such as **Matches regex** `.*`.
3. Configure Hangar:

```yaml
oidc:
  providers:
    - id: okta
      name: Okta
      issuer: https://example.okta.com/oauth2/default
      client_id: <client ID>
      scopes: [openid, profile, email, groups]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [hangar-admins]
      auto_create_users: true
```
