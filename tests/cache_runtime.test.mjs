import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { cachePath, decodeCache } from "../src/machine/cache_runtime.mjs";

const hash = (value) => createHash("sha256").update(value).digest("hex");
function bundle(path = "@scope/file.txt") {
  const content = Buffer.from("cached");
  const manifest = Buffer.from(
    JSON.stringify([{ path, size: content.length, sha256: hash(content) }]),
  );
  const size = Buffer.alloc(4);
  size.writeUInt32BE(manifest.length);
  const data = Buffer.concat([Buffer.from("COAR1\0"), size, manifest, content]);
  const metadata = Buffer.from(
    JSON.stringify({ key: "v1", path: "/workspace/deps", digest: hash(data) }),
  );
  const metaSize = Buffer.alloc(4);
  metaSize.writeUInt32BE(metadata.length);
  return Buffer.concat([Buffer.from("COCA1\0"), metaSize, metadata, data]);
}

test("verifies immutable cache metadata, file digests and paths before restore", () => {
  assert.equal(
    decodeCache(bundle(), "/workspace/deps").entries[0].content.toString(),
    "cached",
  );
  assert.throws(() => decodeCache(bundle(), "/workspace/other"));
  assert.throws(() => decodeCache(bundle("../escape"), "/workspace/deps"));
  const changed = bundle();
  changed[changed.length - 1] ^= 1;
  assert.throws(() => decodeCache(changed, "/workspace/deps"));
});

test("cache directory selection cannot address host or credential mounts", () => {
  assert.equal(
    cachePath("~/.bun/install/cache", "/workspace", "/run/co-job/home"),
    "/run/co-job/home/.bun/install/cache",
  );
  assert.equal(
    cachePath("deps", "/workspace", "/run/co-job/home"),
    "/workspace/deps",
  );
  for (const path of [
    "/etc",
    "../../etc",
    ".git",
    "/run/co-environment.sh",
    "deps\nother",
  ])
    assert.throws(() => cachePath(path, "/workspace", "/run/co-job/home"));
});
