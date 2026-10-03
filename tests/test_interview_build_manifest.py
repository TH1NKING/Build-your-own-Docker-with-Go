"""Regression checks for stale and altered demo artifacts (no Linux required)."""
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("manifest", Path(__file__).with_name("interview-build-manifest.py"))
manifest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(manifest)


class ManifestTests(unittest.TestCase):
    def test_source_and_product_changes_invalidate_prepared_snapshot(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ("go.mod", "go.sum", "Makefile"):
                (root / name).write_text("initial", encoding="utf-8")
            (root / "internal").mkdir()
            source = root / "internal" / "example.go"
            source.write_text("initial", encoding="utf-8")
            build = root / "build"
            build.mkdir()
            for name in manifest.PRODUCTS:
                (build / name).write_bytes(b"original")
            original = manifest.snapshot(root, build)
            self.assertEqual(original, manifest.snapshot(root, build))
            source.write_text("changed", encoding="utf-8")
            self.assertNotEqual(original, manifest.snapshot(root, build))
            with self.assertRaisesRegex(ValueError, "Source changed during preparation"):
                manifest.snapshot(root, build, original["sources"])
            source.write_text("initial", encoding="utf-8")
            (root / "internal" / "new.go").write_text("new", encoding="utf-8")
            self.assertNotEqual(original, manifest.snapshot(root, build))
            (root / "internal" / "new.go").unlink()
            (build / "sandboxd").write_bytes(b"altered")
            self.assertNotEqual(original, manifest.snapshot(root, build))
            (build / "sandboxd").unlink()
            with self.assertRaises(ValueError):
                manifest.snapshot(root, build)

    def test_snapshot_ignores_generated_python_cache(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ("go.mod", "go.sum", "Makefile"):
                (root / name).write_text("input", encoding="utf-8")
            before = manifest.sources(root)
            (root / "tests" / "__pycache__").mkdir(parents=True)
            (root / "tests" / "__pycache__" / "generated.pyc").write_bytes(b"cache")
            self.assertEqual(before, manifest.sources(root))


if __name__ == "__main__":
    unittest.main()
