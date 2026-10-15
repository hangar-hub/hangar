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
HANGAR_TEST_POSTGRES=1 go test -tags compat -count=1 ./compat/ollama/
HANGAR_OLLAMA_BINARY=/usr/local/bin/ollama go test -tags compat -count=1 ./compat/ollama/
```

Every scenario runs against each of these targets, with a new Ollama (empty model store, new key) for each:

| Target | What it runs |
|---|---|
| `in-process` | Hangar's Ollama registry in the test process, over the Repo service fake from `internal/repo/repotest`, with a key verifier that checks Ollama's signatures the way `auth.OllamaStore` does. |
| `binary-sqlite` | The `hangar` binary from `compat/harness` (built from `./cmd/hangar`, or `HANGAR_BINARY`) on a temporary SQLite database, with every real service wired. The suite signs in as the bootstrap admin, creates a Token, uploads the Repos with `hangar push`, and registers the Ollama key through `/api/hangar/v1/me/ollama-keys`. |
| `binary-postgres` | The same binary on a fresh Postgres database. It runs when `HANGAR_TEST_POSTGRES=1` (a `postgres:18-alpine` container, so Docker must be running) or `HANGAR_TEST_POSTGRES_DSN` names a server. `make test-compat` sets `HANGAR_TEST_POSTGRES=1`. |

For the binary targets, Ollama connects through a reverse proxy in the test process that keeps the `Host` header and records each request, so the suite can check the token endpoint and the signed, ranged blob downloads.

With a local binary, Ollama runs with its own temporary `HOME` and `OLLAMA_MODELS`, so it never touches your models or keys.

## Ollama limits

- Ollama 0.31.1 loads one model file per model, so Hangar refuses to serve a quant that is split into several GGUF files (`*-00001-of-0000N.gguf`) and says so in the error.
- Ollama reads the direct download URL of each blob from the `Location` header of its first `GET`, and sends its ranged part downloads there without an `Authorization` header. Hangar answers that `GET` with the blob and a `Location` header holding a signed URL of the same blob on the same host.
