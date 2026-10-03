#!/usr/bin/env python3
"""Prepare the reviewed Profile source cache; --check is strictly offline.

Only Python's standard library is needed. Run preparation as your ordinary
account before the interview, then use --check before the offline demo.
"""

import argparse
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


REPOSITORY_ROOT = Path(__file__).resolve().parent.parent
MAX_INPUT_SIZE = 256 * 1024 * 1024
MAX_LOCK_SIZE = 1024 * 1024
CHUNK_SIZE = 64 * 1024
RESERVED_FILENAMES = {"CON", "PRN", "AUX", "NUL"} | {
    f"{prefix}{number}" for prefix in ("COM", "LPT") for number in range(1, 10)
}


class PreparationError(Exception):
    """A failed prerequisite, with an actionable message for the operator."""


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise PreparationError(f"duplicate JSON key in lock: {key}")
        result[key] = value
    return result


def validate_source_url(value):
    if not isinstance(value, str) or any(ord(char) < 33 for char in value):
        raise PreparationError("source must be an absolute HTTPS URL")
    try:
        parsed = urllib.parse.urlsplit(value)
        valid = (
            parsed.scheme == "https"
            and parsed.hostname
            and parsed.username is None
            and parsed.password is None
            and not parsed.fragment
        )
        parsed.port  # Reject malformed ports before making a request.
    except ValueError as error:
        raise PreparationError("source must be an absolute HTTPS URL") from error
    if not valid:
        raise PreparationError("source must be an absolute HTTPS URL without credentials or fragment")


def read_inputs(lock_path):
    with lock_path.open("rb") as stream:
        raw = stream.read(MAX_LOCK_SIZE + 1)
    if len(raw) > MAX_LOCK_SIZE:
        raise PreparationError("Profile lock exceeds the 1 MiB limit")
    lock = json.loads(raw, object_pairs_hook=unique_object)
    if not isinstance(lock, dict) or lock.get("schema") != "profile-build-lock/v1":
        raise PreparationError("unsupported Profile lock schema")
    if lock.get("profile") != "python-data-v1" or lock.get("target") != {"os": "linux", "arch": "amd64"}:
        raise PreparationError("Profile lock must select python-data-v1 for linux/amd64")
    inputs = lock.get("inputs")
    if not isinstance(inputs, list) or not inputs:
        raise PreparationError("Profile lock must declare external inputs")
    filenames = set()
    for entry in inputs:
        if not isinstance(entry, dict):
            raise PreparationError("each locked input must be an object")
        filename = entry.get("filename")
        if (
            not isinstance(filename, str)
            or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,254}", filename) is None
            or filename.endswith(".")
            or filename.split(".")[0].upper() in RESERVED_FILENAMES
        ):
            raise PreparationError(f"unsafe source filename: {filename!r}")
        if filename.casefold() in filenames:
            raise PreparationError(f"duplicate source filename: {filename}")
        filenames.add(filename.casefold())
        size = entry.get("size")
        if type(size) is not int or not 1 <= size <= MAX_INPUT_SIZE:
            raise PreparationError(f"{filename}: size must be between 1 and {MAX_INPUT_SIZE} bytes")
        digest = entry.get("digest")
        if not isinstance(digest, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
            raise PreparationError(f"{filename}: invalid locked SHA-256")
        validate_source_url(entry.get("source"))
    return inputs


def existing_regular_file(path):
    """Never follow or replace a cache symlink or special file."""
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode):
        raise PreparationError(f"{path}: expected a regular file; refusing to replace it")
    return info


def verify_cached(path, entry):
    info = existing_regular_file(path)
    if info is None:
        return "missing"
    if info.st_size != entry["size"]:
        return f"size mismatch (got {info.st_size}, want {entry['size']})"
    digest = hashlib.sha256()
    # Bound reads even if a cache file is concurrently extended.
    remaining = entry["size"] + 1
    with path.open("rb") as stream:
        while remaining:
            chunk = stream.read(min(CHUNK_SIZE, remaining))
            if not chunk:
                break
            digest.update(chunk)
            remaining -= len(chunk)
    if remaining != 1:
        return "size changed while reading"
    if "sha256:" + digest.hexdigest() != entry["digest"]:
        return "SHA-256 mismatch"
    return None


class HTTPSOnlyRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        validate_source_url(new_url)
        return super().redirect_request(request, response, code, message, headers, new_url)


