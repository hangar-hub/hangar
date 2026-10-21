---
title: Serve vLLM models from a private model hub
description: "Serve models with vLLM from weights stored in Hangar, by pointing its huggingface_hub downloads at your Instance with HF_ENDPOINT."
---

# Use vLLM

vLLM downloads models with `huggingface_hub`, so `HF_ENDPOINT` points it at Hangar:

```sh
export HF_ENDPOINT=https://hangar.example.com
export HF_TOKEN=hgr_...
vllm serve Qwen/Qwen3-0.6B
```

In a container:

```sh
docker run --gpus all -p 8000:8000 \
  -e HF_ENDPOINT=https://hangar.example.com -e HF_TOKEN=hgr_... \
  -v ~/.cache/huggingface:/root/.cache/huggingface \
  vllm/vllm-openai:latest --model Qwen/Qwen3-0.6B
```

Pull the model into the Arsenal first (or turn on Pull-through), and pull its tokenizer and config files along with the weights. Mounting the cache means a restart loads from disk without asking Hangar again.

For a fully offline start, download once and set `HF_HUB_OFFLINE=1`:

```sh
hangar pull Qwen/Qwen3-0.6B
HF_HUB_OFFLINE=1 vllm serve Qwen/Qwen3-0.6B
```
