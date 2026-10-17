# 07. Indexing and scanning

## When it runs

- An `index` and a `scan` Job run for every Commit that becomes the head of any branch, whether it came from a push, an HTTP commit, a UI upload or a Pull.
- Both only read files whose content is present. In a Partial mirror they index what is present, and run again when a Pull adds files.
- Results are keyed by `(repo_id, commit, indexer_version)`. Bumping `indexer_version` re-indexes everything in the background, at low priority.
- Readers never wait for indexing. Until results exist, the Hub API omits the derived fields and the UI shows "Indexing".

## Reading without downloading

The indexer reads headers with ranged reads through `blob.Store.Get` with a `Range`. It never reads a whole large file.

- **safetensors:** read 8 bytes (little-endian u64 header length `n`), reject if `n` is over 100 MiB, then read `n` bytes of JSON.
- **GGUF:** use `gguf-parser-go` over a reader that fetches 1 MiB windows on demand. Header metadata can be tens of MiB because of tokenizer arrays; stop reading once tensor infos are parsed.
- Sharded files: read `model.safetensors.index.json` and every shard header; for GGUF splits, read the first shard's metadata and every shard's tensor count.

## What the index holds

`metadata.summary` (JSON) has these fields. The Hub API maps them to the Hub's fields ([03](03-hf-api.md)), and the UI and filters use them directly.

