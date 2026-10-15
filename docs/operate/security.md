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
- Pickle-based weights can run code when loaded. Hangar scans them and shows a warning on suspicious files; prefer safetensors. To refuse downloads of risky files, turn on **Block downloads of unsafe files** under **Admin**, **Settings**. See [Blocking unsafe downloads](#blocking-unsafe-downloads).

## Blocking unsafe downloads

With **Block downloads of unsafe files** on (`scan_block_unsafe_downloads` in the native API), Hangar refuses to serve:

- Files the security scan marked unsafe.
- Pickle, joblib and NumPy files that have no scan result yet, because the scan is still running or its Job failed. They download once the scan finds them safe. While the scan of a new Commit runs, a file whose content is unchanged since an earlier scanned Commit keeps that Commit's result, in the web UI and for downloads by path.

The setting applies to every way a file leaves Hangar: `huggingface_hub` and other Hub API clients, the web UI, git-lfs, the Ollama registry and zip exports. Hub API and web UI downloads can go ahead with `accept_risk=1` in the URL; git-lfs and Ollama answer with a 403 and the reason, and a zip export of a folder with such a file is refused. A `git clone` without LFS is not checked, because git transfers whole Commits; the Hub's default `.gitattributes`, which Hangar applies to new Repos, stores every pickle format in LFS.

Links that Hangar signs, for git-lfs, Pull-through and zip export downloads, are checked again when they are used, so a link issued before you turned the setting on, or before the scan finished, stops working. In `redirect` storage mode, presigned links go straight to the Blob store, so Hangar checks them only when it issues them, and they work until they expire.

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
