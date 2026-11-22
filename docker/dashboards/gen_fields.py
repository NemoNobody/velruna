#!/usr/bin/env python3
"""Bake index-pattern field lists into internal/dashboards/objects.ndjson.

OSD 3.x has no externally callable field-introspection API, so the bundle
carries the fields up front: a pattern with fields:[] cannot resolve its
timeFieldName and every visualization dies with "Could not locate that
index-pattern-field". Re-run this after a mapping change, rebuild the binary
(the bundle is embedded) and push again.

Usage (from the repo root):
    python3 docker/dashboards/gen_fields.py [path-to-velruna.conf]

The cluster URL/user come from OPENSEARCH_URL/OPENSEARCH_USER, else the config
file (default ./velruna.conf); the password comes from OPENSEARCH_PASSWORD.
Never prints the password.
"""
import base64
import json
import os
import ssl
import sys
import urllib.request

TYPE_MAP = {
    # "type" must be the OSD field type (OSD_FIELD_TYPES enum), not the raw
    # es type: terms.buckets filterFieldTypes match it exactly, and a saved
    # field of type "keyword" fails with "is invalid for use with the Terms
    # aggregation" — esType keyword normalizes to OSD "string".
    "text": ("string", False, False),
    "keyword": ("string", True, True),
    "date": ("date", True, True),
    "boolean": ("boolean", True, True),
    "long": ("number", True, True),
    "integer": ("number", True, True),
    "short": ("number", True, True),
    "byte": ("number", True, True),
}


META_FIELDS = [
    # OSD exposes _index as a meta field; without it in the saved field list
    # aggregations on it fail with "Could not locate that index-pattern-field"
    {
        "name": "_index",
        "type": "string",
        "esTypes": ["_index"],
        "count": 0,
        "scripted": False,
        "searchable": True,
        "aggregatable": True,
        "readFromDocValues": False,
    },
]


def flatten(props, prefix=""):
    fields = []
    for name, spec in props.items():
        path = f"{prefix}{name}"
        t = spec.get("type")
        if t is None:
            if "properties" in spec:  # object: walk in
                fields.extend(flatten(spec["properties"], path + "."))
            continue
        mapped = TYPE_MAP.get(t)
        if mapped is None:
            continue
        osd_type, aggs, docs = mapped
        fields.append({
            "name": path,
            "type": osd_type,
            "esTypes": [t],
            "count": 0,
            "scripted": False,
            "searchable": True,
            "aggregatable": aggs,
            "readFromDocValues": docs,
        })
        if "fields" in spec:  # multi-fields (e.g. text + .keyword subfield)
            for sub, sspec in spec["fields"].items():
                sm = TYPE_MAP.get(sspec.get("type") or "")
                if sm is None:
                    continue
                st, sa, sd = sm
                fields.append({
                    "name": f"{path}.{sub}",
                    "type": st,
                    "esTypes": [sspec["type"]],
                    "count": 0,
                    "scripted": False,
                    "searchable": True,
                    "aggregatable": sa,
                    "readFromDocValues": sd,
                })
    return fields


def main():
    conf_path = sys.argv[1] if len(sys.argv) > 1 else "velruna.conf"
    # A container/CI run may supply everything through OPENSEARCH_* variables;
    # a missing config file is then normal, not a crash.
    conf = {}
    if os.path.exists(conf_path):
        with open(conf_path, encoding="utf-8") as f:
            conf = json.load(f)
    url = os.environ.get("OPENSEARCH_URL") or os.environ.get("VELRUNA_OS_URL") or conf.get("opensearch_url")
    user = (os.environ.get("OPENSEARCH_USER") or os.environ.get("VELRUNA_OS_USER")
            or conf.get("opensearch_user", "admin"))
    pw = os.environ.get("OPENSEARCH_PASSWORD")
    if not url or not pw:
        sys.exit("cluster url/password unresolved (config + OPENSEARCH_PASSWORD)")

    ctx = ssl._create_unverified_context()

    def get(path):
        req = urllib.request.Request(f"{url.rstrip('/')}/{path}")
        req.add_header("Authorization", "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode())
        with urllib.request.urlopen(req, context=ctx, timeout=15) as r:
            return json.load(r)

    by_title = {
        "velruna": "velruna",
        "velruna-history-*": "velruna-history-*",
        "velruna-docs": "velruna-docs",
    }
    ndjson_path = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                              "..", "..", "internal", "dashboards", "objects.ndjson"))
    objs = []
    with open(ndjson_path, encoding="utf-8") as f:
        for line in f:
            if line.strip():
                objs.append(json.loads(line))

    for o in objs:
        if o["type"] != "index-pattern":
            continue
        title = o["attributes"]["title"]
        idx = by_title.get(title)
        if idx is None:
            continue
        try:
            mapping = get(f"{idx}/_mapping?ignore_unavailable=true&allow_no_indices=true")
        except Exception as e:  # noqa: BLE001 - report and keep the old list
            print(f"{title}: mapping unavailable ({e}); fields unchanged")
            continue
        merged = {}
        for entry in mapping.values():
            props = entry.get("mappings", {}).get("properties", {})
            for name, spec in props.items():
                merged.setdefault(name, spec)
        fields = sorted(flatten(merged) + [dict(m) for m in META_FIELDS], key=lambda f: f["name"])
        o["attributes"]["fields"] = json.dumps(fields)
        print(f"{title}: {len(fields)} field(s) baked")

    with open(ndjson_path, "w", encoding="utf-8") as f:
        f.write("\n".join(json.dumps(o, ensure_ascii=False) for o in objs) + "\n")
    print("objects.ndjson updated")


if __name__ == "__main__":
    main()
