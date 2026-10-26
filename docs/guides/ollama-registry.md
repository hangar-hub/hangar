---
title: Run a private Ollama registry for GGUF models
description: "Run Hangar as a self-hosted Ollama registry: serve GGUF models from your own server to every Ollama install, with access control and no Modelfiles."
---

# Run a private Ollama registry

Hangar serves every GGUF Repo in its Arsenal as an Ollama registry. Ollama installs on your network pull from it by name, the same way they pull from huggingface.co, and the files stay on your own storage. There is no Modelfile to write and no separate registry to run.

This guide sets up the registry for a team or a fleet of Ollama servers. For pulling one model to your own machine, see [Ollama](ollama.md).

## How it works

An Ollama model name can start with a registry host. For a name such as `hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M`, Ollama asks `hangar.example.com` for the manifest of the `Q4_K_M` tag of `bartowski/Llama-3.2-3B-Instruct-GGUF`. Hangar builds the manifest from the Repo when Ollama asks for it:

- The model layer is the GGUF file for the quant the tag names, served from the Blob store without a copy.
- The chat template comes from the Repo's `template` file, or is matched from the GGUF file's own chat template.
- `system`, `params` and `LICENSE` files in the Repo become the system prompt, parameters and licence.
- A matching `mmproj-*.gguf` projector is included for vision models.

Every GGUF Repo works this way: Mirrors of huggingface.co Repos, Repos you uploaded, and models you imported from Ollama.

## Before you start

- A running Instance. See [Get started](../get-started/index.md).
- A host name for Hangar that every Ollama install can reach. Ollama asks for the registry at the root of the host, so serve Hangar at the root of its own host name, not under a path such as `example.com/hangar/`.
- HTTPS with a certificate the Ollama machines trust, which [TLS and reverse proxies](../operate/proxies.md) describes. Ollama uses HTTPS unless you pass `--insecure`.

## 1. Fill the registry

Pull the quants you want to offer from huggingface.co. `--server-only` puts them in the Arsenal without downloading them to your machine:

```sh
hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M --server-only
hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q8_0 --server-only
```

Each Pull keeps only the quants you chose, as a [Partial mirror](../concepts/mirrors.md#partial-mirrors). To follow new upstream Commits of the same quant, add `--track`.

To serve a GGUF model that is not on any Upstream, such as your own fine-tune, upload it to a Repo with its `template`, `system` and `params` files if it has them:

```sh
hangar push ./my-finetune-gguf acme/support-assistant-GGUF --create --private
```

To move models you already have in Ollama into the registry, import them. Each model and tag becomes a Repo in the Namespace you name:

```sh
hangar import --from-ollama --namespace acme
```

See [Uploading](uploading.md) and [Importing an existing cache](import.md).

## 2. Decide who can pull

Ollama signs every registry request with the key pair in the `.ollama` folder of the user that runs the Ollama server. Hangar reads Repos for a key only after someone registers it. Choose one of these ways.

### A key for each person

Each person registers their own key with `hangar ollama key register`, or pastes `~/.ollama/id_ed25519.pub` under **Settings**, **Ollama keys**. Their Ollama then reads what they can read, private Repos included.

### A key for each Ollama server

For a shared Ollama server, create a User for it, give that User `read` in the Organisations whose models it serves, sign in as it and paste the server's public key under **Settings**, **Ollama keys**. A key pasted in Settings reads every Repo its User can read, so keep that User's access narrow.

For the official `ollama/ollama` container image, the server runs as `root`, so its key is `/root/.ollama/id_ed25519.pub` inside the container:

```sh
docker exec ollama cat /root/.ollama/id_ed25519.pub
```

Keep `/root/.ollama` on a volume, as Ollama's own instructions do, so the key survives a new container and stays registered.

### No key

If the models are public Repos and the Instance allows anonymous reads (`security.anonymous_read`, off by default), Ollama pulls them without a key. Anyone who can reach the Instance can then read every public Repo, so use this only on a network you trust.

## 3. Pull from Ollama

On each machine, pull with the registry host in front of the Repo ID and the quant as the tag:

```sh
ollama pull hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
ollama run hangar.example.com/bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
```

- The tag picks a quant, ignoring case, or a full GGUF filename. Without a tag, Ollama asks for `latest`, and Hangar serves `Q4_K_M` if the Repo has it, otherwise the closest quant.
- For an Ollama server in a container, run the same command inside it, such as `docker exec ollama ollama pull hangar.example.com/...`.
- To give the model a shorter name on one machine, copy it with `ollama cp`, such as `ollama cp hangar.example.com/acme/support-assistant-GGUF:Q4_K_M support`.

On a plain-HTTP Instance on your own network, add `--insecure` to `ollama pull`. `hangar ollama install` runs `ollama pull` for you and adds `--insecure` for `http://` Instances. It also checks that the quant is in the Arsenal and that your key is registered first; see [Install with the hangar CLI](ollama.md#install-with-the-hangar-cli).

## Keep the registry up to date

- A tracking Mirror checks its upstream branch every six hours by default and pulls new versions of the quants it holds. Ollama gets the new file the next time it pulls the same name.
- To let people pull other quants of a Repo the registry already mirrors, an admin can turn on [Pull-through](../concepts/mirrors.md#pull-through). A request for a quant the Mirror does not hold then starts its Pull, and `ollama pull` asks you to try again when it finishes. A Repo that is not in the Arsenal at all still needs a Pull first.
- To keep large Pulls off your connection during working hours, see [Pull windows and bandwidth](pull-windows.md).

## Errors you may see

| Problem | Cause | Fix |
|---|---|---|
| `ollama pull` fails with `file does not exist` | The quant is not in the Arsenal. Ollama does not show the message Hangar sends with it, which names the Pull to run. | Pull the quant with `hangar pull <repo>:<quant> --server-only`, or use `hangar ollama install`, which checks first. |
| A private Repo fails to pull on one machine but works on another | Hangar does not know the key of the Ollama server that sent the request, or that key's User cannot read the Repo. Hangar answers a Repo you cannot read as if it did not exist. | Register the key of the machine that runs `ollama serve`, not only the one you type commands on, and check the User's Role. |
| A gated Repo fails to pull | The key's User has not been granted access. | Request access to the Repo in Hangar. See [Gated models](gated.md). |
| A TLS or certificate error | Ollama uses HTTPS, and the Instance has no certificate the machine trusts. | Set up TLS, or add `--insecure` on a network you trust. |

## Limits

- `ollama push` to Hangar is not supported. Upload with `hangar push`, `hf upload` or git, or use `hangar import --from-ollama`.
- Ollama loads one GGUF file per model, so Hangar does not serve a quant split into several files (`*-00001-of-0000N.gguf`), and says so in its answer.
- Ollama refuses redirects to other hosts, so Hangar streams Ollama downloads itself, even on S3 storage in `redirect` mode.
- Hangar's CI tests the registry with Ollama 0.31.1; see [Tested versions and platforms](../reference/compatibility.md).

## Related pages

- [Ollama](ollama.md): pull one model to your own machine.
- [Permissions](../concepts/permissions.md#ollama-keys): Ollama keys, Tokens and Roles.
- [Mirrors and Upstreams](../concepts/mirrors.md): Partial mirrors, tracking and Pull-through.
