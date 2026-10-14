# OIDC with Google

1. In the Google Cloud console, open **APIs & Services**, **Credentials**, **Create credentials**, **OAuth client ID**:
   - Application type **Web application**.
   - Authorised redirect URI: `https://hangar.example.com/auth/oidc/google/callback`.
2. Configure Hangar:

```yaml
oidc:
  providers:
    - id: google
      name: Google
      issuer: https://accounts.google.com
      client_id: <client ID>.apps.googleusercontent.com
      scopes: [openid, profile, email]
      claims: {username: email, email: email, name: name}
      auto_create_users: true
```

Google sends no groups, so grant admin rights and Organisation Membership in Hangar. To limit sign-in to your Google Workspace domain, set the consent screen's user type to **Internal**.
