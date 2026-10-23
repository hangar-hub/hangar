---
title: Hugging Face rate limits and slow downloads in CI
description: "Fix 429 Too Many Requests and slow model downloads from huggingface.co in CI: tokens, caching, and serving models to GitHub Actions from Hangar."
---

# Avoid Hugging Face rate limits and slow downloads in CI

CI jobs often start with an empty Hugging Face cache, so every run downloads the same models from huggingface.co again. With parallel jobs and a test matrix, that can add up to enough requests to reach huggingface.co's rate limits, and the downloads slow every run down.

## The errors

When a client reaches a rate limit, huggingface.co answers with HTTP status `429 Too Many Requests`. Recent versions of `huggingface_hub` raise `HfHubHTTPError` with a message such as:

```text
429 Too Many Requests: you have reached your 'resolvers' rate limit.
Retry after 120 seconds (0/3000 requests remaining in current 300s window).
Url: https://huggingface.co/...
```

From version 1.2.0, before it gives up, `huggingface_hub` waits until the limit resets and retries, and logs lines such as `Rate limited. Waiting 121.0s before retry [Retry 1/5].`, which makes a job slow rather than failed. Versions before 1.0, built on `requests`, show the error with this message:

```text
429 Client Error: Too Many Requests for url: https://huggingface.co/...
```

`transformers`, `datasets`, `sentence-transformers` and vLLM download with `huggingface_hub`, so they show the same errors.

## Why CI reaches the limits

Hugging Face sets separate limits for API calls, for file downloads (which it calls resolvers) and for web pages, counted over 5-minute windows. Requests without a token count against the limit of the IP address they come from, which is the lowest limit. Requests with a token count against that account's limit. See [Hub rate limits](https://huggingface.co/docs/hub/rate-limits) for the current numbers.

CI makes this worse:

- Each job, and each cell of a test matrix, downloads the models again unless something restores a cache.
- Loading a model makes several requests, one for each file and some for metadata, so a test suite that loads a few models makes many.
- Workflows for pull requests from forks do not get your repository's secrets, so they run without `HF_TOKEN` even when other workflows have it.

## Fixes that need nothing new

These help whether or not you run Hangar:

1. Set `HF_TOKEN` in every job that downloads, so requests count against your account rather than the runner's IP address. This is the first fix Hugging Face's rate limits page suggests.
2. Keep the Hugging Face cache between runs. In GitHub Actions, restore `~/.cache/huggingface/hub` with `actions/cache`, as in the example below.
3. Load models at a Commit SHA (`revision=` in Python, `--revision` for `hf download`), so every run loads the same files, and change the cache key when you change a Revision.
4. With the cache restored, set `HF_HUB_OFFLINE=1` for the test step, so the libraries load from the cache without making any requests.

A cache still misses when the key changes or when GitHub evicts it, and every miss downloads from huggingface.co again.

## Serve models to CI from Hangar

A Hangar Instance keeps one copy of each model and serves it with the Hugging Face Hub API. With `HF_ENDPOINT` pointing at it, CI downloads from your Instance, and huggingface.co sees one download per file, from Hangar's Pull. Hangar does not rate-limit downloads.

### Set it up

1. [Run an Instance](../get-started/index.md) that the runners can reach. GitHub-hosted runners connect from the internet, so they need the Instance on a public address with TLS. Self-hosted runners on your own network can use a private one.
2. Pull the models your tests use into the Arsenal, at the Revisions they pin:

    ```sh
    hangar pull hf-internal-testing/tiny-random-bert --server-only
    hangar pull sentence-transformers/all-MiniLM-L6-v2 --revision <commit SHA> --server-only
    ```

    Mirrors keep the upstream Repo IDs and Commit SHAs, so tests pinned to a Commit SHA on huggingface.co work unchanged. If you would rather not list models up front, an admin can turn on [Pull-through](../concepts/mirrors.md#pull-through), so the first request for a model starts its Pull.

3. Save a huggingface.co token as the Instance token for the `huggingface` Upstream (`upstreams.huggingface.token`, or `HANGAR_UPSTREAMS_HUGGINGFACE_TOKEN`), so the Instance's own Pulls count against an account rather than its IP address. If huggingface.co still answers `429`, Hangar waits and tries again. A file that still fails after its retries leaves the Pull `partial`, with a **Retry failed** action.
4. Create a `read` Token for CI under **Settings**, **Tokens**, limited to the Repos the tests use, and store it as a secret in your CI system, such as `HANGAR_TOKEN` in GitHub Actions.

### GitHub Actions example

```yaml
name: Tests
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-24.04
    env:
      HF_ENDPOINT: https://hangar.example.com
      HF_TOKEN: ${{ secrets.HANGAR_TOKEN }}
      DISABLE_SAFETENSORS_CONVERSION: "1"
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: "3.12"
      - name: Restore the Hugging Face cache
        uses: actions/cache@v4
        with:
          path: ~/.cache/huggingface/hub
          # models.txt lists the Repo IDs and Revisions the tests load,
          # so the cache changes when they do.
          key: hf-hub-${{ hashFiles('models.txt') }}
      - run: pip install -r requirements.txt
      - run: pytest
```

The libraries read `HF_ENDPOINT` and `HF_TOKEN` from the environment, so the tests need no changes. Restoring the cache still saves time when the Instance is close by, because nothing is downloaded at all. `DISABLE_SAFETENSORS_CONVERSION` stops `transformers` from trying the conversion service on huggingface.co for models that only have pickle weights; see [Point HF_ENDPOINT at Hangar](../get-started/hf-endpoint.md).

To download everything in one step and run the tests with no network requests, add a step before the tests and set `HF_HUB_OFFLINE` for them:

```yaml
      - run: hf download sentence-transformers/all-MiniLM-L6-v2 --revision <commit SHA>
      - run: pytest
        env:
          HF_HUB_OFFLINE: "1"
```

Pull requests from forks get no `HANGAR_TOKEN`. To let them download anyway, keep the models they need in public Repos and turn on anonymous reads (`security.anonymous_read`). Anyone who can reach the Instance can then read every public Repo, so weigh that against keeping fork jobs off the Instance.

### Other CI systems

Any CI system works the same way: set `HF_ENDPOINT` to the Instance, `HF_TOKEN` to a Hangar Token from the system's secret store, and keep `~/.cache/huggingface/hub`, or `HF_HUB_CACHE` if you set it, between runs if the system can.

## Errors you may see with Hangar

| Error | Cause | Fix |
|---|---|---|
| "Repository Not Found for url" | The Repo is not in the Arsenal, or the Token cannot read it. Private Repos answer as if they did not exist. | Pull the Repo, and check the Token's scope and the Repos it is limited to. |
| "Invalid credentials in Authorization header" | `HF_TOKEN` holds a huggingface.co token (`hf_...`) rather than a Hangar Token (`hgr_...`), or the Token expired. | Store a Hangar Token in the secret. |
| `HfHubHTTPError` with status 409, naming a `hangar pull` command | The Repo is a Partial mirror and the file was not pulled. | Run the command it names, or pull the files the tests load. |
| `LocalEntryNotFoundError` with `HF_HUB_OFFLINE=1` | The files are not in the restored cache. | Download them in an earlier step, or change the cache key. |

## Related pages

- [Share one model cache across a team](shared-cache.md): the same idea for people and servers.
- [Python libraries](python.md) and [the hf CLI](hf-cli.md): `HF_ENDPOINT` with each client.
- [Permissions](../concepts/permissions.md#tokens): Token scopes and limits.
