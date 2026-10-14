"""Build the source fixtures of internal/datasets/convert and record what
the `datasets` library makes of them.

Usage: python compat/viewer/oracle/convert_cases.py OUT_DIR

For each case this writes OUT_DIR/<case>/files/... (the source files),
and OUT_DIR/<case>/expected.json with the builder, its options, the
Features `datasets` infers, and per Split the number of examples, the
num_bytes it reports and every row. Media values are written as their file
name and size, and rows come from the embedded table, as the Hub's Parquet
export stores them. Run it with the library versions of the Hub's Dataset Viewer worker
(datasets 5.1.0, pandas 2.3.3, pyarrow 24.0.0) plus Pillow; the Go tests
read the output offline.
"""

from __future__ import annotations

import base64
import bz2
import datetime
import decimal
import gzip
import io
import json
import os
import shutil
import struct
import sys
import tarfile
from pathlib import Path

import datasets
import pyarrow as pa
from datasets.features import audio as _audio
from datasets.table import embed_table_storage

# Audio.encode_example imports torch even for file paths, which the
# AudioFolder builder passes; encode paths without it.
_encode_audio = _audio.Audio.encode_example


def _encode_audio_path(self, value):
    if isinstance(value, str):
        return {"bytes": None, "path": value}
    if isinstance(value, dict) and value.get("path") is not None and value.get("bytes") is None:
        return {"bytes": None, "path": value["path"]}
    return _encode_audio(self, value)


_audio.Audio.encode_example = _encode_audio_path

OUT = Path(sys.argv[1])


def png(w: int, h: int, seed: int) -> bytes:
    import zlib

    raw = b"".join(b"\x00" + bytes([(x * 7 + y * 3 + seed) % 256 for x in range(w * 3)]) for y in range(h))

    def chunk(t: bytes, d: bytes) -> bytes:
        return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)

    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


def wav(n: int, rate: int = 8000) -> bytes:
    data = bytes((i * 13) % 256 for i in range(n))
    return b"RIFF" + struct.pack("<I", 36 + len(data)) + b"WAVEfmt " + struct.pack("<IHHIIHH", 16, 1, 1, rate, rate, 1, 8) + b"data" + struct.pack("<I", len(data)) + data


def enc(v):
    if isinstance(v, dict) and set(v) == {"bytes", "path"}:
        return {"media": v["path"], "size": None if v["bytes"] is None else len(v["bytes"])}
    if isinstance(v, bytes):
        return {"b64": base64.b64encode(v).decode()}
    if isinstance(v, datetime.datetime):
        return {"ts": v.isoformat()}
    if isinstance(v, datetime.date):
        return {"date": v.isoformat()}
    if isinstance(v, decimal.Decimal):
        return {"dec": str(v)}
    if isinstance(v, float):
        if v != v:
            return {"float": "nan"}
        if v in (float("inf"), float("-inf")):
            return {"float": str(v)}
        return v
    if isinstance(v, dict):
        return {k: enc(x) for k, x in v.items()}
    if isinstance(v, list):
        return [enc(x) for x in v]
    return v


def run(name: str, builder: str, files: dict[str, bytes], data_files: dict[str, list[str]], **kwargs):
    root = OUT / name
    if root.exists():
        shutil.rmtree(root)
    fdir = root / "files"
    for p, b in files.items():
        (fdir / p).parent.mkdir(parents=True, exist_ok=True)
        (fdir / p).write_bytes(b)
    dd = datasets.load_dataset(
        builder,
        data_files={s: [str(fdir / p) for p in ps] for s, ps in data_files.items()},
        cache_dir=str(root / ".cache"),
        **kwargs,
    )
    out = {"builder": builder, "options": kwargs, "data_files": data_files, "splits": {}}
    for split, ds in dd.items():
        out["features"] = ds.features.to_dict()
        table = embed_table_storage(ds.data.table)
        rows = [enc(r) for r in table.to_pylist()]
        num_bytes, embedded = ds.info.splits[split].num_bytes, table.nbytes
        # WebDataset rows name the shard by its local path; record a
        # machine-independent name and count its bytes instead.
        for r in rows:
            if "__url__" in r:
                shard = Path(r["__url__"]).name
                num_bytes += len(shard) - len(r["__url__"])
                embedded += len(shard) - len(r["__url__"])
                r["__url__"] = shard
        out["splits"][split] = {
            "num_examples": ds.info.splits[split].num_examples,
            "num_bytes": num_bytes,
            "embedded_nbytes": embedded,
            "rows": rows,
        }
    shutil.rmtree(root / ".cache")
    (root / "expected.json").write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
    print("wrote", name)


csv1 = "id,name,score,flag,mixed,empty,when\n1,a,1.5,True,x,,2020-01-01\n2,b,,False,1,,2020-01-02\n3,,2.25,True,2.5,,\n"
run("csv_types", "csv", {"train.csv": csv1.encode(), "test.csv": b"id,name,score,flag,mixed,empty,when\n4,d,3.0,False,y,,2021-01-01\n"},
    {"train": ["train.csv"], "test": ["test.csv"]})
run("csv_ints_with_na", "csv", {"a.csv": b"a,b\n1,\n2,3\n,4\n"}, {"train": ["a.csv"]})
run("tsv", "csv", {"a.tsv": "x\ty\n\"q\tq\",1\n\xc3\xa9,2\n".encode()}, {"train": ["a.tsv"]}, sep="\t")
run("csv_semicolon_names", "csv", {"a.csv": b"1;2\n3;4\n"}, {"train": ["a.csv"]}, sep=";", names=["p", "q"])
run("csv_quotes_dupes", "csv", {"a.csv": b'x,x,\n"a ""quoted"" value","multi\nline",9\n'}, {"train": ["a.csv"]})
run("csv_gzip", "csv", {"a.csv.gz": gzip.compress(b"k,v\nalpha,1\nbeta,2\n")}, {"train": ["a.csv.gz"]})

