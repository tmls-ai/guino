#!/usr/bin/env python3
"""Require a Guino source tree and matching package versions before publishing."""
import argparse
import json
from pathlib import Path
import re
import tomllib


def check(root: Path, tag: str) -> None:
    if not re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", tag):
        raise ValueError("release tag must be a stable version such as v0.1.0")
    version = tag[1:]
    if tuple(map(int, version.split("."))) < (0, 1, 0):
        raise ValueError("Guino releases require version 0.1.0 or newer")
    if (root / "go.mod").read_text().splitlines()[0] != "module github.com/tmls-ai/guino":
        raise ValueError("tag must contain the Guino Go module")
    if not (root / "cmd/guino/main.go").is_file():
        raise ValueError("tag must contain the Guino CLI")
    ts = json.loads((root / "sdk/typescript/package.json").read_text())
    py = tomllib.loads((root / "sdk/python/pyproject.toml").read_text())["project"]
    for package, expected in ((ts, "@tmls-ai/guino"), (py, "guino")):
        if package["name"] != expected or package["version"] != version:
            raise ValueError(f"{expected} identity/version must match {tag}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    args = parser.parse_args()
    try:
        check(Path(__file__).resolve().parents[1], args.tag)
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"Release refused: {error}\n")
    print(f"Guino package identities match {args.tag}")
