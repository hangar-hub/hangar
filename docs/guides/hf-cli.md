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

`hf` keeps one active Token in `~/.cache/huggingface/token`, whichever endpoint it came from. If you also use huggingface.co, set `HF_ENDPOINT` and `HF_TOKEN` in each shell instead of signing in to both.
