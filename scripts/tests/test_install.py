import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parents[1] / "install.sh"


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="guino-installer-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / "tools"
        self.bin.mkdir()
        self.dest = self.root / "install with spaces"
        self.dest.mkdir()
        self.env = os.environ | {
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "GUINO_INSTALL_DIR": str(self.dest),
            "GUINO_VERSION": "v0.1.0",
            "TEST_FIXTURES": str(self.root),
            "TEST_OS": "Darwin",
            "TEST_ARCH": "arm64",
        }
        self.tool("uname", '#!/bin/sh\ncase "$1" in -s) echo "$TEST_OS";; -m) echo "$TEST_ARCH";; esac\n')
        self.tool("curl", '''#!/usr/bin/env python3
import os, pathlib, shutil, sys
args = sys.argv[1:]
url = next(a for a in args if a.startswith("https://"))
root = pathlib.Path(os.environ["TEST_FIXTURES"])
with (root / "urls.txt").open("a") as log:
    log.write(url + "\\n")
if url.endswith("/releases/latest"):
    if os.environ.get("TEST_NO_RELEASE"):
        sys.exit(22)
    print('{"tag_name":"v0.1.0"}')
else:
    source = root / url.rsplit("/", 1)[1]
    if not source.exists():
        sys.exit(22)
    shutil.copyfile(source, args[args.index("-o") + 1])
''')

    def tool(self, name, content):
        path = self.bin / name
        path.write_text(content)
        path.chmod(0o755)

    def archive(self, os_name="darwin", arch="arm64", corrupt_checksum=False, symlink=False):
        name = f"guino_0.1.0_{os_name}_{arch}.tar.gz"
        path = self.root / name
        body = b"#!/bin/sh\nprintf 'guino 0.1.0\\n'\n"
        with tarfile.open(path, "w:gz") as tar:
            entry = tarfile.TarInfo("guino")
            entry.mode = 0o755
            if symlink:
                entry.type = tarfile.SYMTYPE
                entry.linkname = "/bin/sh"
                tar.addfile(entry)
            else:
                entry.size = len(body)
                tar.addfile(entry, io.BytesIO(body))
        digest = "0" * 64 if corrupt_checksum else hashlib.sha256(path.read_bytes()).hexdigest()
        (self.root / "checksums.txt").write_text(f"{digest}  {name}\n")

    def run_installer(self):
        return subprocess.run(["sh", str(INSTALLER)], env=self.env, text=True, capture_output=True)

    def test_install_all_release_platforms(self):
        for os_name, machine, archive_os, archive_arch in (
            ("Darwin", "arm64", "darwin", "arm64"),
            ("Darwin", "x86_64", "darwin", "amd64"),
            ("Linux", "aarch64", "linux", "arm64"),
            ("Linux", "x86_64", "linux", "amd64"),
        ):
            with self.subTest(os=os_name, arch=machine):
                self.env.update(TEST_OS=os_name, TEST_ARCH=machine)
                self.archive(archive_os, archive_arch)
                result = self.run_installer()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(subprocess.check_output([str(self.dest / "guino")], text=True), "guino 0.1.0\n")
                self.assertFalse(list(self.dest.glob(".guino.*")))

    def test_corrupt_download_does_not_replace_existing_binary(self):
        target = self.dest / "guino"
        target.write_text("existing binary")
        self.archive(corrupt_checksum=True)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Checksum mismatch", result.stderr)
        self.assertEqual(target.read_text(), "existing binary")

    def test_latest_release_discovery(self):
        self.env.pop("GUINO_VERSION")
        self.archive()
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("api.github.com/repos/tmls-ai/guino/releases/latest", (self.root / "urls.txt").read_text())

    def test_no_release_has_source_install_guidance(self):
        self.env.pop("GUINO_VERSION")
        self.env["TEST_NO_RELEASE"] = "1"
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Build from source", result.stderr)
        self.assertFalse((self.dest / "guino").exists())

    def test_missing_checksum_rejected(self):
        self.archive()
        (self.root / "checksums.txt").write_text("unrelated\n")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum missing", result.stderr)

    def test_symlink_binary_rejected(self):
        self.archive(symlink=True)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("regular guino binary", result.stderr)

    def test_legacy_and_unsafe_version_rejected_before_download(self):
        for version in ("v0.0.6", "v0.1.0/../../main", "v01.1.0", "latest"):
            with self.subTest(version=version):
                self.env["GUINO_VERSION"] = version
                self.assertNotEqual(self.run_installer().returncode, 0)
                self.assertFalse((self.root / "urls.txt").exists())

    def test_unsupported_platform_rejected(self):
        self.env["TEST_OS"] = "Windows_NT"
        self.assertNotEqual(self.run_installer().returncode, 0)
        self.assertFalse((self.root / "urls.txt").exists())

    def test_directory_destination_is_not_reported_as_installed(self):
        self.archive()
        existing = self.dest / "guino"
        existing.mkdir()
        marker = existing / "keep"
        marker.write_text("unchanged")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("destination is a directory", result.stderr)
        self.assertEqual(list(existing.iterdir()), [marker])
        self.assertEqual(marker.read_text(), "unchanged")

    def test_symlink_to_directory_destination_is_preserved(self):
        self.archive()
        existing = self.root / "unrelated"
        existing.mkdir()
        (self.dest / "guino").symlink_to(existing, target_is_directory=True)
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(existing.iterdir()), [])
        self.assertTrue((self.dest / "guino").is_symlink())
