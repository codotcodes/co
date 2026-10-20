import {
  constants,
  closeSync,
  existsSync,
  fstatSync,
  lstatSync,
  mkdirSync,
  openSync,
  readSync,
  readdirSync,
  statSync,
  writeFileSync,
  chmodSync,
} from "node:fs";
import { createHash } from "node:crypto";
import { dirname, join, resolve, sep } from "node:path";
import { readBounded } from "./command_files.mjs";

const limit = 32 * 1024 * 1024;
const hash = (value) => createHash("sha256").update(value).digest("hex");
const validPath = (value) =>
  typeof value === "string" &&
  value.length <= 1024 &&
  value
    .split("/")
    .every(
      (part) =>
        part &&
        ![".", ".."].includes(part) &&
        part.toLowerCase() !== ".git" &&
        !/[\\\x00-\x1f\x7f]/.test(part),
    );

export function cachePath(value, workspace, home) {
  if (typeof value !== "string" || /[\r\n*?\0]/.test(value))
    throw new Error("Cache requires one literal directory path");
  const path = resolve(
    value.startsWith("~/")
      ? join(home, value.slice(2))
      : value.startsWith("/")
        ? value
        : join(workspace, value),
  );
  if (
    ![`${workspace}${sep}`, `${home}${sep}`].some((root) =>
      path.startsWith(root),
    )
  )
    throw new Error("Cache path must be inside the checkout or job home");
  const relative = path.startsWith(`${workspace}${sep}`)
    ? path.slice(workspace.length + 1)
    : path.slice(home.length + 1);
  if (!validPath(relative)) throw new Error("Invalid cache directory path");
  return path;
}

function readBinary(path) {
  const fd = openSync(
    path,
    constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
  );
  try {
    const metadata = fstatSync(fd);
    if (
      !metadata.isFile() ||
      metadata.nlink !== 1 ||
      metadata.size > limit + 4096
    )
      throw new Error("Invalid cache entry");
    const bytes = Buffer.alloc(metadata.size);
    let offset = 0;
    while (offset < bytes.length) {
      const count = readSync(fd, bytes, offset, bytes.length - offset, null);
      if (!count) throw new Error("Truncated cache entry");
      offset += count;
    }
    return bytes;
  } finally {
    closeSync(fd);
  }
}

export function decodeCache(bytes, path) {
  if (bytes.length < 10 || bytes.subarray(0, 6).toString() !== "COCA1\0")
    throw new Error("Invalid cache header");
  const size = bytes.readUInt32BE(6);
  if (size > 4096 || 10 + size > bytes.length)
    throw new Error("Invalid cache metadata");
  const metadata = JSON.parse(bytes.subarray(10, 10 + size));
  const bundle = bytes.subarray(10 + size);
  if (
    metadata.path !== path ||
    metadata.digest !== hash(bundle) ||
    bundle.length < 10 ||
    bundle.subarray(0, 6).toString() !== "COAR1\0"
  )
    throw new Error("Cache integrity failure");
  const manifestSize = bundle.readUInt32BE(6);
  if (manifestSize > 1024 * 1024 || 10 + manifestSize > bundle.length)
    throw new Error("Invalid cache manifest");
  const manifest = JSON.parse(bundle.subarray(10, 10 + manifestSize));
  if (!Array.isArray(manifest) || manifest.length > 4096)
    throw new Error("Too many cached files");
  const entries = [];
  const paths = new Set();
  let offset = 10 + manifestSize;
  for (const entry of manifest) {
    if (
      !validPath(entry.path) ||
      paths.has(entry.path) ||
      !Number.isSafeInteger(entry.size) ||
      entry.size < 0 ||
      entry.size > limit ||
      offset + entry.size > bundle.length
    )
      throw new Error("Invalid cached file");
    paths.add(entry.path);
    const content = bundle.subarray(offset, offset + entry.size);
    if (hash(content) !== entry.sha256)
      throw new Error("Cached file integrity failure");
    if (entry.mode !== undefined && ![0o644, 0o755].includes(entry.mode))
      throw new Error("Invalid cached file permissions");
    entries.push({ path: entry.path, content, mode: entry.mode ?? 0o644 });
    offset += entry.size;
  }
  if (offset !== bundle.length) throw new Error("Unexpected cache content");
  return { metadata, entries };
}

