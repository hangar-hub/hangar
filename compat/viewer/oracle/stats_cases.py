"""Record the Hub's column statistics for internal/datasets/stats.

Usage: python compat/viewer/oracle/stats_cases.py DATASET_VIEWER_SRC OUT_JSON

DATASET_VIEWER_SRC is a checkout of github.com/huggingface/dataset-viewer.
The script loads libs/libcommon/src/libcommon/statistics_utils.py from it,
with the few helpers it imports from libcommon.utils taken from the same
checkout, runs every column class on the cases below and writes the
statistics, or the error, of each case. Run it with the versions of the
Hub's worker (polars 1.39.3, numpy 2.4.6, python-dateutil 2.9.0.post0);
the Go tests read the output offline.
"""

from __future__ import annotations

import ast
import datetime
import importlib.util
import json
import math
import sys
import types
from pathlib import Path

import numpy as np
import polars as pl

src = Path(sys.argv[1]) / "libs/libcommon/src/libcommon"

exceptions = types.ModuleType("libcommon.exceptions")


class StatisticsComputationError(Exception):
    pass


exceptions.StatisticsComputationError = StatisticsComputationError
utils = types.ModuleType("libcommon.utils")
tree = ast.parse((src / "utils.py").read_text())
wanted = {"is_datetime", "get_timezone", "datetime_to_string", "identify_datetime_format"}
code = "from datetime import datetime, timedelta, timezone\nfrom typing import Any, Optional\nfrom dateutil import parser\n"
code += "\n".join(ast.unparse(n) for n in tree.body if isinstance(n, ast.FunctionDef) and n.name in wanted)
exec(code, utils.__dict__)  # noqa: S102 - trusted source checkout
libcommon = types.ModuleType("libcommon")
sys.modules.update({"libcommon": libcommon, "libcommon.exceptions": exceptions, "libcommon.utils": utils})
spec = importlib.util.spec_from_file_location("libcommon.statistics_utils", src / "statistics_utils.py")
su = importlib.util.module_from_spec(spec)
spec.loader.exec_module(su)

rng = np.random.default_rng(7)
cases = []


def add(name, kind, values, dtype=None, **extra):
    cases.append({"name": name, "kind": kind, "values": values, "dtype": dtype, **extra})


add("ints", "int", [int(x) for x in rng.integers(-50, 500, 997)])
add("ints_nulls", "int", [None if i % 9 == 0 else int(x) for i, x in enumerate(rng.integers(0, 10, 300))])
add("ints_one_value", "int", [7, 7, 7])
add("ints_two_values", "int", [2, 3, 3, 2])
add("ints_all_null", "int", [None, None])
add("ints_large", "int", [10**12 + i * 37 for i in range(50)])
add("floats", "float", [float(x) for x in rng.normal(3.5, 2.25, 1000)])
add("floats_nan_null", "float", [None, float("nan"), 0.5, 1.25, -3.0, None, 2.0])
add("floats_tiny", "float", [1e-9 * i for i in range(20)])
add("floats_one", "float", [0.1, 0.1])
add("strings_label", "string", [str(x) for x in rng.choice(["a", "b", "c", None], 200)])
add("strings_text", "string", ["x" * int(n) for n in rng.integers(1, 300, 500)] + [None])
add("strings_unique_small", "string", ["u%d" % i for i in range(10)])
add("strings_unicode", "string", ["héllo wörld " * (i % 5 + 1) for i in range(40)])
add("dates_iso", "string", ["2020-01-%02d" % (i % 28 + 1) for i in range(100)] + ["2021-06-15"])
add("dates_times", "string", ["2021-03-04 %02d:%02d:%02d" % (i % 24, i % 60, (i * 7) % 60) for i in range(60)])
add("dates_us", "string", ["%d/%d/2019" % (i % 12 + 1, i % 28 + 1) for i in range(30)])
add("dates_tz", "string", ["2022-01-01T10:00:00+0200", "2022-01-02T11:30:00+0200", "2022-01-05T00:00:00+0200"])
add("dates_mixed", "string", ["2020-01-01", "2020/01/02", "2020-01-03"])
add("dates_words", "string", ["October 12 2016", "August 23 2016"])
add("years", "string", ["1999", "2000", "2010", "2024"])
add("bools", "bool", [True, False, None, True, True])
add("bools_true", "bool", [True, True])
add("labels", "class_label", [0, 1, 1, -1, None, 2, 0], names=["neg", "neu", "pos", "unused"])
add("lists", "list", [[1, 2], [], None, [3], [4, 5, 6, 7]])
add("lists_all_null", "list", [None, None])
add("timestamps", "datetime", ["2020-01-01T00:00:00", "2020-01-01T12:30:00", None, "2020-03-01T00:00:00", "2020-02-29T23:59:59"])
add("widths", "image", [300, 500, None, 327])
add("durations", "audio", [0.19041, 0.84867, 0.56741, 0.30778])

out = []
for c in cases:
    name, kind, values = c["name"], c["kind"], c["values"]
    try:
        if kind == "int":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.Int64)})
            item = su.IntColumn(name).compute_and_prepare_response(df)
        elif kind == "float":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.Float64)})
            item = su.FloatColumn(name).compute_and_prepare_response(df)
        elif kind == "string":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.String)})
            item = su.StringColumn(name).compute_and_prepare_response(df)
        elif kind == "bool":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.Boolean)})
            item = su.BoolColumn(name).compute_and_prepare_response(df)
        elif kind == "class_label":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.Int64)})
            item = su.ClassLabelColumn(name, feature_dict={"names": c["names"], "_type": "ClassLabel"}).compute_and_prepare_response(df)
        elif kind == "list":
            df = pl.DataFrame({name: pl.Series(values, dtype=pl.List(pl.Int64))})
            item = su.ListColumn(name).compute_and_prepare_response(df)
        elif kind == "datetime":
            vals = [None if v is None else datetime.datetime.fromisoformat(v) for v in values]
            df = pl.DataFrame({name: pl.Series(vals, dtype=pl.Datetime("us"))})
            item = su.DatetimeColumn(name).compute_and_prepare_response(df)
        elif kind in ("image", "audio"):
            col = su.ImageColumn if kind == "image" else su.AudioColumn
            transformed = pl.from_dict({name: values})
            inner = su.IntColumn(name) if kind == "image" else su.FloatColumn(name)
            nan_count = sum(v is None for v in values)
            st = inner.compute_statistics(data=transformed)
            st["nan_count"] = nan_count
            st["nan_proportion"] = np.round(nan_count / len(values), 5).item() if nan_count else 0.0
            item = {"column_name": name, "column_type": col.get_column_type().value, "column_statistics": st}
        result = {"item": json.loads(json.dumps(item, default=lambda o: o.value if hasattr(o, "value") else str(o)))}
    except Exception as e:  # the Hub answers with an error for these
        result = {"error": str(e)[:200]}
    out.append({**c, **result})
Path(sys.argv[2]).write_text(json.dumps(out, indent=1) + "\n")
print("wrote", len(out), "cases")
