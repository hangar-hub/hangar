# Pickle scanning

Research behind Hangar's pickle scanner (META-2, [07](../spec/07-indexing-and-scanning.md) "Pickle scanner"). Sources were read on 2026-10-13. The scanner is `internal/scan/pickle`; its allow and deny lists are `internal/scan/pickle/lists.go`, where every entry names its source.

## Sources

| Tag in `lists.go` | Source |
|---|---|
| `[picklescan]` | `mmaitre314/picklescan` 1.0.5, `src/picklescan/scanner.py`: `_safe_globals`, `_unsafe_globals` and the matching rules. |
| `[torch]` | PyTorch 2.9.0, `torch/_weights_only_unpickler.py`: `_get_allowed_globals` and `_tensor_rebuild_functions`, the globals `torch.load(weights_only=True)` accepts. |
| `[hf]` | Hugging Face Hub documentation, "Pickle Scanning" (`https://huggingface.co/docs/hub/security-pickle`). |
| `[spec]` | [07](../spec/07-indexing-and-scanning.md). |
| `[hangar]` | The phase 3 security review (`docs/build/handoffs/p3-scan.md`): functions found to run code, load native code or another pickle, or reach the import system, checked against CPython 3.11, PyTorch 2.7, NumPy 2.2, joblib 1.5 and pandas 2.2. |

## Why imports are what matters

