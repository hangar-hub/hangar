"""Record how the Hub's Dataset Viewer encodes cells, for internal/datasets/viewer.

Usage: python compat/viewer/oracle/cells_cases.py OUT_DIR MEDIA_DIR

Writes OUT_DIR/cells.parquet, a table with one column per Arrow type the
viewer encodes without media, and OUT_DIR/cells.json, the rows as the
Hub's /rows returns them: pyarrow's to_pylist() serialised with
libcommon.utils.orjson_dumps (orjson with OPT_UTC_Z, OPT_SERIALIZE_NUMPY,
OPT_NON_STR_KEYS and base64 for bytes, str() for anything else). It also
writes OUT_DIR/media.parquet, with images and audio nested in lists and
structs and the Features in the `huggingface` metadata, and
OUT_DIR/media.json, the asset file names libcommon's append_hash_suffix
gives each media cell's JSON path. MEDIA_DIR holds the image and audio
files to embed. Run it
with the Hub worker's versions:

    uv run --no-project --with pyarrow==24.0.0 --with orjson==3.11.3 \\
        --with pandas==2.3.3 python -I cells_cases.py OUT_DIR MEDIA_DIR
"""

from __future__ import annotations

import base64
import datetime as dt
import decimal
import json
import sys
import zlib
from pathlib import Path

import orjson
import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq


def orjson_default(obj):
    if isinstance(obj, bytes):
        return base64.b64encode(obj).decode("utf-8")
    if isinstance(obj, pd.Timestamp):
        return obj.to_pydatetime()
    return str(obj)


def dumps(v) -> bytes:
    return orjson.dumps(v, option=orjson.OPT_UTC_Z | orjson.OPT_SERIALIZE_NUMPY | orjson.OPT_NON_STR_KEYS, default=orjson_default)


utc = dt.timezone.utc
cols = {
    "i8": (pa.int8(), [-128, 0, 127, None]),
    "u64": (pa.uint64(), [0, 2**64 - 1, 7, None]),
    "f32": (pa.float32(), [1.5, float("nan"), float("inf"), None]),
    "f64": (pa.float64(), [1.0, 1e-7, 123456789.125, -0.0]),
    "f64big": (pa.float64(), [1e16, 1e22, 2.5e-300, float("-inf")]),
    "b": (pa.bool_(), [True, False, None, True]),
    "s": (pa.string(), ["plain", "ünïcödé", "", None]),
    "ls": (pa.large_string(), ["<a &amp; b>", None, "c\nd", "\"q\""]),
    "bin": (pa.binary(), [b"\x00\x01", b"", None, b"hello"]),
    "d32": (pa.date32(), [dt.date(2024, 1, 2), dt.date(1969, 12, 31), None, dt.date(1, 1, 1)]),
    "ts_s": (pa.timestamp("s"), [dt.datetime(2024, 1, 2, 3, 4, 5), None, dt.datetime(1970, 1, 1), dt.datetime(1999, 12, 31, 23, 59, 59)]),
    "ts_us": (pa.timestamp("us"), [dt.datetime(2024, 1, 2, 3, 4, 5, 123456), None, dt.datetime(1960, 6, 1, 0, 0, 0, 1), dt.datetime(2024, 1, 2)]),
    "ts_tz": (pa.timestamp("ms", tz="UTC"), [dt.datetime(2024, 1, 2, 3, 4, 5, 6000, tzinfo=utc), None, dt.datetime(2000, 1, 1, tzinfo=utc), dt.datetime(2024, 1, 2, tzinfo=utc)]),
    "t64": (pa.time64("us"), [dt.time(12, 34, 56), dt.time(0, 0, 0, 1), None, dt.time(23, 59, 59, 999999)]),
    "t32": (pa.time32("ms"), [dt.time(1, 2, 3), dt.time(1, 2, 3, 4000), None, dt.time(0, 0)]),
    "dur": (pa.duration("us"), [dt.timedelta(seconds=1), dt.timedelta(days=2, microseconds=5), dt.timedelta(microseconds=-1), None]),
    "dur_s": (pa.duration("s"), [dt.timedelta(days=1), dt.timedelta(seconds=-90000), dt.timedelta(0), None]),
    "dec": (pa.decimal128(10, 2), [decimal.Decimal("1.23"), decimal.Decimal("-0.50"), None, decimal.Decimal("12345678.00")]),
    "list_i": (pa.list_(pa.int32()), [[1, 2], [], None, [None, 3]]),
    "list_f": (pa.list_(pa.float64()), [[1.0, 2.5], [float("nan")], None, []]),
    "struct": (pa.struct([("x", pa.int64()), ("y", pa.string()), ("when", pa.date32())]),
               [{"x": 1, "y": "a", "when": dt.date(2020, 2, 29)}, None, {"x": None, "y": None, "when": None}, {"x": -1, "y": "b", "when": None}]),
    "list_struct": (pa.list_(pa.struct([("k", pa.string()), ("v", pa.float64())])), [[{"k": "a", "v": 1.0}], [], None, [{"k": None, "v": None}]]),
    "fixed": (pa.list_(pa.int16(), 2), [[1, 2], [3, 4], [-1, 0], [5, 6]]),
}


def append_hash_suffix(string, json_path=None):
    """libcommon.viewer_utils.features.append_hash_suffix."""
    return f"{string}-{hex(zlib.adler32(json.dumps(json_path).encode()))[2:]}" if json_path else string


def media(out: Path, media_dir: Path) -> None:
    png = (media_dir / "i.png").read_bytes()
    gif = (media_dir / "i.gif").read_bytes()
    wav = (media_dir / "a.wav").read_bytes()
    img = pa.struct([("bytes", pa.binary()), ("path", pa.string())])
    table = pa.table(
        {
            "pics": pa.array([[{"bytes": png, "path": "a.png"}, {"bytes": gif, "path": "b.gif"}], []], type=pa.list_(img)),
            "meta": pa.array([{"img": {"bytes": png, "path": "x.png"}, "n": 1}, {"img": None, "n": 2}],
                             type=pa.struct([("img", img), ("n", pa.int64())])),
            "sound": pa.array([{"bytes": wav, "path": "a.wav"}, None], type=img),
        }
    )
    features = {
        "pics": {"feature": {"_type": "Image"}, "_type": "List"},
        "meta": {"img": {"_type": "Image"}, "n": {"dtype": "int64", "_type": "Value"}},
        "sound": {"_type": "Audio"},
    }
    table = table.replace_schema_metadata({"huggingface": json.dumps({"info": {"features": features}})})
    pq.write_table(table, out / "media.parquet")
    paths = [[0], [1], ["img"], ["a", 0, "ü", "<&>"], ["quote\"s", "back\\slash", "\n", "\u0001", "😀"]]
    names = {json.dumps(p): append_hash_suffix("image", p) for p in paths}
    (out / "media.json").write_text(json.dumps({"names": names}, indent=1) + "\n")


def main() -> int:
    out = Path(sys.argv[1])
    media(out, Path(sys.argv[2]))
    out.mkdir(parents=True, exist_ok=True)
    table = pa.table({k: pa.array(v, type=t) for k, (t, v) in cols.items()})
    pq.write_table(table, out / "cells.parquet")
    rows = [json.loads(dumps(r)) for r in table.to_pylist()]
    raw = [dumps(r).decode() for r in table.to_pylist()]
    (out / "cells.json").write_text(json.dumps({"rows": raw}, indent=1, ensure_ascii=False) + "\n")
    print(json.dumps(rows[1], ensure_ascii=False)[:2000])
    return 0


if __name__ == "__main__":
    sys.exit(main())
