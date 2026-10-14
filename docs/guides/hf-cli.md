# Use the hf CLI

The `hf` CLI that comes with `huggingface_hub` works against Hangar with `HF_ENDPOINT`:

```sh
export HF_ENDPOINT=https://hangar.example.com
hf auth login --token hgr_...
hf auth whoami
hf download hf-internal-testing/tiny-random-bert --include "*.json"
hf repos create acme/my-model --private
hf upload acme/my-model ./weights
```

Use `hf auth login --token`, or the `HF_TOKEN` variable. The browser sign-in that `hf auth login` starts without `--token` also works: it opens Hangar's device page, where you approve the code, and Hangar creates a write Token for the CLI.

`hf` stores the Token per endpoint name in `~/.cache/huggingface/token`; if you also use huggingface.co, switch with `HF_ENDPOINT` and `HF_TOKEN` per shell instead of logging in to both.
