# Pickle scanning

Research behind Hangar's pickle scanner (META-2, [07](../spec/07-indexing-and-scanning.md) "Pickle scanner"). Sources were read on 2026-10-13. The scanner is `internal/scan/pickle`; its allow and deny lists are `internal/scan/pickle/lists.go`, where every entry names its source.

## Sources

| Tag in `lists.go` | Source |
|---|---|
| `[picklescan]` | `mmaitre314/picklescan` 1.0.5, `src/picklescan/scanner.py`: `_safe_globals`, `_unsafe_globals` and the matching rules. |
| `[torch]` | PyTorch 2.9.0, `torch/_weights_only_unpickler.py`: `_get_allowed_globals` and `_tensor_rebuild_functions`, the globals `torch.load(weights_only=True)` accepts. |
| `[hf]` | Hugging Face Hub documentation, "Pickle Scanning" (`https://huggingface.co/docs/hub/security-pickle`). |
| `[spec]` | [07](../spec/07-indexing-and-scanning.md). |

## Why imports are what matters

- A pickle is a program for a small stack machine. `GLOBAL`, `STACK_GLOBAL` and `INST` import a callable by `module` and `name`; `REDUCE`, `BUILD`, `INST`, `OBJ`, `NEWOBJ` and `NEWOBJ_EX` call it. An unpickler runs the program as it reads it ([hf], "What is a pickle?"; Python's `Lib/pickle.py` and `Lib/pickletools.py`).
- The Hub lists the imports of every pickle it scans, read with `pickletools.genops` so nothing runs, and highlights suspicious ones ([hf], "Hub's Security Scanner"). Hangar does the same in Go, so it needs no Python.

## How the scanner reads a pickle

- It walks opcodes of protocols 0 to 5 and keeps a model of the stack: strings pushed by the string opcodes, marks, and an opaque value for everything else. `MEMOIZE`, `PUT`, `BINPUT` and `LONG_BINPUT` store the top of the stack; `GET`, `BINGET` and `LONG_BINGET` push it back. This is enough to name the module and attribute of a `STACK_GLOBAL` whose arguments come from the memo.
- When a `STACK_GLOBAL` argument is not a known string (it was computed by a call), the import cannot be named. picklescan reports such imports as dangerous; Hangar classifies them as unsafe with the name `?.?`.
- Large `BINBYTES`, `BINUNICODE` and similar arguments are skipped as they stream past; only the first 4 KiB of a string is kept. The stack and memo are capped at a million entries.
- Python 2 module names are mapped to Python 3 ones (`__builtin__` to `builtins`, `copy_reg` to `copyreg`), as `pickle`'s `fix_imports` does, so `__builtin__.eval` and `builtins.eval` are both unsafe.

## File types

- **Zip-based PyTorch archives** (`torch.save` since PyTorch 1.6): a zip file whose `data.pkl` holds the object pickle and whose other members hold raw tensor data (PyTorch 2.9.0 `torch/serialization.py`: `_save` writes `data.pkl`, `_load` reads it). Hangar scans every member that ends in `.pkl` and skips the rest. When it scans a Commit, it reads the zip's central directory and the `.pkl` members with ranged reads, so tensor data is never read. `ScanFile` streams the archive instead, reading each local file header and discarding non-pickle members as they pass.
- **Legacy PyTorch files**: five pickles back to back (a magic number, the protocol version, system information, the object, then the storage keys), followed by raw storage bytes (PyTorch 2.9.0 `torch/serialization.py`, `_legacy_load`). The scanner reads up to five pickles in any protocol, then more only while they start with `PROTO`, and stops at the first data that is not a pickle. It keeps the imports of a pickle that fails part-way, because an unpickler would already have run them.
- **NumPy `.npy`**: arrays with an object dtype (`descr` such as `|O`) store a pickle after the header, which `numpy.load` only reads with `allow_pickle=True`. Other arrays hold raw numbers and are safe without scanning.
- **joblib**: a pickle, optionally compressed. Hangar reads zlib, gzip and bz2. xz and lz4 files get an error result rather than a guess. joblib writes NumPy array data inside the pickle stream, which the opcode parser cannot read past, so some joblib files end in an error result after their imports.
- **Raw pickles**: `.pkl`, `.pickle`, `.bin`, `.pt`, `.pth` and `.ckpt` that are not zip archives. A `.bin` file with an `.xml` file of the same name is OpenVINO weights and is not scanned.

## Classification

- **Unsafe**: the deny list. It is picklescan's `_unsafe_globals` in full, plus the names [07](../spec/07-indexing-and-scanning.md) adds (`importlib.*`, `http.client.*`, `torch.load`, `torch.hub.*`, `multiprocessing.*`, `marshal.*`, `builtins.__import__`). A `module.*` entry matches the module and its submodules, as picklescan's parent-module check does.
- **Safe**: the allow list. It is picklescan's `_safe_globals` plus the globals PyTorch's weights-only unpickler accepts by default (tensor rebuild functions, `torch.Size`, `torch.device`, `torch.nn.parameter.Parameter`, dtypes and quantisation schemes, `builtins.set`, `builtins.bytearray`, `builtins.complex`, `_codecs.encode`, `collections.Counter`). Torch storage classes (`torch.*Storage`) and NumPy dtype classes (`numpy.dtypes.*DType`) match by suffix. `numpy.core.multiarray.scalar`, which NumPy pickles of scalars use, is also allowed.
- **Suspicious**: everything else, such as model classes from `transformers` or `sklearn`. Loading them runs their constructors, so a person should check them.
- **`functools.partial`**: [07](../spec/07-indexing-and-scanning.md) makes it unsafe only when it wraps an unsafe target; picklescan treats it as unsafe outright. Hangar classifies it as suspicious, and as unsafe when the same file also imports an unsafe global.
- A file's status is its worst import; a Repo's status is its worst file. A file that cannot be parsed gets the `error` status, unless it has worse findings ([07](../spec/07-indexing-and-scanning.md) orders them safe, error, suspicious, unsafe).

## Known limits

- The scanner trusts the structure it can see. A pickle can build a callable without importing it by name only through an import, so every way to reach code goes through an import the scanner lists; but a safe-listed callable that is later found to be exploitable would pass until the lists change.
- Pickles nested inside other formats that Hangar does not open (for example a pickle stored as a string inside another pickle, or inside `.npz` files) are not scanned. picklescan scans `.npz`; Hangar does not in v1.0.
- Results are keyed by scanner version (`scan.Version`). Change it with the lists: `scan.Service.RescanStale` then queues scans of every branch, tag and pull request tip with older results, which stay readable until the new ones are stored.
