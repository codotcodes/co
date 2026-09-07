#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.release = self.root / "release"
        self.release.mkdir()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.destination = self.root / "directory with spaces"
        self.env = dict(os.environ, CO_DOWNLOAD_BASE=self.release.as_uri(), CO_INSTALL_DIR=str(self.destination), PATH=f"{self.tools}:{os.environ['PATH']}")

    def tool(self, name, body):
        path = self.tools / name
        path.write_text("#!/bin/sh\n" + body + "\n")
        path.chmod(0o755)

    def platform(self, os_name, arch, libc):
        self.tool("uname", f'case "$1" in -s) echo {os_name};; -m) echo {arch};; esac')
        self.env["CO_LIBC"] = "gnu" if libc == "glibc" else "musl"

    def archive(self, target, version, symlink=False):
        name = f"co-{target}.tar.gz"
        archive = self.release / name
        data = f"#!/bin/sh\nprintf 'co {version}\\n'\n".encode()
        with tarfile.open(archive, "w:gz") as tar:
            info = tarfile.TarInfo("./co")
            info.mode = 0o755
            if symlink:
                info.type = tarfile.SYMTYPE
                info.linkname = "/bin/sh"
                tar.addfile(info)
            else:
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (self.release / (name + ".sha256")).write_text(f"{digest}  {name}\n")
        return archive

    def install(self, success=True):
        result = subprocess.run(["sh", str(ROOT / "install.sh")], env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)

    def version(self):
        return subprocess.check_output([str(self.destination / "co"), "version"], text=True).strip()

    def test_supported_targets_install_and_upgrade(self):
        for system, arch, libc, target in [
            ("Linux", "x86_64", "glibc", "x86_64-unknown-linux-gnu"),
            ("Linux", "aarch64", "glibc", "aarch64-unknown-linux-gnu"),
            ("Linux", "x86_64", "musl", "x86_64-unknown-linux-musl"),
            ("Linux", "aarch64", "musl", "aarch64-unknown-linux-musl"),
            ("Darwin", "x86_64", "unused", "x86_64-apple-darwin"),
            ("Darwin", "arm64", "unused", "aarch64-apple-darwin"),
        ]:
            with self.subTest(target=target):
                self.platform(system, arch, libc)
                self.archive(target, "0.0.1")
                self.install()
                self.assertEqual(self.version(), "co 0.0.1")
                self.archive(target, "0.0.2")
                self.install()
                self.assertEqual(self.version(), "co 0.0.2")
                self.assertEqual(list(self.destination.glob(".co.*")), [])

    def test_invalid_download_preserves_installed_version(self):
        self.platform("Linux", "x86_64", "glibc")
        archive = self.archive("x86_64-unknown-linux-gnu", "0.0.1")
        self.install()
        archive.write_bytes(b"corrupted")
        self.install(False)
        self.assertEqual(self.version(), "co 0.0.1")
        archive = self.archive("x86_64-unknown-linux-gnu", "0.0.2")
        archive.with_name(archive.name + ".sha256").write_text("0" * 64 + "  different-archive\n")
        self.install(False)
        self.assertEqual(self.version(), "co 0.0.1")
        self.archive("x86_64-unknown-linux-gnu", "0.0.2", symlink=True)
        self.install(False)
        self.assertEqual(self.version(), "co 0.0.1")

    def test_unsupported_platform(self):
        self.platform("Windows_NT", "x86_64", "unused")
        self.install(False)
        self.assertFalse(self.destination.exists())

    def test_linux_defaults_to_static_musl(self):
        self.platform("Linux", "x86_64", "unused")
        del self.env["CO_LIBC"]
        self.archive("x86_64-unknown-linux-musl", "0.0.1")
        self.install()
        self.assertEqual(self.version(), "co 0.0.1")

    def test_formula_requires_verified_archives(self):
        spec = importlib.util.spec_from_file_location("formula", ROOT / "scripts/homebrew-formula.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for arch in ["aarch64", "x86_64"]:
            for os_name in ["apple-darwin", "unknown-linux-musl"]:
                self.archive(f"{arch}-{os_name}", "0.0.1")
        self.assertIn('version "0.0.1"', module.formula("v0.0.1", self.release))
        next(self.release.glob("*.tar.gz")).write_bytes(b"changed")
        with self.assertRaises(ValueError):
            module.formula("v0.0.1", self.release)


if __name__ == "__main__":
    unittest.main()
