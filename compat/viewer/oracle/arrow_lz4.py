"""Write the LZ4-compressed Arrow IPC fixtures of internal/datasets/arrowipc.

Usage: python compat/viewer/oracle/arrow_lz4.py TESTDATA_DIR

The script reads TESTDATA_DIR/types.arrow, the uncompressed stream whose
rows are in types.json, and writes the same table as an IPC stream
(lz4.arrow) and as an IPC file (lz4_file.arrow) with every buffer
compressed as an LZ4 frame, the codec the `datasets` arrow builder and
pyarrow read. Small batches keep both buffer forms in use: pyarrow
leaves a buffer uncompressed (length -1) when compressing does not make
it smaller. Run it with the Hub worker's pyarrow:

    uv run --no-project --with pyarrow==24.0.0 \\
        python -I arrow_lz4.py internal/datasets/arrowipc/testdata
"""

from __future__ import annotations

import sys
from pathlib import Path

import pyarrow as pa


def main() -> int:
    out = Path(sys.argv[1])
    with pa.ipc.open_stream((out / "types.arrow").read_bytes()) as r:
        table = r.read_all()
    opts = pa.ipc.IpcWriteOptions(compression="lz4")
    sink = pa.BufferOutputStream()
    with pa.ipc.new_stream(sink, table.schema, options=opts) as w:
        w.write_table(table, max_chunksize=2)
    (out / "lz4.arrow").write_bytes(sink.getvalue().to_pybytes())
    sink = pa.BufferOutputStream()
    with pa.ipc.new_file(sink, table.schema, options=opts) as w:
        w.write_table(table, max_chunksize=2)
    (out / "lz4_file.arrow").write_bytes(sink.getvalue().to_pybytes())
    # Check the fixtures read back as the table pyarrow wrote.
    for name in ("lz4.arrow", "lz4_file.arrow"):
        data = (out / name).read_bytes()
        reader = pa.ipc.open_file(data) if name.endswith("_file.arrow") else pa.ipc.open_stream(data)
        assert reader.read_all().equals(table), name
    print("wrote lz4.arrow and lz4_file.arrow")
    return 0


if __name__ == "__main__":
    sys.exit(main())
