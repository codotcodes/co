import runpy
from pathlib import Path
import unittest

release_notes = runpy.run_path(Path(__file__).with_name("release-notes.py"))[
    "release_notes"
]


class ReleaseNotesTests(unittest.TestCase):
    def test_extracts_only_the_requested_release(self):
        source = (
            "## [Unreleased]\n\n## [0.9.0] - 2026-10-17\n\n- New feature.\n\n"
            "## [0.8.0] - 2026-09-30\n\n- Old feature.\n"
        )
        self.assertEqual(release_notes(source, "v0.9.0"), "- New feature.\n")

    def test_rejects_missing_empty_duplicate_and_nonstable_entries(self):
        heading = "## [0.9.0] - 2026-10-17\n"
        for source, tag in [
            (heading, "v0.9.0"),
            (heading + "- Notes.\n", "v0.9.1"),
            ((heading + "- Notes.\n") * 2, "v0.9.0"),
            (heading + "- Notes.\n", "v0.9.0-beta.1"),
        ]:
            with self.subTest(source=source, tag=tag), self.assertRaises(ValueError):
                release_notes(source, tag)


if __name__ == "__main__":
    unittest.main()
