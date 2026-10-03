"""No-network regression tests for the interview source-cache preparation."""

import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("prepare_profile_sources", Path(__file__).with_name("prepare-profile-sources.py"))
SOURCES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SOURCES)


class Download(io.BytesIO):
    def __init__(self, data, length=None, url="https://example.invalid/archive.tar.gz"):
        super().__init__(data)
        self.headers = {} if length is None else {"Content-Length": length}
        self.url = url

    def geturl(self):
        return self.url


class ProfileSourceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.cache = self.root / "cache"
        self.data = b"locked source bytes"
        self.entry = {"filename": "archive.tar.gz", "source": "https://example.invalid/archive.tar.gz", "size": len(self.data), "digest": "sha256:" + hashlib.sha256(self.data).hexdigest()}
        self.lock = self.root / "profile.lock.json"
        self.write_lock()
        self.stdout = io.StringIO()
        self.stderr = io.StringIO()
        self.enterContext(contextlib.redirect_stdout(self.stdout))
        self.enterContext(contextlib.redirect_stderr(self.stderr))
        self.opener = self.enterContext(mock.patch.object(SOURCES.urllib.request, "build_opener"))

    def write_lock(self, entries=None):
        self.lock.write_text(json.dumps({"schema": "profile-build-lock/v1", "profile": "python-data-v1", "target": {"os": "linux", "arch": "amd64"}, "inputs": [self.entry] if entries is None else entries}), encoding="utf-8")

    def invoke(self, *arguments):
        return SOURCES.main(["--lock", str(self.lock), "--cache", str(self.cache), "--retries", "0", *arguments])

    def cache_bytes(self, data):
        self.cache.mkdir(exist_ok=True)
        (self.cache / self.entry["filename"]).write_bytes(data)

    def serve(self, data, **kwargs):
        self.opener.return_value.open.return_value = Download(data, **kwargs)

    def assert_no_partial(self):
        self.assertEqual(list(self.cache.glob("*.part")), [])

    def test_offline_check_missing_cache_does_not_create_or_download(self):
        self.assertEqual(self.invoke("--check"), 1)
        self.assertFalse(self.cache.exists())
        self.opener.assert_not_called()
        self.assertNotIn("PROFILE_SOURCES_READY", self.stdout.getvalue())
        self.assertIn("missing", self.stderr.getvalue())

    def test_offline_check_and_prepare_reuse_only_valid_bytes(self):
        self.cache_bytes(self.data)
        before = (self.cache / self.entry["filename"]).stat().st_mtime_ns
        self.assertEqual(self.invoke("--check"), 0)
        self.assertEqual(self.invoke(), 0)
        self.assertIn("PROFILE_SOURCES_READY", self.stdout.getvalue())
        self.assertEqual((self.cache / self.entry["filename"]).stat().st_mtime_ns, before)
        self.opener.assert_not_called()

    def test_offline_check_rejects_size_and_digest_without_modifying_cache(self):
        for data in (b"short", b"x" * len(self.data)):
            with self.subTest(data=data):
                self.cache_bytes(data)
                self.assertEqual(self.invoke("--check"), 1)
                self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), data)
        self.opener.assert_not_called()

    def test_download_publishes_verified_bytes_and_reuses_on_next_check(self):
        self.serve(self.data, length=str(len(self.data)))
        self.assertEqual(self.invoke(), 0)
        self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), self.data)
        self.assert_no_partial()
        self.assertEqual(self.invoke("--check"), 0)
        self.opener.return_value.open.assert_called_once()

    def test_failed_download_preserves_old_target_and_unrelated_partial(self):
        self.cache_bytes(b"previous source")
        unrelated = self.cache / "operator.part"
        unrelated.write_bytes(b"leave alone")
        self.serve(b"x" * len(self.data))
        self.assertEqual(self.invoke(), 1)
        self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), b"previous source")
        self.assertEqual(unrelated.read_bytes(), b"leave alone")
        self.assertEqual(list(self.cache.glob("*.part")), [unrelated])
        self.assertNotIn("PROFILE_SOURCES_READY", self.stdout.getvalue())

    def test_oversized_truncated_and_bad_content_length_downloads_fail(self):
        for data, length in ((self.data + b"extra", None), (self.data[:-1], None), (self.data, "0"), (self.data, "unknown")):
            with self.subTest(data=data, length=length):
                self.serve(data, length=length)
                self.assertEqual(self.invoke(), 1)
                self.assertFalse((self.cache / self.entry["filename"]).exists())
                self.assert_no_partial()

    def test_valid_download_replaces_corrupt_regular_file(self):
        self.cache_bytes(b"corrupt")
        self.serve(self.data)
        self.assertEqual(self.invoke(), 0)
        self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), self.data)

    def test_unsafe_and_duplicate_filenames_fail_before_cache_creation(self):
        for filename in ("../outside", "a/b", "a\\b", ".", "..", "/tmp/archive", "C:outside", "has\nnewline", "-option", "name.", "NUL", "a" * 256):
            with self.subTest(filename=filename):
                self.entry["filename"] = filename
                self.write_lock()
                self.assertEqual(self.invoke(), 1)
        self.entry["filename"] = "archive.tar.gz"
        self.write_lock([self.entry, self.entry])
        self.assertEqual(self.invoke(), 1)
        self.assertFalse(self.cache.exists())
        self.opener.assert_not_called()

    def test_invalid_sizes_digests_urls_and_duplicate_keys_fail(self):
        cases = [("size", True), ("size", 0), ("size", SOURCES.MAX_INPUT_SIZE + 1), ("digest", "sha256:" + "A" * 64), ("source", "http://example.invalid/file"), ("source", "file:///tmp/file"), ("source", "https://user:password@example.invalid/file"), ("source", "https://example.invalid:bad/file"), ("source", "https://example.invalid/\nfile")]
        original = dict(self.entry)
        for key, value in cases:
            with self.subTest(key=key, value=value):
                self.entry = {**original, key: value}
                self.write_lock()
                self.assertEqual(self.invoke(), 1)
        self.lock.write_text('{"schema":"one","schema":"two"}', encoding="utf-8")
        self.assertEqual(self.invoke(), 1)
        self.assertFalse(self.cache.exists())
        self.opener.assert_not_called()

    def test_cache_directory_in_place_of_file_is_not_replaced(self):
        target = self.cache / self.entry["filename"]
        target.mkdir(parents=True)
        self.assertEqual(self.invoke(), 1)
        self.assertTrue(target.is_dir())
        self.opener.assert_not_called()

    def test_cache_symlink_is_not_followed_or_replaced(self):
        self.cache.mkdir()
        outside = self.root / "outside"
        outside.write_bytes(self.data)
        target = self.cache / self.entry["filename"]
        try:
            target.symlink_to(outside)
        except OSError:
            self.skipTest("creating symlinks requires privilege on this platform")
        self.assertEqual(self.invoke(), 1)
        self.assertEqual(self.invoke("--check"), 1)
        self.assertTrue(target.is_symlink())
        self.assertEqual(outside.read_bytes(), self.data)
        self.opener.assert_not_called()

    def test_transfer_deadline_stops_before_read_and_cleans_up(self):
        self.serve(self.data)
        with mock.patch.object(SOURCES.time, "monotonic", side_effect=[0, 301]):
            self.assertEqual(self.invoke(), 1)
        self.assertIn("exceeded", self.stderr.getvalue())
        self.assertFalse((self.cache / self.entry["filename"]).exists())
        self.assert_no_partial()

    def test_http_redirect_is_rejected(self):
        handler = SOURCES.HTTPSOnlyRedirectHandler()
        with self.assertRaises(SOURCES.PreparationError):
            handler.redirect_request(None, None, 302, "Found", {}, "http://example.invalid/archive")

    def test_retry_uses_fresh_partial_and_publishes_only_success(self):
        self.opener.return_value.open.side_effect = [OSError("connection interrupted"), Download(self.data)]
        with mock.patch.object(SOURCES.time, "sleep"):
            self.assertEqual(self.invoke("--retries", "1"), 0)
        self.assertEqual(self.opener.return_value.open.call_count, 2)
        self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), self.data)
        self.assert_no_partial()

    def test_broken_http_response_has_actionable_failure_and_no_partial(self):
        self.opener.return_value.open.side_effect = SOURCES.http.client.IncompleteRead(b"partial")
        self.assertEqual(self.invoke(), 1)
        self.assertIn("preparation failed", self.stderr.getvalue())
        self.assertFalse((self.cache / self.entry["filename"]).exists())
        self.assert_no_partial()

    def test_interrupted_download_cleans_own_partial_and_preserves_old_target(self):
        self.cache_bytes(b"previous source")
        self.opener.return_value.open.side_effect = KeyboardInterrupt()
        self.assertEqual(self.invoke(), 130)
        self.assertEqual((self.cache / self.entry["filename"]).read_bytes(), b"previous source")
        self.assert_no_partial()


if __name__ == "__main__":
    unittest.main()
