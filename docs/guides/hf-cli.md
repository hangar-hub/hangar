---
title: Use the hf CLI with a self-hosted Hugging Face Hub
description: "Use the Hugging Face hf CLI with Hangar: set HF_ENDPOINT, sign in with a Token or the browser, and download, create and upload Repos, branches and datasets."
---

# Use the hf CLI

The `hf` CLI that comes with `huggingface_hub` works against Hangar with `HF_ENDPOINT`. The compatibility suite runs `hf auth login`, `hf auth whoami`, `hf repos create`, `hf upload` and `hf download` against Hangar on every pull request.

```sh
export HF_ENDPOINT=https://hangar.example.com
hf auth login --token hgr_...
hf auth whoami
hf download hf-internal-testing/tiny-random-bert --include "*.json"
hf repos create acme/my-model --private
hf upload acme/my-model ./weights
```

## Sign in

Use `hf auth login --token`, or the `HF_TOKEN` variable. The browser sign-in that `hf auth login` starts without `--token` also works: it opens Hangar's device page, where you approve the code, and Hangar creates a write Token for the CLI.

`hf` keeps one active Token in `~/.cache/huggingface/token`, whichever endpoint it came from. If you also use huggingface.co, set `HF_ENDPOINT` and `HF_TOKEN` in each shell instead of signing in to both:

```sh
HF_ENDPOINT=https://hangar.example.com HF_TOKEN=hgr_... hf auth whoami
```

`eval "$(hangar env)"` sets both for the Instance you signed in to with `hangar login`.

## Download

```sh
hf download acme/my-model                              # every file, into the Hugging Face cache
hf download acme/my-model config.json                  # one file
hf download acme/my-model --include "*.safetensors"    # files that match
hf download acme/my-model --revision dev               # a branch, a tag or a Commit SHA
hf download acme/my-model --local-dir ./my-model       # into a folder instead of the cache
hf download acme/reviews --repo-type dataset           # a dataset Repo
```

Mirrors keep the upstream Repo IDs and Commit SHAs, so the same commands and Revisions work against Hangar and huggingface.co. If a Repo is not in the Arsenal yet, pull it first with `hangar pull` or from **Discover**.

## Upload

```sh
hf repos create acme/my-model --private
hf upload acme/my-model ./weights .                            # a folder, as one Commit
hf upload acme/my-model ./weights . --commit-message "Add weights"
hf upload acme/my-model ./config.json copy/config.json --revision dev
```

Uploading to a branch that does not exist yet creates it. With `hf_xet` installed, large files go up as Xet uploads, which Hangar accepts with no setup. Every upload is a real Commit, the same as one made with git; see [Uploading](uploading.md).

## Errors you may see

| Error | Cause | Fix |
|---|---|---|
| "Invalid credentials in Authorization header" | The Token is wrong, expired or revoked, or it is a huggingface.co token (`hf_...`) rather than a Hangar one (`hgr_...`). | Create a Token in Hangar under **Settings**, **Tokens**. |
| `hf auth whoami` shows your huggingface.co account | `HF_ENDPOINT` is not set in this shell. | Export `HF_ENDPOINT`, or use `eval "$(hangar env)"`. |
| "Repository Not Found for url" | The Repo is private and your Token cannot read it, or it is not in the Arsenal. Private Repos answer as if they did not exist. | Check the Token, and pull the Repo first. |
| HTTP 409, saying the file is not in the arsenal | The Repo is a Partial mirror and the file was not pulled. | Download only the files it holds with `--include`, or run the `hangar pull` command it names. |

See also [Python libraries](python.md) and [Troubleshooting](../operate/troubleshooting.md).
