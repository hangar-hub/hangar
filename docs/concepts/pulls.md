---
description: "How a Pull fetches a Repo or some of its files from an Upstream, its lifecycle and network controls, and the remote code and base models it pulls too."
---

# Pulls and Dependencies

## A Pull's lifecycle

A Pull names an Upstream, a Repo, a Revision (the upstream default branch if you give none) and a file selection: everything, glob `include` and `exclude` patterns, explicit paths, GGUF quants, or for datasets Configs, Splits and Parquet-only.

1. **Plan.** Hangar resolves the Revision, fetches the upstream git history so Commits, trees and pointer files match exactly, lists the files with their sizes and SHA-256 values and applies the selection. Objects already anywhere in the Instance are marked present without a transfer. The Mirror's branches point at the upstream Commits as soon as planning finishes, so you can browse the history straight away.
2. **Transfer.** One `pull.file` Job per missing object streams it from the Upstream into the Blob store, hashing as it goes. Interrupted transfers resume with HTTP ranges, and an object that does not match its SHA-256 or size is rejected and retried, so nothing unverified becomes visible.
3. **Finish.** Hangar indexes and scans the new Commit, resolves Dependencies and fires the `pull.completed` webhook. If some files still failed after their retries, the Pull ends `partial`, with an error per file and a **Retry failed** action.

Small, non-LFS files come with the git history and need no transfer.

## Network controls

Admins can cap the bandwidth Pulls use across the whole Instance, limit how many files transfer at once, and confine Pulls to time windows such as `01:00-07:00`. See [Pull windows and bandwidth](../guides/pull-windows.md).

## Dependencies

After indexing a Commit, Hangar reads the files that name other Repos:

| Source | Finds | Kind | Pulled automatically |
|---|---|---|---|
| `auto_map` in `config.json`, `tokenizer_config.json` and the other config files | Remote code in another Repo, as in `jinaai/xlm-roberta-flash-implementation--modeling_lora.XLMRobertaLoRA` | `code` | Yes, recursively (`pull.dependencies.auto_pull_code`) |
| `adapter_config.json` `base_model_name_or_path` | A PEFT adapter's base model | `adapter_base` | Only with `pull.dependencies.auto_pull_base_models` |
| Model card `base_model` | Where the model came from | `card_base_model` | No |
| Model card `datasets` | Training datasets | `dataset` | No |
| `modules.json`, `model_index.json` | sentence-transformers and diffusers components in other Repos | `code` | Yes |

A Pull is not complete until its code Dependencies are, so `from_pretrained(..., trust_remote_code=True)` works offline right after it. Suggested Dependencies are listed on the Repo page and by `hangar info`, each with a one-click Pull.
