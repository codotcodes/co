import { test } from "node:test";
import assert from "node:assert/strict";
import { gzipSync } from "node:zlib";
import { mkdtempSync, rmSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import {
  extractTar,
  expand,
  selectNode,
} from "../src/machine/action_runtime.mjs";

function archive(name, type = "0", value = "fixture") {
  const header = Buffer.alloc(512);
  header.write(name, 0, 100);
  header.write("0000755", 100);
  header.write(Buffer.byteLength(value).toString(8).padStart(11, "0"), 124);
  header.write(type, 156);
  const content = Buffer.alloc(Math.ceil(Buffer.byteLength(value) / 512) * 512);
  content.write(value);
  return gzipSync(Buffer.concat([header, content, Buffer.alloc(512)]));
}

test("extracts bounded action files and rejects traversal and links", () => {
  const root = mkdtempSync(join(tmpdir(), "co-action-"));
  try {
    extractTar(archive("repo-commit/dist/index.js"), root);
    assert.equal(readFileSync(join(root, "dist/index.js"), "utf8"), "fixture");
    for (const name of [
      "repo/../outside",
      "/absolute/file",
      "repo/path\\escape",
    ])
      assert.throws(() => extractTar(archive(name), root));
    assert.throws(() => extractTar(archive("repo/link", "2"), root));
  } finally {
    rmSync(root, { recursive: true });
  }
});

test("selects explicit and LTS Node versions without treating input as shell", () => {
  const releases = [
    { version: "v24.1.0", lts: false, files: ["linux-x64", "linux-arm64"] },
    { version: "v22.3.1", lts: "Jod", files: ["linux-x64", "linux-arm64"] },
  ];
  assert.equal(selectNode(releases, "22"), "22.3.1");
  assert.equal(selectNode(releases, "22.3"), "22.3.1");
  assert.equal(selectNode(releases, "lts/*"), "22.3.1");
  assert.equal(selectNode(releases, "latest"), "24.1.0");
  assert.throws(() => selectNode(releases, "22; curl attacker"));
});

test("composite interpolation has an explicit context allowlist", () => {
  assert.equal(
    expand(
      "${{ inputs.version }} ${{ env.NAME }}",
      { version: "1" },
      { NAME: "test" },
    ),
    "1 test",
  );
  assert.throws(() => expand("${{ secrets.TOKEN }}", {}));
});

test("composite steps carry environment and map declared outputs", () => {
  const root = mkdtempSync(join(tmpdir(), "co-composite-"));
  try {
    const module = new URL("../src/machine/action_runtime.mjs", import.meta.url)
      .href;
    const plan = {
      runs: {
        steps: [
          {
            id: "first",
            shell: "sh",
            run: 'echo CHILD_VALUE=works >> "$GITHUB_ENV"; echo result=42 >> "$GITHUB_OUTPUT"',
          },
          {
            shell: "sh",
            run: 'test "$CHILD_VALUE" = works; test "${{ steps.first.outputs.result }}" = 42; test "${{ inputs.message }}" = hello',
          },
        ],
      },
      outputs: { result: { value: "${{ steps.first.outputs.result }}" } },
    };
    const script = `import {runComposite} from ${JSON.stringify(module)}; process.env.GITHUB_ENV=${JSON.stringify(join(root, "parent-env"))}; process.env.GITHUB_PATH=${JSON.stringify(join(root, "parent-path"))}; process.env.GITHUB_OUTPUT=${JSON.stringify(join(root, "parent-output"))}; await runComposite(${JSON.stringify(plan)}, {message:"hello"}, ${JSON.stringify(root)});`;
    const result = spawnSync(
      process.execPath,
      ["--input-type=module", "-e", script],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.match(
      readFileSync(join(root, "parent-env"), "utf8"),
      /CHILD_VALUE=works/,
    );
    assert.match(
      readFileSync(join(root, "parent-output"), "utf8"),
      /result<<CO_.*\n42\n/,
    );
  } finally {
    rmSync(root, { recursive: true });
  }
});
