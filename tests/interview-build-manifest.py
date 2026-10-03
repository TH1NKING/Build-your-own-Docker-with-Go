#!/usr/bin/env python3
"""Bind prepared interview binaries to the local source, without network access."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parent.parent
PRODUCTS = (
    "sandboxd", "sandbox-init", "sandbox-waiting-init", "sandbox-root-probe",
    "profile-bundle", "worker", "agentctl", "control-plane", "runtime-lab",
    "t05-tests", "sandbox-creation-tests", "python.bundle",
)


def digest(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"Expected regular file: {path.name}")
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def sources(root):
    paths = [root / name for name in ("go.mod", "go.sum", "Makefile")]
    for directory in ("cmd", "internal", "tests", "profiles", "with_Go", "with_shell"):
        paths.extend(p for p in (root / directory).rglob("*")
                     if p.is_file() and p.suffix in (".go", ".sh", ".py", ".json", ".sql"))
    return {p.relative_to(root).as_posix(): digest(p) for p in sorted(paths)}


def snapshot(root, build, expected_sources=None):
    current_sources = sources(root)
    if expected_sources is not None and current_sources != expected_sources:
        raise ValueError("Source changed during preparation; run prepare again")
    return {"version": 1, "sources": current_sources,
            "products": {name: digest(build / name) for name in PRODUCTS}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("write", "check"))
    parser.add_argument("build", type=Path)
    parser.add_argument("--expected-sources", type=Path,
                        help="Source snapshot captured before compilation (write only)")
    args = parser.parse_args()
    if args.expected_sources is not None and args.operation != "write":
        parser.error("--expected-sources is only valid when writing a manifest")
    build = args.build.resolve()
    manifest = build / "manifest.json"
    try:
        expected_sources = None
        if args.expected_sources is not None:
            expected_sources = json.loads(args.expected_sources.read_text(encoding="utf-8"))
        current = snapshot(ROOT, build, expected_sources)
        if args.operation == "write":
            # Publication is the final step; failed builds never obtain a stamp.
            temporary = build / "manifest.json.part"
            temporary.write_text(json.dumps(current, indent=2, sort_keys=True) + "\n", encoding="utf-8")
            os.replace(temporary, manifest)
        elif json.loads(manifest.read_text(encoding="utf-8")) != current:
            raise ValueError("Source or prepared artifact changed; run prepare again")
    except (OSError, ValueError) as error:
        print(f"Interview build check failed: {error}", file=sys.stderr)
        return 1
    print("INTERVIEW_BUILD_VERIFIED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