- A pickle is a program for a small stack machine. `GLOBAL`, `STACK_GLOBAL` and `INST` import a callable by `module` and `name`; `REDUCE`, `BUILD`, `INST`, `OBJ`, `NEWOBJ` and `NEWOBJ_EX` call it. An unpickler runs the program as it reads it ([hf], "What is a pickle?"; Python's `Lib/pickle.py` and `Lib/pickletools.py`).
- The Hub lists the imports of every pickle it scans, read with `pickletools.genops` so nothing runs, and highlights suspicious ones ([hf], "Hub's Security Scanner"). Hangar does the same in Go, so it needs no Python.

## How the scanner reads a pickle

- It walks opcodes of protocols 0 to 5 the way CPython's unpickler does, and keeps a model of the stack: strings pushed by the string opcodes, marks, and an opaque value for everything else. `MEMOIZE`, `PUT`, `BINPUT` and `LONG_BINPUT` store the top of the stack; `GET`, `BINGET` and `LONG_BINGET` push it back. Memo indexes are tracked exactly, so `MEMOIZE` numbers entries as the unpickler does. This is enough to name the module and attribute of a `STACK_GLOBAL` whose arguments come from the memo.
- Protocol 0 arguments are decoded as Python decodes them: `UNICODE` with raw-unicode-escape (`\u006f` is `o`), `STRING` with `escape_decode` (a two-digit octal escape such as `\56` is `.`), and `GET` and `PUT` indexes as `int()` reads them (underscores, spaces and leading zeros). Lines of any length are read; only the first 64 KiB is kept.
- When a `STACK_GLOBAL` argument is not a known string (it was computed by a call, or was too long to keep), the import cannot be named. picklescan reports such imports as dangerous; Hangar classifies them as unsafe with the name `?.?`. The extension opcodes (`EXT1`, `EXT2`, `EXT4`) push a global registered with `copyreg.add_extension`, which the scanner cannot name either, so they count as unsafe `?.?` imports.
- Large `BINBYTES`, `BINUNICODE` and similar arguments are skipped as they stream past; only the first 4 KiB of a string is kept, and an import whose name was cut is unsafe. Strings kept on the stack and in the memo are capped at 64 MiB each; a memo string past the cap reads back as unknown.
- Every stop is one of two kinds. **Invalid** means the unpickler raises an error at the same point, so nothing after it runs; the imports before it are the file's. **Unsupported** means the scanner cannot follow the stream any further although an unpickler might (a stack deeper than a million entries, a joblib array it cannot measure, an unreadable compressor): the result is partial, and `scan.block_unsafe_downloads` refuses the file.

### How joblib reads a pickle

`joblib.load` does not read the opcode stream the way `pickle.load` does. Its `NumpyUnpickler` (joblib 1.5.3, `joblib/numpy_pickle.py`) subclasses `pickle._Unpickler` and, after every `BUILD` of a `joblib.numpy_pickle.NumpyArrayWrapper`, reads the array from the file: a padding length byte and padding when the wrapper has `numpy_array_alignment_bytes`, then `count * itemsize` bytes, or, for an object dtype, a whole pickle with `pickle.load`. So the same bytes are opcodes to `pickle.load` and array data to `joblib.load`. A file can hide a call inside what `pickle.load` reads as a `BINBYTES` argument and `joblib.load` skips as array data; the scanner used to read only the `pickle.load` way.

- The scanner reads every stream that names `NumpyArrayWrapper` (or `joblib`) a second time, as joblib does, and every `.joblib` file both ways. The imports of both readings are the file's.
- In the joblib reading it tracks ints, tuples, lists, dicts and instances, and works out the array size from the wrapper's `shape` and `dtype` as NumPy does after `numpy.dtype.__setstate__`: `hasobject` from bit 0 of the state's flags, and the item size from the type string, or from the state's `elsize` for `S`, `U` and `V` types. Type strings whose size depends on the platform (`f16`, `g`, `l`) are unsupported.
- `pickle._Unpickler` reads opcodes from the current protocol 4 frame, while the array data comes from the file after the frame; the joblib reading keeps the frame apart in the same way.
- Where joblib's choice depends on something the scanner cannot see, the result is partial: an object array whose `allow_mmap` is set (with `mmap_mode`, joblib maps the bytes instead of unpickling them), or `BUILD` on a value that may be a wrapper but was made by a call the scanner cannot follow.

## File types

- **Content decides.** The name only decides whether a file is read (see [07](../spec/07-indexing-and-scanning.md)); what it holds decides how. A `.bin` file of OpenVINO or other raw weights fails at its first opcode, so it is not a pickle and is safe. Before the phase 3 review the scanner skipped any `.bin` with an `.xml` file of the same name, so a pickle named `pytorch_model.bin` with an empty `pytorch_model.xml` beside it was never read.
- **Zip archives**: zip-based PyTorch archives (`torch.save` since PyTorch 1.6) hold the object pickle in `data.pkl` and raw tensor data in other members (PyTorch 2.9.0 `torch/serialization.py`). Hangar scans every `.pkl` member of such an archive and skips the rest, reading only the central directory and those members with ranged reads. In other zip archives it scans `.npy` members as NumPy files (`.npz`, which `numpy.load` opens whatever the name) and every other member as a pickle (`pandas.read_pickle` of a `.zip` unpickles its one member, whatever its name).
- **Compressed files**: `joblib.dump` and `pandas.to_pickle` compress by extension and their readers decompress by magic number. Hangar reads gzip, zlib, bz2, lz4 and zstd, and scans both the raw bytes (as `torch.load` and `pickle.load` read them) and the decompressed stream. xz and lzma give a partial result, because Go's standard library cannot read them.
- **Legacy PyTorch files**: five pickles back to back (a magic number, the protocol version, system information, the object, then the storage keys), followed by raw storage bytes (PyTorch 2.9.0 `torch/serialization.py`, `_legacy_load`). The scanner reads up to five pickles in any protocol, then more only while they start with `PROTO`, and stops at the first data that is not a pickle. A limit reached in any of the five pickles makes the result partial. Older PyTorch files that are tar archives are partial.
- **NumPy `.npy`**: arrays whose `descr` is not one simple numeric or string type store a pickle after the header, which `numpy.load` only reads with `allow_pickle=True`. A file without the `.npy` magic is read as a pickle, because `numpy.load(allow_pickle=True)` unpickles it.
- **joblib**: see "How joblib reads a pickle".

## Classification

- **Unsafe**: the deny list. It is picklescan's `_unsafe_globals` in full, plus the names [07](../spec/07-indexing-and-scanning.md) adds (`importlib.*`, `http.client.*`, `torch.load`, `torch.hub.*`, `multiprocessing.*`, `marshal.*`, `builtins.__import__`). A `module.*` entry matches the module and its submodules, as picklescan's parent-module check does.
- **Safe**: the allow list. It is picklescan's `_safe_globals` plus the globals PyTorch's weights-only unpickler accepts by default (tensor rebuild functions, `torch.Size`, `torch.device`, `torch.nn.parameter.Parameter`, dtypes and quantisation schemes, `builtins.set`, `builtins.bytearray`, `builtins.complex`, `_codecs.encode`, `collections.Counter`). Torch storage classes (`torch.*Storage`) and NumPy dtype classes (`numpy.dtypes.*DType`) match by suffix. `numpy.core.multiarray.scalar`, which NumPy pickles of scalars use, is also allowed.
- **Suspicious**: everything else, such as model classes from `transformers` or `sklearn`. Loading them runs their constructors, so a person should check them.
- **`functools.partial`**: [07](../spec/07-indexing-and-scanning.md) makes it unsafe only when it wraps an unsafe target; picklescan treats it as unsafe outright. Hangar classifies it as suspicious, and as unsafe when the same file also imports an unsafe global.
- A file's status is its worst import; a Repo's status is its worst file. A file that cannot be parsed gets the `error` status, unless it has worse findings ([07](../spec/07-indexing-and-scanning.md) orders them safe, error, suspicious, unsafe).

## Known limits

- Only imports on the deny list are unsafe; everything not on either list is suspicious, and `scan.block_unsafe_downloads` does not refuse suspicious files. A function that runs code and is on neither list passes the setting. The deny list is picklescan's plus the functions the phase 3 review found; it cannot be complete.
- A function's result can be another object the scanner cannot see. In the joblib reading, an instance built from values that name joblib is refused as partial, but a call that returns a `NumpyArrayWrapper` without being passed anything that names it (for example through a module that imports the class under another name) is not recognised. Such a pickle still lists the import it calls, as suspicious or unsafe.
- Files are read when their name is one a standard loader uses. A pickle under another name, loaded by a Repo's own code with `trust_remote_code`, is not scanned; that code already runs whatever it wants.
- Results are keyed by scanner version (`scan.Version`, 2 since the phase 3 review). Change it with the lists: `scan.Service.RescanStale` then queues scans of every branch, tag and pull request tip with older results, which stay readable until the new ones are stored. An older result with an error counts as partial.
