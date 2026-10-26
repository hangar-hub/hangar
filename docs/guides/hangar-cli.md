---
description: "Use the hangar CLI to sign in, find and inspect Repos, pull and download, upload and import, follow Jobs and script Hangar."
---

# The hangar CLI

`hangar` is one binary for the server and the client. Install it as [Binary](../get-started/binary.md) describes. Every command is in the [CLI reference](../reference/cli.md).

## Sign in

```sh
hangar login --url https://hangar.example.com     # device sign-in in your browser
hangar login --url https://hangar.example.com --token hgr_...
hangar whoami
hangar logout
```

The device sign-in opens the Instance's **Connect a device** page. Type the code the CLI prints, check the client (`hangar-cli`), the time and the IP address, and choose `read` access, or `write` if you will push, upload or pull into the Instance. The Token expires in 30 days; run `hangar login` again then.

The URL and username are saved in `~/.config/hangar/config.yaml`; the Token goes in the OS keychain (macOS Keychain, Windows Credential Manager or the Secret Service on Linux) where one is available, otherwise in that file with mode 0600. In scripts and CI, set `HANGAR_CLI_URL` and `HANGAR_CLI_TOKEN` instead, or pass `--url` and `--token`. `HANGAR_CLI_CONFIG` points the CLI at another configuration file, and `HANGAR_CLI_NO_KEYCHAIN=1` keeps the Token in that file instead of the OS keychain.

## Find and inspect

```sh
hangar search llama --filter format=gguf --filter quant=Q4_K_M   # the Arsenal
hangar search qwen --upstream --filter gguf --sort downloads     # Discover on huggingface.co
hangar info bartowski/Llama-3.2-3B-Instruct-GGUF                 # files, quants, Dependencies, Arsenal state
hangar info datasets/lhoestq/demo1                               # Configs, Splits, rows and sizes
```

## Pull and download

```sh
hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
hangar pull hf-internal-testing/tiny-random-bert --include '*.json' --include '*.safetensors'
hangar pull Qwen/Qwen3-0.6B --server-only
hangar pull datasets/lhoestq/demo1 --to ./demo1
```

If the files are not in the Arsenal, `hangar pull` asks the Instance to pull them and shows the progress, then downloads them into the Hugging Face cache (`$HF_HUB_CACHE`, default `~/.cache/huggingface/hub`), in the same layout Python libraries use, so they reuse the files and so does the CLI. Downloads resume after an interruption, retry by themselves after a dropped connection or a busy proxy, and are checked against their SHA-256 or git object ID.

## Upload and import

```sh
hangar push ./my-finetune acme/my-finetune --create --private
hangar import --from-hf-cache
```

See [Uploading](uploading.md) and [Importing an existing cache](import.md).

## Local tools

```sh
eval "$(hangar env)"                                          # HF_ENDPOINT and HF_TOKEN for Python tools
hangar lmstudio install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

## Jobs

```sh
hangar jobs list
hangar jobs watch <job-id>
hangar jobs cancel <job-id>
```

## Scripting

Every command takes `--json` and prints JSON on standard output; progress goes to standard error. `hangar server` and `hangar worker` print only logs, so for them `--json` selects JSON logs, which is also the default. Exit codes: `0` success, `1` error, `2` usage error, `3` not found, `4` authentication error.