def download_once(entry, cache, timeout):
    target = cache / entry["filename"]
    existing_regular_file(target)
    # A unique .part avoids overwriting interrupted downloads or another process.
    descriptor, temporary = tempfile.mkstemp(prefix=".profile-source-", suffix=".part", dir=cache)
    partial = Path(temporary)
    try:
        with os.fdopen(descriptor, "wb") as output:
            digest = hashlib.sha256()
            size = 0
            deadline = time.monotonic() + timeout
            opener = urllib.request.build_opener(HTTPSOnlyRedirectHandler())
            request = urllib.request.Request(entry["source"], headers={"User-Agent": "profile-source-preparation/1", "Accept-Encoding": "identity"})
            with opener.open(request, timeout=min(timeout, 30)) as response:
                validate_source_url(response.geturl())
                length = response.headers.get("Content-Length")
                if length is not None and (not length.isdecimal() or int(length) != entry["size"]):
                    raise PreparationError(f"{entry['filename']}: HTTP Content-Length does not match the lock")
                while True:
                    if time.monotonic() >= deadline:
                        raise PreparationError(f"{entry['filename']}: download exceeded {timeout:g} seconds")
                    # read1 returns after one underlying read, so slow streams do
                    # not keep one large read alive beyond the deadline checks.
                    chunk = response.read1(min(CHUNK_SIZE, entry["size"] + 1 - size))
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > entry["size"]:
                        raise PreparationError(f"{entry['filename']}: download exceeds locked size")
                    digest.update(chunk)
                    output.write(chunk)
            if size != entry["size"] or "sha256:" + digest.hexdigest() != entry["digest"]:
                raise PreparationError(f"{entry['filename']}: downloaded size or SHA-256 does not match the lock")
            output.flush()
            os.fsync(output.fileno())
        existing_regular_file(target)
        os.replace(partial, target)
    finally:
        # Only remove the temporary file this invocation created.
        partial.unlink(missing_ok=True)


def prepare(inputs, cache, check, timeout, retries):
    if not check:
        cache.mkdir(parents=True, exist_ok=True)
    errors = []
    for entry in inputs:
        target = cache / entry["filename"]
        reason = verify_cached(target, entry)
        if reason is None:
            print(f"Verified cached source: {entry['filename']}")
            continue
        if check:
            errors.append(f"{target}: {reason}")
            continue
        print(f"Preparing source: {entry['filename']} ({reason})", flush=True)
        for attempt in range(retries + 1):
            try:
                download_once(entry, cache, timeout)
                break
            except (OSError, ValueError, http.client.HTTPException, urllib.error.URLError, PreparationError) as error:
                if attempt == retries:
                    raise PreparationError(f"{entry['filename']}: preparation failed after {attempt + 1} attempt(s): {error}") from error
                print(f"Retrying {entry['filename']} after failed attempt {attempt + 1}: {error}", file=sys.stderr, flush=True)
                time.sleep(min(attempt + 1, 3))
        print(f"Downloaded and verified source: {entry['filename']}")
    if errors:
        raise PreparationError("offline cache check failed:\n  " + "\n  ".join(errors) + "\nRun preparation without --check while online, then retry.")
    print("PROFILE_SOURCES_READY")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path, default=REPOSITORY_ROOT / "profiles/python-data-v1/profile.lock.json", help="reviewed Profile lock (default: this checkout's python-data-v1)")
    parser.add_argument("--cache", type=Path, default=Path(os.environ.get("PROFILE_BUNDLE_SOURCE_CACHE") or REPOSITORY_ROOT / ".cache/profile-sources"), help="source directory (default: PROFILE_BUNDLE_SOURCE_CACHE or this checkout's .cache/profile-sources)")
    parser.add_argument("--check", action="store_true", help="read-only verification; never download or create a directory")
    parser.add_argument("--timeout", type=float, default=300, help="per-attempt transfer deadline in seconds; individual network reads time out within 30 seconds (default: 300)")
    parser.add_argument("--retries", type=int, default=3, help="additional download attempts, from 0 through 5 (default: 3)")
    args = parser.parse_args(argv)
    if not math.isfinite(args.timeout) or not 0 < args.timeout <= 3600:
        parser.error("--timeout must be greater than 0 and at most 3600 seconds")
    if not 0 <= args.retries <= 5:
        parser.error("--retries must be between 0 and 5")
    try:
        prepare(read_inputs(args.lock), args.cache.absolute(), args.check, args.timeout, args.retries)
    except (OSError, ValueError, PreparationError) as error:
        print(f"profile-sources: {error}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("profile-sources: interrupted; no unverified source was published", file=sys.stderr)
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