| Field | Source |
|---|---|
| `formats` | Set of `gguf`, `safetensors`, `mlx`, `onnx`, `pytorch`, `tensorflow`, `flax`, `coreml`, `openvino`, `tflite`, from file extensions and layout. MLX: safetensors plus `config.json` with a `quantization` or `quantization_config` block and an `mlx` tag or `library_name: mlx`. |
| `architecture` | `config.json` `architectures[0]` and `model_type`; GGUF `general.architecture`. |
| `parameters` | Total and per dtype: sum of tensor element counts from safetensors headers (the Hub's `safetensors.parameters` and `safetensors.total`), or GGUF tensor infos. |
| `quantisations` | Per GGUF file or group: quant name and `general.file_type`; for MLX, bits and group size from config. |
| `context_length` | GGUF `<arch>.context_length`; `config.json` `max_position_embeddings`, `n_positions`, `max_seq_len` or `seq_length`. |
| `chat_template` | GGUF `tokenizer.chat_template`; `tokenizer_config.json` `chat_template`; `chat_template.jinja`. |
| `tokens` | BOS and EOS tokens (the Hub's `gguf.bos_token` and `eos_token`). |
| `card_data` | YAML front matter of `README.md`, parsed safely (no anchors to arbitrary types, maximum 1 MiB). |
| `licence` | `card_data.license`, `license_name` and `license_link`; else a `LICENSE*` file's SPDX match. |
| `pipeline_tag`, `library_name` | `card_data` first; otherwise inferred: `config.json` architectures suffix (for example `ForCausalLM` gives `text-generation`), `sentence-transformers` files give `sentence-similarity`, `model_index.json` gives `diffusers`, GGUF only gives `gguf` library. |
| `tags` | `card_data.tags` plus derived tags (`gguf`, `safetensors`, `mlx`, licence, `base_model:<id>`, `region:` not used). |
| `size` | Total bytes, and bytes present. |
| `files` | Per file: format, dtype, quant, tensor count, parameters, for the file viewers. |

Dataset Repos also get the dataset fields in [14](14-datasets.md) (DATA-9).

All parsers are fuzz-tested (`go test -fuzz`) with seed corpora in `testdata/`. A malformed file yields a per-file `error` entry and never fails the whole index.

## Scanning

### Pickle scanner

Scans `.bin`, `.pt`, `.pth`, `.ckpt`, `.pkl`, `.pickle`, `.joblib`, `.npy` and `.npz`, and those names with a compression or archive suffix (`.gz`, `.bz2`, `.xz`, `.lzma`, `.z`, `.zst`, `.lz4`, `.zip`, `.tar`), such as `model.joblib.gz` or `checkpoint.pth.tar`, because `joblib.load` and `pandas.read_pickle` decompress them. Reads the pickle opcode stream without executing it. The content, not the name, decides how a file is read, the way `torch.load`, `joblib.load`, `numpy.load` and `pandas.read_pickle` read it:

- Zip archives: PyTorch archives (`*/data.pkl` and other `.pkl` members; tensor data members are skipped), `.npz` members and the member of a pandas zip.
- Compressed files (gzip, zlib, bz2, lz4, zstd): the raw bytes and the decompressed stream are both read. xz and lzma are not read, so their result is partial.
- `.npy` files: the pickle after the header when the dtype may hold objects; a file without the `.npy` magic is read as a pickle, as `numpy.load(allow_pickle=True)` does.
- Raw pickles, read as CPython's unpickler reads them, and also as `joblib.load` reads them when the stream names `NumpyArrayWrapper`: joblib reads NumPy array data from the file after each wrapper is built, and a pickle after it for object arrays.
- Data that is not a pickle at all (it fails at its first opcode), such as OpenVINO weights in a `.bin` file, is safe: no loader runs anything.
- Large files are read in full but streamed; the scanner stops where the unpickler stops.

A file whose content the scanner cannot follow to where every loader stops (an unsupported compressor, an old PyTorch tar archive, a joblib array it cannot measure, a stream beyond its limits) gets a **partial** result: its status is at least error and `partial` is set.

- Collect every import from `GLOBAL`, `STACK_GLOBAL` (tracking the preceding string pushes and memo), `INST` and the extension opcodes (`EXT1`, `EXT2`, `EXT4`, which name a registered global the scanner cannot see, so they count as an unsafe import).
- Classify each import under the name the pickle gives and the name `fix_imports` maps it to (`cPickle.loads` is `pickle.loads`). A dotted name, which protocol 4 resolves attribute by attribute, is unsafe when any part is a dunder or any tail of it is on the denylist (`collections.abc.sys.modules`).
- Classify each `module.name`:
  - **safe:** an allowlist of known tensor rebuild and container functions (for example `torch._utils._rebuild_tensor_v2`, `torch._utils._rebuild_parameter`, `collections.OrderedDict`, `torch.*Storage`, `numpy.core.multiarray._reconstruct`, `numpy.dtype`, `numpy.ndarray`, `builtins.set`, `_codecs.encode`).
  - **unsafe:** a denylist (for example `os.*`, `posix.*`, `nt.*`, `subprocess.*`, `builtins.eval`, `builtins.exec`, `builtins.compile`, `builtins.__import__`, `builtins.getattr`, `builtins.open`, `socket.*`, `shutil.*`, `sys.*`, `runpy.*`, `importlib.*`, `pickle.*`, `webbrowser.*`, `pty.*`, `requests.*`, `urllib.*`, `httplib.*`, `bdb.*`, `pdb.*`, `asyncio.*`, `torch.load`, `torch.hub.*`, `operator.attrgetter`, `functools.partial` when wrapping an unsafe target).
  - **suspicious:** anything else.
- Keep the allow and deny lists in `internal/scan/pickle/lists.go` with a source comment for each entry; seed them from the current picklescan and Hugging Face documentation, and cite them in `docs/research/pickle-scanning.md`.
- File status is the worst classification found. Repo status is the worst file status.

### Integrity checks

- safetensors: header JSON valid; offsets in range, non-overlapping and covering the data section; file size matches.
- GGUF: magic, version, tensor offsets within the file.
- LFS pointers: size in the pointer equals the object size.

### Where results show

- UI: a badge on the file and the Repo (**Safe**, **Suspicious imports**, **Unsafe imports**), with the import list on the file page.
- Hub API: `securityStatus` on model info when requested ([03](03-hf-api.md)) and per-file `securityFileStatus` in tree expansions where the Hub has them.
- Native API and webhook event `scan.completed`.
- Instance setting `scan.block_unsafe_downloads` (default false): when true, downloads of files marked unsafe need `?accept_risk=1` (UI) or return 403 with a clear message for API clients. Files that can run code when loaded (the names the pickle scanner reads) are also refused when their result is partial or they have no result yet. Every protocol that serves file bytes applies it through one gate (`scan.Gate`): the Hub API, git-lfs, git clone and fetch, Pull-through links, the Ollama registry and zip exports. Objects asked for by OID are decided from every file in the Repo's history that holds them; a git fetch is refused while any reachable Commit holds a refused file stored in git rather than LFS.
