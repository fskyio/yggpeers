#!/usr/bin/env python3
"""Rebuild the bundled country tiles. Requires Python 3 and Tippecanoe 2.79.0."""

import argparse
import hashlib
import json
import math
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import urllib.request


ROOT = Path(__file__).resolve().parents[1]
STATIC = ROOT / "internal/server/static"
SOURCE = (
    "https://raw.githubusercontent.com/nvkelso/natural-earth-vector/"
    "f1890d9f152c896d250a77557a5751a93d494776/"
    "geojson/ne_10m_admin_0_countries.geojson"
)
SOURCE_SHA256 = "239eec57ac17f100a11e2536cffc56752c318b50ae765b0918ff7aab4ce8f255"
MAX_ZOOM = 7


def polygons(feature):
    geometry = feature["geometry"]
    return [geometry["coordinates"]] if geometry["type"] == "Polygon" else geometry["coordinates"]


def main_extent(feature):
    """Projected extent of the largest land component, ignoring distant islands."""
    extents = []
    for polygon in polygons(feature):
        points = []
        for lon, lat in polygon[0]:
            sin_lat = math.sin(math.radians(max(-85, min(85, lat))))
            points.append(((lon + 180) / 360, 0.5 - math.log((1 + sin_lat) / (1 - sin_lat)) / (4 * math.pi)))
        width = max(p[0] for p in points) - min(p[0] for p in points)
        height = max(p[1] for p in points) - min(p[1] for p in points)
        extents.append((width * height, max(width, height)))
    return max(extents)


def prepare(data):
    # Include the app's canonical country names alongside Natural Earth's names.
    app_names = dict(re.findall(r'"([A-Z]{2})":\s*"([^"]+)"', (ROOT / "internal/fetcher/fetcher.go").read_text()))
    groups = {}
    for feature in data["features"]:
        props = feature["properties"]
        code = props["ISO_A2_EH"]
        if code == "-99":
            code = "NE:" + props["ADM0_A3"]
        groups.setdefault(code, []).append(feature)

    countries = {}
    features = []
    for code, members in sorted(groups.items()):
        primary = max(members, key=main_extent)
        props = primary["properties"]
        name = app_names.get(code, props["NAME_LONG"])
        aliases = {name, code}
        for member in members:
            aliases.update(member["properties"].get(field) for field in
                           ("ADMIN", "NAME", "NAME_LONG", "NAME_EN", "FORMAL_EN"))
        aliases.discard(None)
        aliases.discard("")
        countries[code] = {"name": name, "aliases": sorted(aliases)}

        # Use a marker until the largest land component spans about 14 pixels.
        extent = main_extent(primary)[1]
        marker_zoom = min(11, math.ceil(math.log2(14 / (512 * extent))))
        if marker_zoom >= 5:
            countries[code]["marker"] = [props["LABEL_X"], props["LABEL_Y"]]
            countries[code]["markerZoom"] = marker_zoom

        features.append({
            "type": "Feature",
            "properties": {"code": code, "name": name},
            "geometry": {"type": "MultiPolygon", "coordinates": [p for member in members for p in polygons(member)]},
        })
    assert all(code in countries and "marker" in countries[code] for code in ("HK", "SG"))
    return {"type": "FeatureCollection", "features": features}, countries


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, help="Use a previously downloaded, checksum-verified source")
    parser.add_argument("--tippecanoe", default="tippecanoe")
    args = parser.parse_args()
    executable = shutil.which(args.tippecanoe)
    if not executable:
        raise SystemExit("Tippecanoe 2.79.0 is required to rebuild map assets")
    executable = str(Path(executable).resolve())
    version = subprocess.check_output([executable, "--version"], stderr=subprocess.STDOUT, text=True)
    if "v2.79.0" not in version:
        raise SystemExit("Use Tippecanoe 2.79.0 for reproducible map assets")
    raw = args.source.read_bytes() if args.source else urllib.request.urlopen(SOURCE, timeout=60).read()
    if hashlib.sha256(raw).hexdigest() != SOURCE_SHA256:
        raise SystemExit("Natural Earth source checksum mismatch")
    geometry, countries = prepare(json.loads(raw))
    with tempfile.TemporaryDirectory(prefix="yggpeers-map-") as directory:
        source = Path(directory) / "countries.geojson"
        output = Path(directory) / "countries.pmtiles"
        source.write_text(json.dumps(geometry, separators=(",", ":")) + "\n")
        subprocess.run([
            "tippecanoe", "--quiet", "--output", output.name, "--layer", "countries",
            "--minimum-zoom", "0", "--maximum-zoom", str(MAX_ZOOM),
            "--no-simplification-of-shared-nodes", "--no-tiny-polygon-reduction",
            "--simplify-only-low-zooms", "--name", "Country boundaries",
            "--attribution", '<a href="https://www.naturalearthdata.com/">Natural Earth</a>',
            source.name,
        ], executable=executable, cwd=directory, check=True)
        (STATIC / "countries.pmtiles").write_bytes(output.read_bytes())
    (STATIC / "countries.json").write_text(json.dumps(countries, ensure_ascii=False, separators=(",", ":")) + "\n")
    print(f"Built {len(countries)} country/territory features, zooms 0–{MAX_ZOOM}")
    for name in ("countries.pmtiles", "countries.json"):
        print(f"{name}: {(STATIC / name).stat().st_size:,} bytes")


if __name__ == "__main__":
    main()
