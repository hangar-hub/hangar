---
title: Use Hugging Face models offline or air-gapped
description: "Run Hangar offline as an air-gapped model and dataset store: feed it from another Instance, or carry a backup or a Hugging Face cache across."
---

# Air-gapped Instances

An Instance with no internet access, also called an offline or air-gapped Instance, can be fed from another Hangar Instance that has internet access. Clients on the inside load models and datasets from it with `HF_ENDPOINT`, as they would from huggingface.co. To load files a client already downloaded with no network requests at all, set `HF_HUB_OFFLINE=1` on the client; see [Python libraries](python.md).

1. Run an outer Instance with internet access, and pull what the inner network needs into it.
2. On the inner Instance, add the outer one as an Upstream: **Admin**, **Upstreams**, **Add upstream**, with its URL and a read Token for it as the Instance token. Or in configuration:

   ```yaml
   upstreams:
     outer:
       base_url: https://hangar-outer.example.com
       enabled: true
       token: hgr_...
     huggingface:
       enabled: false
   ```

3. Pull from it like from huggingface.co: **Discover** lets you choose the Upstream, and the CLI takes `--upstream outer`:

   ```sh
   hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M --upstream outer
   ```

Mirrors keep the Repo IDs and Commit SHAs from the outer Instance, which kept them from huggingface.co, so `HF_ENDPOINT` swaps still work inside.

## Keep the web UI on the inside

The web UI links to the documentation at https://hangar.page/, for example from the explanation that opens when you hover over or click a badge such as **Unsafe imports**. On a network without internet access, point those links at a copy of the documentation, or turn them off, with `ui.docs_url` (`HANGAR_UI_DOCS_URL`).

To use a copy, build the documentation from the Hangar source at the release tag your Instance runs, on a machine with internet access, and serve the `site` folder on the inside. The copy must serve the same paths as https://hangar.page/, such as `concepts/mirrors/`. `hangar version` prints the version, and so does **Admin**, **System** in the web UI. For version 1.2.3:

```sh
git clone --branch v1.2.3 https://github.com/hangar-hub/hangar.git
cd hangar
uv run --no-project --with-requirements docs/requirements.txt mkdocs build
```

The build downloads the fonts, and the script that draws the native API reference, into `site`, so the copy loads nothing from the internet. Copies built from 1.2.3 and earlier releases load that script from `cdn.jsdelivr.net`, so their [Native API](../reference/native-api.md) page stays blank without internet access; the other pages work.

Then set the URL of the copy:

```sh
HANGAR_UI_DOCS_URL=https://docs.internal.example.com/hangar/
```

To remove the links, set `HANGAR_UI_DOCS_URL=` (empty), or `docs_url: ""` under `ui` in the configuration file. The explanations still show, without the link.

Also turn off **Load external images in cards** under **Admin**, **Settings**, so Model cards do not try to load images from the internet.

## Fully disconnected networks

If no connection is allowed at all, move data with a backup instead: back up the outer Instance (`hangar admin backup`), carry the folder across, and restore it into the inner one (`hangar admin restore`). See [Backup and restore](../operate/backup.md). Or carry a Hugging Face cache across and run `hangar import --from-hf-cache --offline` on the inside.
