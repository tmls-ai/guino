import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("check_release", Path(__file__).parents[1] / "check-release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for directory in ("cmd/guino", "sdk/typescript", "sdk/python"):
            (self.root / directory).mkdir(parents=True)
        (self.root / "cmd/guino/main.go").touch()
        (self.root / "go.mod").write_text("module github.com/tmls-ai/guino\n")
        (self.root / "sdk/typescript/package.json").write_text(json.dumps({"name": "@tmls-ai/guino", "version": "0.1.0"}))
        (self.root / "sdk/python/pyproject.toml").write_text('[project]\nname = "guino"\nversion = "0.1.0"\n')

    def test_guino_release(self):
        release.check(self.root, "v0.1.0")

    def test_legacy_tag_rejected(self):
        with self.assertRaisesRegex(ValueError, "Guino releases require version 0.1.0 or newer"):
            release.check(self.root, "v0.0.6")

    def test_legacy_source_rejected_even_with_new_tag(self):
        (self.root / "go.mod").write_text("module github.com/us/den\n")
        with self.assertRaisesRegex(ValueError, "Guino Go module"):
            release.check(self.root, "v0.1.0")

    def test_version_mismatch_rejected(self):
        with self.assertRaisesRegex(ValueError, "must match"):
            release.check(self.root, "v0.2.0")

    def test_non_version_ref_rejected(self):
        for tag in ("main", "v0.1.0;echo bad", "v01.1.0", "v0.1.0/other"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release.check(self.root, tag)