function restore(directory, entries) {
  for (const entry of entries) {
    const target = join(directory, entry.path);
    const parent = dirname(target);
    for (let current = parent; current !== "/"; current = dirname(current)) {
      if (existsSync(current) && !lstatSync(current).isDirectory())
        throw new Error("Cache directory contains a link");
    }
    mkdirSync(parent, { recursive: true });
    if (
      existsSync(target) &&
      (!lstatSync(target).isFile() || lstatSync(target).nlink !== 1)
    )
      throw new Error("Cache target contains a link");
    writeFileSync(target, entry.content, {
      flag:
        constants.O_WRONLY |
        constants.O_CREAT |
        constants.O_TRUNC |
        constants.O_NOFOLLOW,
      mode: 0o644,
    });
    chmodSync(target, entry.mode);
  }
}

export function runCache(inputs, cacheRoot = "/run/co-cache") {
  const path = cachePath(
    inputs.path,
    process.env.GITHUB_WORKSPACE,
    process.env.HOME,
  );
  const key = inputs.key;
  if (typeof key !== "string" || !key || key.length > 512 || key.includes("\0"))
    throw new Error("Cache key must contain 1-512 characters");
  const primary = hash(`${path}\0${key}`);
  const prefixes = (inputs["restore-keys"] ?? "")
    .split(/\r?\n/)
    .filter(Boolean);
  if (prefixes.length > 16 || prefixes.some((prefix) => prefix.length > 512))
    throw new Error("Too many cache restore keys");
  let matched;
  let selected;
  let rank = Infinity;
  if (existsSync(cacheRoot)) {
    const names = readdirSync(cacheRoot)
      .filter((name) => /^[a-f0-9]{64}\.cache$/.test(name))
      .slice(0, 32);
    const modified = (name) => {
      try {
        return statSync(join(cacheRoot, name)).mtimeMs;
      } catch {
        return 0;
      }
    };
    names.sort(
      (a, b) =>
        Number(b === `${primary}.cache`) - Number(a === `${primary}.cache`) ||
        modified(b) - modified(a),
    );
    for (const name of names) {
      try {
        const value = decodeCache(readBinary(join(cacheRoot, name)), path);
        const candidateRank =
          value.metadata.key === key
            ? -1
            : prefixes.findIndex((prefix) =>
                value.metadata.key.startsWith(prefix),
              );
        if (
          (candidateRank < 0 && value.metadata.key !== key) ||
          candidateRank >= rank
        )
          continue;
        selected = value;
        rank = candidateRank;
        if (rank === -1) break;
      } catch {
        /* A missing or corrupt optional entry is a cache miss. */
      }
    }
    if (selected) {
      try {
        if (inputs["lookup-only"] !== "true") restore(path, selected.entries);
        matched = selected.metadata.key;
      } catch {
        console.warn("Cache restore failed; continuing without a cache hit.");
      }
    }
  } else
    console.warn("Runner cache is unavailable; continuing without a cache.");
  const output = `cache-hit=${matched === key ? "true" : "false"}\n`;
  writeFileSync(process.env.GITHUB_OUTPUT, output, { flag: "a" });
  if (inputs["fail-on-cache-miss"] === "true" && !matched)
    throw new Error("No matching cache entry");
  if (inputs["lookup-only"] !== "true" && matched !== key) {
    const requestPath = join(
      dirname(process.env.RUNNER_TOOL_CACHE),
      "cache-requests.json",
    );
    const requests = JSON.parse(readBounded(requestPath) || "[]");
    if (!Array.isArray(requests) || requests.length >= 16)
      throw new Error("Too many cache requests");
    requests.push({ key, path });
    writeFileSync(requestPath, JSON.stringify(requests), {
      flag:
        constants.O_WRONLY |
        constants.O_CREAT |
        constants.O_TRUNC |
        constants.O_NOFOLLOW,
      mode: 0o600,
    });
  }
  console.log(
    matched
      ? "Restored a matching runner cache."
      : "No matching runner cache; save is conditional on successful trusted completion.",
  );
}
