# Air-gapped Instances

An Instance with no internet access can be fed from another Hangar that has it.

1. Run an outer Instance with internet access, and pull what the inner network needs into it.
2. On the inner Instance, add the outer one as an Upstream: **Admin**, **Upstreams**, **Add upstream**, with its URL and a read Token for it as the Instance token. Or in configuration:

   ```yaml
   upstreams:
     outer:
       base_url: https://hangar-outer.example.com
       enabled: true
       token: hgr_...
     huggingface:
       enabled: false
   ```

3. Pull from it like from huggingface.co: **Discover** lets you choose the Upstream, and the CLI takes `--upstream outer`:

   ```sh
   hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M --upstream outer
   ```

Mirrors keep the Repo IDs and Commit SHAs from the outer Instance, which kept them from huggingface.co, so `HF_ENDPOINT` swaps still work inside.

## Fully disconnected networks

If no connection is allowed at all, move data with a backup instead: back up the outer Instance (`hangar admin backup`), carry the folder across, and restore it into the inner one (`hangar admin restore`). See [Backup and restore](../operate/backup.md). Or carry a Hugging Face cache across and run `hangar import --from-hf-cache --offline` on the inside.
