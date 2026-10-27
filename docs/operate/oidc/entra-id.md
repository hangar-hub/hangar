---
title: Single sign-on with Microsoft Entra ID (OIDC)
description: "Set up sign-in to Hangar with Microsoft Entra ID as the OpenID Connect provider: register an app, create a client secret and configure Hangar."
---

# OIDC with Microsoft Entra ID

1. In the Entra admin centre, open **App registrations**, **New registration**:
   - Redirect URI, platform **Web**: `https://hangar.example.com/auth/oidc/entra/callback`.
2. Under **Certificates & secrets**, create a client secret.
3. Under **Token configuration**, **Add groups claim**, choose **Security groups** and, for the ID token, **Group ID** (or **sAMAccountName** for groups synced from Active Directory).
4. Configure Hangar:

```yaml
oidc:
  providers:
    - id: entra
      name: Microsoft
      issuer: https://login.microsoftonline.com/<tenant ID>/v2.0
      client_id: <application (client) ID>
      scopes: [openid, profile, email]
      claims: {username: preferred_username, email: email, name: name, groups: groups}
      admin_groups: [<object ID of the admins group>]
      org_mappings:
        - {group: <object ID of a group>, org: ml, role: write}
      auto_create_users: true
```

Entra sends group object IDs, not names, so use the IDs in `admin_groups` and `org_mappings`. `preferred_username` is the user principal name, such as `alice@example.com`. Hangar usernames cannot contain `@`, so if sign-in reports an invalid username, map `claims.username` to a claim without one, such as an optional `login_hint` or a custom claim with the user's alias.

People in more than 200 groups get no `groups` claim in the token; give Hangar's application an assignment to the groups that matter, or use app roles.
