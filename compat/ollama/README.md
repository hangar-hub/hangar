# Ollama compatibility suite

Runs the real Ollama client against Hangar's Ollama registry (DOWN-4, [08](../../docs/spec/08-local-tools.md) and [13](../../docs/spec/13-testing.md)).

## What it covers

- `ollama pull <host>/<ns>/<name>` (the `latest` tag) and `ollama pull <host>/<ns>/<name>:Q8_0` (a quant).
- `ollama show`, including the parameters and licence layers and the chat template.
- A one-token generation through the Ollama API (`/api/generate` with `num_predict: 1`), which loads the pulled GGUF file.
- A missing quant, which the pull reports as an error.
- A private Repo: the pull fails until the Ollama key is registered, then succeeds through the token endpoint (`/v2/token`), and the model runs.
- No redirects: every blob is served by Hangar, and Ollama's parallel ranged downloads go to Hangar's signed blob URLs.

## Versions and fixtures

- Ollama 0.31.1 (`ollama.TestedOllamaVersion`). The suite uses a local `ollama` binary of that version when `HANGAR_OLLAMA_BINARY` names one or `ollama` on `PATH` is that version; otherwise it runs the `ollama/ollama:0.31.1` image, reaching Hangar at `host.docker.internal`.
- `stories260K.gguf` (1.2 MB) from `ggml-org/tiny-llamas` at Commit `99dd1a73db5a37100bd4ae633f4cfce6560e1567`, cached in `compat/.fixtures` and checked against its SHA-256. The other GGUF files are generated in the test.

## Running

```sh
go test -tags compat -count=1 ./compat/ollama/
HANGAR_BINARY=bin/hangar HANGAR_OLLAMA_BINARY=/usr/local/bin/ollama go test -tags compat -count=1 ./compat/ollama/
```

The suite runs the real `hangar` binary with `compat/harness` (`HANGAR_BINARY`, or a build of `./cmd/hangar`), on SQLite and the filesystem Blob store with `security.anonymous_read` on, and seeds it through the native API: a user, a public and a private Repo, and the user's Ollama key, which `auth.OllamaStore` checks. A recording proxy in front of Hangar logs each request Ollama makes; it keeps the `Host` header, so the signed blob URLs point back at it. With a local binary, Ollama runs with its own temporary `HOME` and `OLLAMA_MODELS`, so it never touches your models or keys. CI runs the suite in the `compat` job of `.github/workflows/compat.yml` with the pinned image.

## Ollama limits

- Ollama 0.31.1 loads one model file per model, so Hangar refuses to serve a quant that is split into several GGUF files (`*-00001-of-0000N.gguf`) and says so in the error.
- Ollama reads the direct download URL of each blob from the `Location` header of its first `GET`, and sends its ranged part downloads there without an `Authorization` header. Hangar answers that `GET` with the blob and a `Location` header holding a signed URL of the same blob on the same host.
