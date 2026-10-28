---
title: Scan models for malware and harden Hangar
description: "Run Hangar securely: scan pickle models for malware, block unsafe downloads, verify the container image and report vulnerabilities."
---

# Security hardening

This page lists what to do to run Hangar securely. For what Hangar protects, its trust boundaries and the threats it mitigates or leaves to you, see the [threat model](../security/threat-model.md).

## Checklist

- Serve Hangar over HTTPS only, with `server.public_url` set to the `https://` address, and set `server.trusted_proxies` to your proxy's addresses. Without it, every client shares the proxy's address, so one client's failed sign-ins throttle everyone; Hangar logs a warning when it sees `X-Forwarded-For` from a peer outside the list.
- Keep `security.registration: closed` (the default) or `invite`, and leave `security.anonymous_read` off unless you want a public Instance. While it is off, signed-out visitors cannot see public Repos, user and Organisation profiles, member lists or public Collections either.
- Use OIDC with groups for Instance admins (`admin_groups`), and turn off local sign-in (`security.local_login: false`), keeping the bootstrap admin as a break-glass account with a long password stored offline. Failed sign-ins from elsewhere do not lock an account out from addresses it signed in from in the last 90 days; if the bootstrap admin is locked out anyway, clear it with `hangar admin unlock-sign-in <username>` on the server host.
- Keep `server.public_url` on HTTPS so the session and CSRF cookies use the `__Host-` prefix, and keep `security.session_idle_timeout` (7 days by default) or shorten it on shared machines.
- Give Tokens the smallest scope, limit them to the Namespaces or Repos they need, and set an expiry.
- Tell users to change the password an admin gave them under **Settings**, **Profile**. A password change signs out the user's other sessions but keeps their Tokens, so someone who thinks their account was used by someone else should also revoke their Tokens under **Settings**, **Tokens**. Wrong current passwords count towards the same throttle as failed sign-ins. Users who forget their password still need a reset link from an admin. When a user changes their email, it waits for confirmation: select **Email link** next to them under **Admin**, **Users** and send the link to the new address, not to the user some other way, because opening it is what proves the address is theirs.
- Store the Instance key, database DSN and S3 keys in secrets, not configuration files. Rotate the Instance key with `hangar admin rotate-key` if it leaks.
- Leave `webhooks.allow_private_targets` off (the default) on Instances shared with people you do not fully trust, so webhooks cannot reach your internal network. Webhooks and Upstreams never reach link-local or cloud metadata addresses, and a public Upstream cannot redirect Hangar to a private address; see [Mirrors and Upstreams](../concepts/mirrors.md#upstreams).
- Watch the audit log (**Admin**, **Audit log**) and export it with your logs.
- Run the image as it ships: non-root (UID 65532), read-only root filesystem, no shell. The Helm chart also drops every capability and uses the `RuntimeDefault` seccomp profile; turn on its NetworkPolicy.
- Pickle-based weights can run code when loaded, so a malicious model can carry malware. Hangar scans them and shows a warning on suspicious and unsafe files; prefer safetensors. To refuse downloads of risky files, turn on **Block downloads of unsafe files** under **Admin**, **Settings**, and choose whether to block suspicious files too. See [Blocking unsafe downloads](#blocking-unsafe-downloads).

## Blocking unsafe downloads

With **Block downloads of unsafe files** on (`scan_block_unsafe_downloads` in the native API), Hangar refuses to serve:

- Files the security scan marked unsafe.
- Files the security scan marked suspicious, when **What to block** is **Unsafe and suspicious files** (`scan_block_level: suspicious` in the native API). The default, **Unsafe files** (`unsafe`), lets them download. See [Choose what to block](#choose-what-to-block).
- Files that can run code when loaded and that the scan could not read in full, such as a pickle compressed with xz, an old PyTorch tar archive or a joblib file whose array data the scan cannot measure. The scan result shows why.
- Files that can run code when loaded and have no scan result yet, because the scan is still running or its Job failed. They download once the scan finds them safe. While the scan of a new Commit runs, a file whose content is unchanged since an earlier scanned Commit keeps that Commit's result, in the web UI and for downloads by path.

Files that can run code when loaded are pickle, PyTorch, joblib and NumPy files, by name: `.bin`, `.pt`, `.pth`, `.ckpt`, `.pkl`, `.pickle`, `.joblib`, `.npy` and `.npz`, and those names with a compression or archive suffix (`.gz`, `.bz2`, `.xz`, `.lzma`, `.z`, `.zst`, `.lz4`, `.zip` and `.tar`), such as `model.joblib.gz` or `checkpoint.pth.tar`. The scan reads each of them by content, the way `torch.load`, `joblib.load`, `numpy.load` and `pandas.read_pickle` do: it decompresses compressed files, reads zip archives and `.npz` members, and follows the NumPy array data that joblib stores inside its pickles. A file that is not a pickle at all, such as OpenVINO weights in a `.bin` file, is safe. A file that is a pickle but ends part-way gets an error result and still downloads, because every loader stops at the same point.

Hangar scans the Commit at the tip of every branch, tag and pull request. A Commit it has not scanned, such as an earlier Commit of a push, takes the result of the same file in a Commit it has. git-lfs, Pull-through links and the Ollama registry ask for objects rather than paths, so Hangar checks every file in the Repo's history that holds the object: an object is refused when a file that holds it is refused, when a file that can run code holds it and no Commit with that file is scanned, and when no Commit of the Repo holds it. In a Partial mirror, a pickle that is not in the Arsenal has no scan result, so it is refused until a Pull brings it in and the scan reads it.

The setting applies to every way a file leaves Hangar: `huggingface_hub` and other Hub API clients, the web UI, git-lfs, `git clone` and `git fetch`, Pull-through links, the Ollama registry and zip exports. Hub API and web UI downloads can go ahead with `accept_risk=1` in the URL; git-lfs and Ollama answer with a 403 and the reason, and a zip export of a folder with such a file is refused. A git clone or fetch sends every Commit, so it is refused while any Commit holds a file stored in git rather than LFS that would be refused; files in LFS download through git-lfs, which checks each one. The Hub's default `.gitattributes`, which Hangar applies to new Repos, stores every pickle format in LFS, so this only affects Repos that changed it. Hangar also refuses clones of a Repo whose history is too long to check (more than 20,000 Commits); use `huggingface_hub` for those.

Links that Hangar signs, for git-lfs, Pull-through and zip export downloads, are checked again when they are used, so a link issued before you turned the setting on, or before the scan finished, stops working. In `redirect` storage mode, presigned links go straight to the Blob store, so Hangar checks them only when it issues them, and they work until they expire.

### Choose what to block

The scan classifies each import a pickle makes against lists of known safe and known dangerous functions. Imports on the deny list make a file **Unsafe imports**; imports on neither list make it **Suspicious imports**. The deny list cannot be complete, so a function that runs code and is on neither list is reported as suspicious, not unsafe.

| What to block | `scan_block_level` | Refuses | Use it when |
|---|---|---|---|
| **Unsafe files** (default) | `unsafe` | Files with imports on the deny list | Users download scikit-learn, joblib or full-model pickles, whose model classes from `sklearn` or `transformers` are on neither list. |
| **Unsafe and suspicious files** | `suspicious` | Also files with imports on neither list, as `torch.load(weights_only=True)` does | Users should load only weights. Most scikit-learn models and pickles of whole models are refused. |

Either way, a user who has reviewed a file can download it through the Hub API or the web UI with `accept_risk=1`.

### What the scan cannot catch

- With **What to block** set to **Unsafe files**, a function that runs code and is on neither list downloads, reported as suspicious. Review files with suspicious imports before you load them, or load them with `torch.load(weights_only=True)`.
- Files are checked by name. A pickle with another name, such as one a Repo's `trust_remote_code` Python loads, is not scanned; `trust_remote_code` already runs the Repo's Python code, so trust the Repo before you use it.
- Repos with more than 20,000 Commits, or more than 50,000 LFS files in their history, are checked for objects only in the history Hangar can walk.

## Verify the image

Images are signed with cosign keyless signing from the release workflow, with an SPDX SBOM attestation and SLSA build provenance. The signing identity is the release workflow at the release tag, `refs/tags/v<version>`, because a tag push is the only thing that starts a release:

Replace `1.2.3` with the version you run:

```sh
cosign verify ghcr.io/hangar-hub/hangar:1.2.3 \
  --certificate-identity-regexp '^https://github.com/hangar-hub/hangar/.github/workflows/release.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation --type spdxjson ghcr.io/hangar-hub/hangar:1.2.3 \
  --certificate-identity-regexp '^https://github.com/hangar-hub/hangar/.github/workflows/release.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/hangar-hub/hangar:1.2.3 --owner hangar-hub
```

The Helm chart is signed by the same workflow. Verify it the same way:

```sh
cosign verify ghcr.io/hangar-hub/charts/hangar:1.2.3 \
  --certificate-identity-regexp '^https://github.com/hangar-hub/hangar/.github/workflows/release.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Release archives come with `checksums.txt`, signed the same way into a Sigstore bundle (`checksums.txt.sigstore.json`), and an SPDX SBOM per archive (`<archive>.spdx.json`). Check the signature, then the archive you downloaded against `checksums.txt`:

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/hangar-hub/hangar/.github/workflows/release.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

## Report a vulnerability

See the [security policy](../project/security.md).
