# OIDC with GitLab

GitLab.com and self-managed GitLab are OpenID Connect providers.

1. In GitLab, open **Admin**, **Applications** (instance-wide) or a group's **Settings**, **Applications**, and add an application:
   - Redirect URI: `https://hangar.example.com/auth/oidc/gitlab/callback`.
   - Confidential: on. Scopes: `openid`, `profile`, `email`.
2. Configure Hangar:

```yaml
oidc:
  providers:
    - id: gitlab
      name: GitLab
      issuer: https://gitlab.example.com
      client_id: <application ID>
      scopes: [openid, profile, email]
      claims: {username: nickname, email: email, name: name, groups: groups_direct}
      admin_groups: [platform/hangar-admins]
      auto_create_users: true
```

GitLab lists a user's groups by full path in the `groups_direct` claim.
