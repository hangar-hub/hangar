# Use Ollama

Hangar serves GGUF Repos to Ollama the way huggingface.co does. Ollama signs its requests with the key pair in `~/.ollama`, and Hangar needs that key for every Repo unless the Repo is public and the Instance allows anonymous reads (`security.anonymous_read`, off by default). Register the key once:

```sh
hangar ollama key register
```

or paste `~/.ollama/id_ed25519.pub` under **Settings**, **Ollama keys**. Then this works with no Modelfile:

```sh
ollama pull hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
ollama run hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

- The tag after `:` picks a quant, case-insensitively, or a full GGUF file name. Without a tag (Ollama's `latest`), Hangar picks `Q4_K_M` if the Repo has it, otherwise the closest quant.
- The chat template comes from the Repo's `template` file, or is matched from the GGUF's own chat template. `system`, `params` and `LICENSE` files in the Repo become the system prompt, parameters and licence.
- A matching `mmproj-*.gguf` projector is included for vision models.
- The quant must be in the Arsenal. For a Partial mirror without it, `ollama pull` fails with `file does not exist`: Ollama does not show the message Hangar sends with it, which names the Pull to run. Pull the quant first with `hangar pull <repo>:<quant> --server-only`. If an admin turned on Pull-through, the request starts the Pull and `ollama pull` asks you to try again when it finishes.

The `hangar` CLI wraps this, and checks that your Ollama key is registered first and offers to register it:

```sh
hangar ollama install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

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
- Ollama must reach Hangar on the host the model name gives, at the root of that host. Ollama refuses redirects to other hosts, so Hangar always streams Ollama downloads itself, even on S3 in `redirect` mode.