jl = "\n".join(json.dumps(x) for x in [
    {"a": 1, "b": "2020-01-01", "c": "2020-01-01 10:00:00", "d": [1, 2], "e": {"x": 1}, "f": 1, "g": None, "h": True, "i": [{"k": 1}], "u": "é/ü"},
    {"a": 2.5, "b": "2021-02-03", "c": "x", "d": [], "e": {"y": "s"}, "f": "str", "g": None, "h": False, "i": [{"k": 2}, {"k": None}], "u": "x"},
    {"a": None, "b": None, "c": None, "d": None, "e": None, "f": [1, {"z": 2}], "g": None, "h": None, "i": None, "u": None},
]) + "\n"
run("jsonl_mixed", "json", {"train.jsonl": jl.encode()}, {"train": ["train.jsonl"]})
run("json_array", "json", {"a.json": json.dumps([{"q": "x", "n": 1}, {"q": "y", "n": 2, "extra": [1.5]}]).encode()}, {"train": ["a.json"]})
run("json_strings", "json", {"a.json": json.dumps(["one", "two"]).encode()}, {"train": ["a.json"]})
run("json_field", "json", {"a.json": json.dumps({"version": 1, "data": [{"t": "a", "v": [1, 2]}, {"t": "b", "v": [3]}]}).encode()}, {"train": ["a.json"]}, field="data")
run("json_nested", "json", {"a.jsonl": (json.dumps({"s": {"a": {"b": [1, 2]}, "c": "x"}, "ll": [[1], [2, 3]], "big": 12345678901234567890}) + "\n" + json.dumps({"s": {"a": {"b": []}, "c": None}, "ll": [], "big": 1}) + "\n").encode()}, {"train": ["a.jsonl"]})

text = "foo\r\nbar\n\nbaz\rqux\nlast"
run("text_lines", "text", {"a.txt": text.encode(), "b.txt": b"x\n"}, {"train": ["a.txt", "b.txt"]})
run("text_keep", "text", {"a.txt": b"a\nb\n"}, {"train": ["a.txt"]}, keep_linebreaks=True)
run("text_paragraph", "text", {"a.txt": b"p1 l1\np1 l2\n\n\np2\n\np3"}, {"train": ["a.txt"]}, sample_by="paragraph")
run("text_bz2", "text", {"a.txt.bz2": bz2.compress(b"one\ntwo\n")}, {"train": ["a.txt.bz2"]})

tbl = pa.table({"x": pa.array([1, 2, None], pa.int32()), "s": ["a", None, "c"], "l": pa.array([[1.0], [], None], pa.list_(pa.float32())),
                "t": pa.array([datetime.datetime(2020, 1, 1), None, datetime.datetime(2020, 1, 3)], pa.timestamp("ms"))})
sink = io.BytesIO()
with pa.ipc.new_stream(sink, tbl.schema) as w:
    w.write_table(tbl)
run("arrow_stream", "arrow", {"data-00000-of-00001.arrow": sink.getvalue()}, {"train": ["data-00000-of-00001.arrow"]})

imgs = {"train/cat/a.png": png(3, 2, 1), "train/cat/b.png": png(4, 4, 2), "train/dog/c.png": png(5, 1, 3), "test/dog/d.png": png(2, 2, 4)}
run("imagefolder_labels", "imagefolder", imgs, {"train": ["train/cat/a.png", "train/cat/b.png", "train/dog/c.png"], "test": ["test/dog/d.png"]})
run("imagefolder_csv", "imagefolder",
    {"train/0.png": png(2, 3, 5), "train/sub/1.png": png(6, 2, 6), "train/metadata.csv": b"file_name,caption,score\n0.png,first,1\nsub/1.png,second,2\n"},
    {"train": ["train/0.png", "train/sub/1.png", "train/metadata.csv"]})
run("audiofolder_jsonl", "audiofolder",
    {"a.wav": wav(800), "b.wav": wav(1600), "metadata.jsonl": (json.dumps({"file_name": "a.wav", "speaker": "x", "dur": 0.1}) + "\n" + json.dumps({"file_name": "b.wav", "speaker": "y", "dur": 0.2}) + "\n").encode()},
    {"train": ["a.wav", "b.wav", "metadata.jsonl"]})


def tar(members: list[tuple[str, bytes]]) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as t:
        for name, data in members:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            t.addfile(info, io.BytesIO(data))
    return buf.getvalue()


shard = tar([("s/000.png", png(2, 2, 7)), ("s/000.json", b'{"label": 1, "tags": ["a"]}'), ("s/000.txt", b"hello"), ("s/000.cls", b"3"),
             ("s/001.png", png(3, 1, 8)), ("s/001.json", b'{"label": 0, "tags": []}'), ("s/001.txt", b"world"), ("s/001.cls", b"4"),
             ("s/002.png", png(1, 1, 9)), ("s/002.json", b'{"label": 2, "tags": ["b", "c"]}'), ("s/002.txt", b"!"), ("s/002.cls", b"5")])
run("webdataset", "webdataset", {"shard-0000.tar": shard}, {"train": ["shard-0000.tar"]})
