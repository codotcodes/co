import { test } from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  rmSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import {
  environmentCommands,
  parseCommands,
  pathCommands,
  readBounded,
  runStep,
} from "../src/machine/command_files.mjs";

test("parses bounded single and multiline command files without evaluating text", () => {
  assert.deepEqual(
    { ...parseCommands("version=1\ntext<<END\n$(touch bad)\nsecond\nEND\n") },
    { version: "1", text: "$(touch bad)\nsecond" },
  );
  assert.throws(() => parseCommands("x<<END\nno ending"));
  assert.throws(() => parseCommands(`x=${"a".repeat(16385)}`));
  for (const name of [
    "PATH",
    "NODE_OPTIONS",
    "LD_PRELOAD",
    "CO_TOKEN",
    "GITHUB_TOKEN",
    "BASH_ENV",
  ])
    assert.throws(() => environmentCommands(`${name}=bad`));
  assert.throws(() => pathCommands("/safe:/unsafe"));
  assert.throws(() => pathCommands("relative"));
});

test("state persists across steps and remains isolated between jobs", async () => {
  const root = mkdtempSync(join(tmpdir(), "co-commands-"));
  const other = mkdtempSync(join(tmpdir(), "co-commands-"));
  try {
    assert.equal(
      await runStep({
        root,
        command:
          'printf "TEST_VALUE=works\\n" >> "$GITHUB_ENV"; printf "/custom/bin\\n" >> "$GITHUB_PATH"; printf "version=42\\n" >> "$GITHUB_OUTPUT"',
        id: "setup",
      }),
      0,
    );
    assert.equal(
      await runStep({
        root,
        command:
          'test "$TEST_VALUE" = works; case "$PATH" in /custom/bin:*) ;; *) exit 1;; esac',
      }),
      0,
    );
    assert.equal(
      JSON.parse(readFileSync(join(root, "state.json"))).outputs.setup.version,
      "42",
    );
    assert.equal(
      await runStep({ root: other, command: 'test -z "${TEST_VALUE:-}"' }),
      0,
    );
  } finally {
    rmSync(root, { recursive: true });
    rmSync(other, { recursive: true });
  }
});

test("command files never follow symlinks", () => {
  const root = mkdtempSync(join(tmpdir(), "co-commands-"));
  try {
    writeFileSync(join(root, "target"), "x=y");
    symlinkSync(join(root, "target"), join(root, "link"));
    assert.throws(() => readBounded(join(root, "link")));
  } finally {
    rmSync(root, { recursive: true });
  }
});

test("dynamic masks hide values and survive subsequent steps", () => {
  const root = mkdtempSync(join(tmpdir(), "co-masks-"));
  try {
    const module = new URL("../src/machine/command_files.mjs", import.meta.url)
      .href;
    const script = `import {runStep} from ${JSON.stringify(module)}; const root=${JSON.stringify(root)}; await runStep({root, command:'printf "::add-mask::private-value\\nprivate-value\\n"'}); await runStep({root, command:'echo private-value'});`;
    const result = spawnSync(
      process.execPath,
      ["--input-type=module", "-e", script],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, "***\n***\n");
  } finally {
    rmSync(root, { recursive: true });
  }
});

test("dynamic masks also exclude published action outputs", () => {
  const root = mkdtempSync(join(tmpdir(), "co-output-masks-"));
  try {
    const module = new URL("../src/machine/command_files.mjs", import.meta.url)
      .href;
    const output = join(root, "published.json");
    const script = `import {runStep} from ${JSON.stringify(module)}; process.env.CO_ACTION_OUTPUT=${JSON.stringify(output)}; await runStep({root:${JSON.stringify(root)}, outputKeys:["token","safe"], command:'echo "::add-mask::private-value"; printf "token=private-value\\nsafe=public\\n" >> "$GITHUB_OUTPUT"'});`;
    const result = spawnSync(
      process.execPath,
      ["--input-type=module", "-e", script],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(JSON.parse(readFileSync(output, "utf8")), {
      safe: "public",
    });
  } finally {
    rmSync(root, { recursive: true });
  }
});
