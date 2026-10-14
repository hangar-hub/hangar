# Security hardening

## Checklist

- Serve Hangar over HTTPS only, with `server.public_url` set to the `https://` address, and set `server.trusted_proxies` to your proxy's addresses.
- Keep `security.registration: closed` (the default) or `invite`, and leave `security.anonymous_read` off unless you want a public Instance.
- Use OIDC with groups for Instance admins (`admin_groups`), and turn off local sign-in (`security.local_login: false`), keeping the bootstrap admin as a break-glass account with a long password stored offline.
- Give Tokens the smallest scope, limit them to the Namespaces or Repos they need, and set an expiry.
- Store the Instance key, database DSN and S3 keys in secrets, not configuration files. Rotate the Instance key with `hangar admin rotate-key` if it leaks.
- On Instances shared with people you do not fully trust, turn off `webhooks.allow_private_targets` so webhooks cannot reach your internal network.
- Watch the audit log (**Admin**, **Audit log**) and export it with your logs.
- Run the image as it ships: non-root (UID 65532), read-only root filesystem, no shell. The Helm chart also drops every capability and uses the `RuntimeDefault` seccomp profile; turn on its NetworkPolicy.
- Pickle-based weights can run code when loaded. Hangar scans them and shows a warning on suspicious files; prefer safetensors.

## Verify the image

Images are signed with cosign keyless signing from the release workflow, with an SPDX SBOM attestation and SLSA build provenance:

```sh
cosign verify ghcr.io/jamesagarside/hangar:1.0.0 \
  --certificate-identity-regexp 'https://github.com/jamesagarside/hangar/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation --type spdxjson ghcr.io/jamesagarside/hangar:1.0.0 \
  --certificate-identity-regexp 'https://github.com/jamesagarside/hangar/.github/workflows/release.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/jamesagarside/hangar:1.0.0 --owner jamesagarside
```

Release archives come with `checksums.txt`, signed the same way (`checksums.txt.sig` and `.pem`), and an SBOM per archive.

## Report a vulnerability

See the [security policy](../project/security.md).
