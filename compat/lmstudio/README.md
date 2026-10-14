# LM Studio layout suite

`compat/lmstudio` checks that `hangar lmstudio install` lays models out the way LM Studio reads them ([08](../../docs/spec/08-local-tools.md)): `<models folder>/<namespace>/<name>/<files>`, with the chosen GGUF quant and any `mmproj-*.gguf` projector, links that resolve into the Hugging Face cache, and `list` and `remove` working. It runs the real `hangar` binary.

```sh
go test -count=1 -tags compat ./compat/lmstudio/
```

| Variable | Effect |
|---|---|
| `HANGAR_LMSTUDIO_URL`, `HANGAR_LMSTUDIO_TOKEN` | Test an Instance that is already running, with a write Token. Without them the suite runs against the fake Instance in `internal/cli/clitest`. |
| `HANGAR_LMSTUDIO_NAMESPACE` | Namespace the test Repo is created in (default `acme`). |
| `HANGAR_BINARY` | A prebuilt `hangar` binary; otherwise the harness builds `./cmd/hangar`. |

## Manual check before a release

LM Studio is a desktop app, so it is not automated. Before tagging a release:

1. Install LM Studio and open it once.
2. Run `hangar lmstudio install <a GGUF Repo>:Q4_K_M` and `hangar lmstudio install <an MLX Repo>` (on Apple silicon) against a test Instance.
3. In LM Studio, open **My Models** and check both models are listed, then load each one and send a prompt.
4. Run `hangar lmstudio remove <repo>` and check LM Studio no longer lists the model.
