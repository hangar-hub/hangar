---
title: "FAQ: a private, self-hosted Hugging Face Hub"
description: "Answers about Hangar: is it a model registry or a private Hugging Face Hub, does it run models, does it work air-gapped, and is it open source."
---

# Frequently asked questions

## What Hangar is

### Is Hangar a model registry?

It covers the storage and serving side of one, which some teams call a model repository or model store. Hangar is a self-hosted model and dataset hub: every Repo is a git repository with branches, tags and Commits, and clients load files through the Hugging Face Hub API, git, Ollama or LM Studio.

For experiment tracking and model lineage, pair it with MLflow: MLflow records runs and model versions, and Hangar stores and serves the files. See [How Hangar compares](concepts/comparison.md#model-registries-mlflow).

### Is Hangar a private Hugging Face Hub alternative?

Yes. Hangar serves the Hugging Face Hub API from your own servers and storage, so models and datasets stay on your network. Set `HF_ENDPOINT` to your Instance, and `huggingface_hub`, `transformers` and `datasets` use it in place of huggingface.co. Private Repos are visible only to the Users and Organisations you give access. See [Point `HF_ENDPOINT` at Hangar](get-started/hf-endpoint.md). For what Hugging Face itself offers, see [Hugging Face Hub on-premises](concepts/on-premises.md).

### Is Hangar a cache or proxy for Hugging Face models?

It does more than cache. Each Pull keeps a [Mirror](concepts/mirrors.md) of the Repo with its git history and the upstream Commit SHAs, so a Revision pinned in your code still resolves after the upstream Repo changes or disappears. You can also upload models that exist nowhere else. Clients load everything from your own storage.

With [Pull-through](concepts/mirrors.md#pull-through) on, Hangar also works as a caching proxy: it fetches a Repo from huggingface.co the first time a client asks for it. A plain caching proxy, such as olah, keeps only whatever clients happen to download. [How Hangar compares](concepts/comparison.md) sets the two kinds of tool side by side.

### Is Hangar like hf-mirror.com?

For your tools, yes: you set `HF_ENDPOINT` to Hangar, as you would to hf-mirror.com. hf-mirror.com is a public mirror of huggingface.co that someone else runs. Hangar runs on your own servers, holds only what you choose, controls who can download it, and works without internet access. See [Moving from a public mirror](get-started/hf-endpoint.md#moving-from-a-public-mirror).

### Does Hangar run models?

No. Hangar is a model storage server, not an inference server: it stores and serves files. Run models with the tools you already use, such as `transformers`, vLLM, Ollama or LM Studio, and point them at Hangar for the weights. Spaces, the Inference API, Hugging Face Jobs, Buckets and kernels, SSH git and Xet downloads are out of scope.

### Is there a hosted version?

No. Hangar is self-hosted only, and its licence does not let others offer it as a hosted or managed service. To look around without installing anything, open the [demo](https://demo.hangar.page/): it runs on sample data, and nothing you do there is saved.

## Using Hangar

### Do I need to change my code?

No. Set `HF_ENDPOINT` to your Instance and `HF_TOKEN` to a Token you create in Hangar. `huggingface_hub`, the `hf` CLI, `transformers`, `diffusers`, `sentence-transformers`, `datasets` and vLLM then work unchanged. Code that names a Repo, such as `org/model`, and a Revision keeps working, because Mirrors keep the upstream IDs and Commit SHAs.

### Which versions of my tools work?

[Tested versions and platforms](reference/compatibility.md) lists the versions of `huggingface_hub`, Python, `transformers`, git, Ollama, Postgres and Kubernetes that CI runs against, and what is documented but not tested. [Hub API compatibility](reference/hub-api-compatibility.md) lists every Hub endpoint and whether Hangar serves it.

### Does it work with Ollama, LM Studio and llama.cpp?

Yes. `ollama pull hangar.example.com/org/model:Q4_K_M` pulls a GGUF quantisation straight from your Instance, `hangar lmstudio install org/model:Q4_K_M` puts one into LM Studio's models folder, and llama.cpp downloads from Hangar with `MODEL_ENDPOINT`. Open WebUI pulls through its Ollama server. See [Ollama](guides/ollama.md), [LM Studio](guides/lm-studio.md) and [llama.cpp](guides/llama-cpp.md). To serve GGUF models to every Ollama install on your network, see [Run a private Ollama registry](guides/ollama-registry.md).

### Does it work with hf_xet?

Yes for uploads. Hangar accepts Xet uploads, so `huggingface_hub` with `hf_xet` installed works with no setup. It stores whole files and serves them over plain HTTP.

### Does Hangar scan models for malware?

Yes, for the formats that can run code. Pickle, PyTorch, joblib and NumPy files can run code when they are loaded, so a malicious model can hide malware in them. Hangar lists the imports of every such file at the tip of each branch, tag and pull request, marks unsafe and suspicious files in the web UI, and can refuse to serve them. Prefer safetensors, which cannot run code. See [Blocking unsafe downloads](operate/security.md#blocking-unsafe-downloads).

### Does it handle datasets?

Yes. Dataset Repos work with `load_dataset`, pandas and Polars over `hf://` and DuckDB over HTTPS, and the web UI has a Dataset Viewer. Hangar builds a Parquet export of each dataset, as huggingface.co does. See [Datasets](guides/datasets.md).

### Can Hangar run offline or air-gapped, with no internet access?

Yes. An Instance with no internet access pulls from another Hangar Instance that has it, as its Upstream. You can also carry a backup or a Hugging Face cache across and import it. See [Air-gapped Instances](guides/air-gapped.md).

## Running Hangar

### What does it need to run?

One container, or one binary with git 2.39 or later, and disk space for the models you keep. It scales out to several processes over Postgres and S3-compatible storage. See [System requirements](get-started/requirements.md).

### Does Hangar connect to anything on the internet?

Only to what you configure. The server connects to its database and Blob store, to Upstreams when someone pulls or a tracking Mirror checks for new Commits, to OIDC providers when people sign in, to webhook targets, and to an OpenTelemetry endpoint if you set one. huggingface.co is a built-in Upstream; turn it off with `upstreams.huggingface.enabled: false`. Hangar sends no usage data anywhere.

In the browser, the web UI links to these docs at https://hangar.page/ (change or remove the links with `ui.docs_url`), and Model cards load images from other sites unless an admin turns off **Load external images in cards** under **Admin**, **Settings**.

### What happens to my data if I stop using Hangar?

It stays usable without Hangar. Every Repo is a git repository, and large files are stored whole, under their SHA-256, in your folder or bucket, so you can copy them out with standard tools. See [Where Hangar keeps data](concepts/data.md) and [Uninstall Hangar](operate/uninstall.md).

### How do I upgrade, and which versions get fixes?

Pin a version, back up, then follow [Upgrades](operate/upgrades.md). Security fixes go into the latest minor release; see [Versioning and support](project/versioning.md).

### Where do I get help?

Check `hangar admin doctor` and [Troubleshooting](operate/troubleshooting.md) first, then open a GitHub issue. See [Get help](project/support.md).

## Licence and cost

### Is Hangar open source?

No, Hangar is source-available. The code is public on GitHub under the Elastic License 2.0, and you can read it, change it and self-host it for free, including inside your company. The licence is not OSI-approved open source because it stops others from offering Hangar as a hosted or managed service.

### What does it cost?

Nothing. Hangar is source-available under the Elastic License 2.0. You can use, change and self-host it, including inside your company. You can't offer it to others as a hosted or managed service, or remove licence notices.

### Can my company use it?

Yes, for free, on your own infrastructure. The limits are the ones above: you can't offer Hangar to others as a hosted or managed service, or remove licence notices. For anything else, read the [licence](project/licence.md) itself; this FAQ is not legal advice.
