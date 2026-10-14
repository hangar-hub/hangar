"""Write small Parquet files as fuzz seeds for internal/datasets/pq.

Usage: python compat/viewer/oracle/parquet_seeds.py OUT_DIR

The fuzzer minimises every input that finds new coverage, and minimising
takes time that grows with the square of the input's size (up to a minute
each by default), so every seed is a few rows and columns, about a
kilobyte or less.
Between them they cover what pyarrow writes and pq.Open reads: dictionary
and plain pages, data pages v1 and v2, every codec parquet-go reads, the
delta, byte stream split and run-length encodings, several row groups, nulls, empty lists, nested lists and structs, maps, every
primitive type the Viewer encodes, legacy INT96 timestamps, and the
ARROW:schema metadata. Run it with the Hub worker's pyarrow:

    uv run --no-project --with pyarrow==24.0.0 \\
        python -I parquet_seeds.py OUT_DIR
"""

from __future__ import annotations

import datetime as dt
import decimal
import sys
from pathlib import Path

import pyarrow as pa
import pyarrow.parquet as pq


def nested() -> pa.Table:
    return pa.table(
        {
            "id": pa.array([0, 1, None, 3, 4, 5], pa.int64()),
            "lst": pa.array([None, [0], [], [0, 1, None], [7], [7, 7]], pa.list_(pa.int64())),
            "st": pa.array(
                [
                    {"a": 1, "b": ["x"], "c": {"x": 0.5}},
                    None,
                    {"a": None, "b": [], "c": None},
                    {"a": 3, "b": None, "c": {"x": None}},
                    {"a": 4, "b": ["x", None, "y"], "c": {"x": 1.5}},
                    {"a": 4, "b": ["y"], "c": {"x": 1.5}},
                ],
                pa.struct(
                    [
                        ("a", pa.int64()),
                        ("b", pa.list_(pa.string())),
                        ("c", pa.struct([("x", pa.float64())])),
                    ]
                ),
            ),
        }
    )


def primitives() -> pa.Table:
    when = dt.datetime(2024, 5, 6, 7, 8, 9, 123456, tzinfo=dt.timezone.utc)
    return pa.table(
        {
            "b": pa.array([True, None, False], pa.bool_()),
            "i8": pa.array([-1, None, 127], pa.int8()),
            "i16": pa.array([-300, 0, None], pa.int16()),
            "i32": pa.array([None, 1 << 20, -5], pa.int32()),
            "u8": pa.array([255, 0, None], pa.uint8()),
            "u32": pa.array([4_000_000_000, None, 1], pa.uint32()),
            "u64": pa.array([18_000_000_000_000_000_000, 0, None], pa.uint64()),
            "f16": pa.array([1.5, None, -2.0], pa.float16()),
            "f32": pa.array([0.25, float("nan"), None], pa.float32()),
            "f64": pa.array([None, float("inf"), -0.0], pa.float64()),
            "s": pa.array(["a", None, "é"], pa.string()),
            "ls": pa.array(["long", "", None], pa.large_string()),
            "bin": pa.array([b"\x00\x01", None, b""], pa.binary()),
            "fsb": pa.array([b"abc", None, b"xyz"], pa.binary(3)),
            "d32": pa.array([decimal.Decimal("1.250"), None, decimal.Decimal("-3.000")], pa.decimal128(9, 3)),
            "d64": pa.array([decimal.Decimal("12345678.25"), None, decimal.Decimal("-1.00")], pa.decimal128(18, 2)),
            "d128": pa.array([decimal.Decimal("1.0001"), None, decimal.Decimal("-12345678901234567890.0000")], pa.decimal128(30, 4)),
            "day": pa.array([dt.date(2024, 5, 6), None, dt.date(1960, 1, 1)], pa.date32()),
            "ts_ms": pa.array([when, None, when], pa.timestamp("ms")),
            "ts_us": pa.array([when, when, None], pa.timestamp("us", tz="UTC")),
            "ts_ns": pa.array([None, when, when], pa.timestamp("ns", tz="Europe/London")),
            "t32": pa.array([dt.time(7, 8, 9), None, dt.time(0, 0)], pa.time32("ms")),
            "t64": pa.array([dt.time(7, 8, 9, 5), None, dt.time(23, 59)], pa.time64("us")),
            "dur": pa.array([dt.timedelta(seconds=1), None, dt.timedelta(microseconds=-3)], pa.duration("us")),
            "m": pa.array([[("k", 1)], None, []], pa.map_(pa.string(), pa.int32())),
            "n": pa.array([None, None, None], pa.null()),
        }
    )


# Columns of primitives() for each seed file, a few per file to keep the
# files small.
GROUPS = {
    "ints": ["b", "i8", "i16", "i32"],
    "uints": ["u8", "u32", "u64"],
    "floats": ["f16", "f32", "f64"],
    "strings": ["s", "bin", "fsb"],
    "decimals": ["d32", "d64", "d128"],
    "dates": ["day", "ts_ms", "t32"],
    "times": ["t64", "dur", "n"],
    "map": ["m"],
}


def main() -> None:
    out = Path(sys.argv[1])
    out.mkdir(parents=True, exist_ok=True)
    for old in out.glob("*.parquet"):
        old.unlink()
    small = {"compression": "none", "store_schema": False}
    table = nested()
    pq.write_table(table.select(["id", "lst"]), out / "list-dict.parquet", row_group_size=2, **small)
    pq.write_table(table.select(["st"]), out / "struct-dict.parquet", row_group_size=3, **small)
    pq.write_table(
        table.select(["id", "lst"]),
        out / "list-v2-snappy.parquet",
        row_group_size=4,
        compression="snappy",
        use_dictionary=False,
        data_page_version="2.0",
        store_schema=False,
    )
    prims = primitives()
    for name, cols in GROUPS.items():
        pq.write_table(prims.select(cols), out / f"{name}.parquet", **small)
    for codec in ["gzip", "brotli", "zstd", "lz4"]:
        pq.write_table(
            table.select(["id", "lst"]),
            out / f"codec-{codec}.parquet",
            row_group_size=4,
            compression=codec,
            store_schema=False,
        )
    pq.write_table(
        prims.select(["b", "i32", "u64", "s", "bin", "f64"]),
        out / "encodings.parquet",
        use_dictionary=False,
        column_encoding={
            "b": "RLE",
            "i32": "DELTA_BINARY_PACKED",
            "u64": "DELTA_BINARY_PACKED",
            "s": "DELTA_BYTE_ARRAY",
            "bin": "DELTA_LENGTH_BYTE_ARRAY",
            "f64": "BYTE_STREAM_SPLIT",
        },
        data_page_version="2.0",
        **small,
    )
    # ARROW:schema gives the exact Arrow types of large strings, time zones
    # and durations.
    pq.write_table(prims.select(["ls", "ts_ns"]), out / "arrow-schema.parquet", compression="none")
    pq.write_table(
        prims.select(["ts_us"]),
        out / "int96.parquet",
        use_deprecated_int96_timestamps=True,
        **small,
    )


if __name__ == "__main__":
    main()
