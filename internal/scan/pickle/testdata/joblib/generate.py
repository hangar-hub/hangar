"""Writes the joblib fixtures in this folder: real joblib.dump output.

Generated with Python 3.11.7, joblib 1.5.3 and NumPy 2.2.6. Run it from
this folder with `python3 -I generate.py` to regenerate them.
"""

import joblib
import numpy as np

arrays = {
    "a": np.arange(10, dtype=np.float64),
    "b": np.zeros((3, 4), dtype=np.int32),
    "c": np.array(["x", "yy"]),
    "d": np.array([1, "x", None], dtype=object),
    "e": np.zeros(3, dtype=[("x", "<i4"), ("y", "<f8")]),
    "f": np.array([True, False]),
    "g": np.array(["2020-01-01", "2021-01-01"], dtype="M8[D]"),
    "h": np.float32(3.5),
    "s": "str",
}
for proto in (2, 3, 4, 5):
    joblib.dump(arrays, f"arrays_p{proto}.joblib", protocol=proto)
joblib.dump(arrays, "arrays_zlib.joblib", compress=3)
joblib.dump(np.zeros(0), "empty.joblib")
joblib.dump(np.zeros((2, 0, 3), dtype=">f4"), "empty_dims.joblib")
joblib.dump(np.asfortranarray(np.ones((3, 2))), "fortran.joblib")
joblib.dump(
    [np.array("abc", dtype="U5"), np.array(b"xy", dtype="S4"), np.zeros(2, dtype="V7"), np.zeros(2, dtype=np.float16)],
    "flexible.joblib",
)
