---
title: Ollama pull from a private GGUF model registry
description: "Run GGUF models from Hangar in Ollama with ollama pull, as you would from huggingface.co, including private Repos and the hangar CLI installer."
---

# Use Ollama

This guide pulls GGUF models from Hangar into Ollama on your own machine. To set Hangar up as the Ollama registry for a team or for shared Ollama servers, see [Run a private Ollama registry](ollama-registry.md).

Hangar serves GGUF Repos to Ollama the way huggingface.co does. Ollama signs its requests with the key pair in `~/.ollama`, and Hangar needs that key for every Repo unless the Repo is public and the Instance allows anonymous reads (`security.anonymous_read`, off by default). Register the key once:

```sh
hangar ollama key register
```

or paste `~/.ollama/id_ed25519.pub` under **Settings**, **Ollama keys**. To prove the key is yours, Settings shows an `ssh-keygen -Y sign` command to run on the machine that has the key; paste its output as the signature. The CLI signs with `~/.ollama/id_ed25519` itself. The private key never leaves your machine. A key you paste in Settings reads every Repo you can read. A key the CLI registers is tied to the CLI's token: it reads only what that token can read, and stops working when the token expires or is revoked. Run `hangar ollama key register` again after you sign the CLI in with a new token. Then this works with no Modelfile:

```sh
ollama pull hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
ollama run hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

- The tag after `:` picks a quant, case-insensitively, or a full GGUF file name. Without a tag (Ollama's `latest`), Hangar picks `Q4_K_M` if the Repo has it, otherwise the closest quant.
- The chat template comes from the Repo's `template` file, or is matched from the GGUF's own chat template. `system`, `params` and `LICENSE` files in the Repo become the system prompt, parameters and licence.
- A matching `mmproj-*.gguf` projector is included for vision models.
- The quant must be in the Arsenal. For a [Partial mirror](../concepts/mirrors.md#partial-mirrors) without it, `ollama pull` fails with `file does not exist`: Ollama does not show the message Hangar sends with it, which names the Pull to run. Pull the quant first with `hangar pull <repo>:<quant> --server-only`, or use `hangar ollama install`, which checks for you. If an admin turned on [Pull-through](../concepts/mirrors.md#pull-through), the request starts the Pull and `ollama pull` asks you to try again when it finishes.

## Install with the hangar CLI

`hangar ollama install` runs `ollama pull` for you, after two checks:

```sh
hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

1. It checks that the quant is in the Arsenal. Without a quant, it checks the one Ollama asks for as `latest`. If the quant is missing, it says so, prints the command that pulls it into the Instance, and asks whether to run it:

    ```text
    bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M is not in the Arsenal, so Ollama would fail with "file does not exist". Pull it into the Instance first:
      hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M --server-only
    Pull it now? [y/N]
    ```

    Answer `y` to pull it and continue. Any other answer stops with exit code 3 and leaves the command for you to run. To pull without the question, add `--pull`.

2. It checks that your Ollama key is registered and offers to register it. Add `--yes` to register it without the question.

A quant the Repo does not have stops the command with the list of quants it does have.

## HTTPS or HTTP

Ollama uses HTTPS unless you pass `--insecure`. Use HTTPS with a trusted certificate (see [TLS and reverse proxies](../operate/proxies.md)), or for a plain-HTTP Instance on your network:

```sh
ollama pull --insecure hangar.lan:8080/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

`hangar ollama install` adds `--insecure` for `http://` Instances.

## Private Repos

With your key registered, Hangar gives Ollama a short-lived token for each Repo you can read, private Repos included.

## Limits

- `ollama push` to Hangar is not supported. Use `hangar import --from-ollama` to bring models you have in Ollama into the Arsenal.
- Ollama must reach Hangar on the host the model name gives, at the root of that host.
- On S3 in `redirect` mode, Hangar answers a model layer download with a redirect to a presigned URL, so `ollama pull` takes the bytes straight from the bucket, as `hf download` does. Ollama follows that redirect only to a public address unless you pass `--insecure`, so with a bucket on a private network, such as MinIO on your LAN, pull with `--insecure` or set `storage.s3.download_mode: proxy`.
