"""Record how the `datasets` library reads card features for internal/datasets/card.

Usage: python compat/viewer/oracle/card_features.py OUT_JSON

Each case is the YAML of a dataset_info `features:` value. The script
converts it with Features._from_yaml_list, as DatasetInfo.from_dict does
for a dataset card, and writes the Features as a dict, or the error. Run
it with datasets 5.1.0:

    uv run --no-project --with datasets==5.1.0 --with pyyaml python -I card_features.py OUT
"""

from __future__ import annotations

import json
import sys

import yaml
from datasets import Features

CASES = {
    "values": "- name: a\n  dtype: string\n- name: b\n  dtype: int64\n- name: c\n  dtype: float32\n- name: d\n  dtype: bool\n",
    "timestamps": "- name: t\n  dtype: timestamp[us, tz=UTC]\n- name: d\n  dtype: date32\n- name: dur\n  dtype: duration[ms]\n",
    "class_label": "- name: label\n  dtype:\n    class_label:\n      names:\n        '0': neg\n        '1': pos\n",
    "list_simple": "- name: tags\n  list: string\n",
    "list_dtype": "- name: tags\n  list:\n    dtype: int32\n",
    "large_list": "- name: big\n  large_list: string\n",
    "list_of_struct": "- name: items\n  list:\n  - name: x\n    dtype: int64\n  - name: y\n    dtype: string\n",
    "sequence_simple": "- name: s\n  sequence: string\n",
    "sequence_length": "- name: s\n  sequence: float32\n  length: 3\n",
    "sequence_of_struct": "- name: s\n  sequence:\n  - name: x\n    dtype: int64\n  - name: y\n    dtype: string\n",
    "struct": "- name: meta\n  struct:\n  - name: source\n    dtype: string\n  - name: score\n    dtype: float64\n",
    "nested_struct": "- name: m\n  struct:\n  - name: inner\n    struct:\n    - name: v\n      dtype: int8\n",
    "image_audio": "- name: image\n  dtype: image\n- name: audio\n  dtype: audio\n",
    "audio_options": "- name: audio\n  dtype:\n    audio:\n      sampling_rate: 16000\n",
    "array2d": "- name: m\n  dtype:\n    array2_d:\n      shape:\n      - 2\n      - 3\n      dtype: float32\n",
    "translation": "- name: t\n  dtype:\n    translation:\n      languages:\n      - en\n      - fr\n",
    "list_of_lists": "- name: l\n  list:\n    list: int64\n",
    "list_class_label": "- name: labels\n  list:\n    class_label:\n      names:\n        '0': a\n        '1': b\n",
    "class_label_missing_id": "- name: label\n  dtype:\n    class_label:\n      names:\n        '0': a\n        '2': c\n",
}


def main() -> int:
    out = []
    for name, text in CASES.items():
        case = {"name": name, "yaml": text}
        try:
            case["features"] = Features._from_yaml_list(yaml.safe_load(text)).to_dict()
        except Exception as e:  # noqa: BLE001 - the error is the answer
            case["error"] = type(e).__name__
        out.append(case)
    with open(sys.argv[1], "w") as f:
        json.dump(out, f, indent=1)
        f.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
