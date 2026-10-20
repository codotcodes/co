import {
  constants,
  openSync,
  readSync,
  closeSync,
  fstatSync,
  writeFileSync,
  mkdirSync,
  readdirSync,
  lstatSync,
} from "node:fs";
import { join } from "node:path";
import { spawn } from "node:child_process";
import { pathToFileURL } from "node:url";
import { StringDecoder } from "node:string_decoder";
import { createHash } from "node:crypto";
import { resolve, relative, sep } from "node:path";

const maxBytes = 65536;
const reserved = /^(?:CO_|GITHUB_|RUNNER_|LD_|DYLD_)/;
const forbidden = new Set([
  "HOME",
  "PATH",
  "ENV",
  "BASH_ENV",
  "IFS",
  "CDPATH",
  "SHELLOPTS",
  "BASHOPTS",
  "NODE_OPTIONS",
  "__proto__",
  "constructor",
  "prototype",
]);

export function readBounded(path) {
  let fd;
  try {
    fd = openSync(
      path,
      constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
    );
    const stat = fstatSync(fd);
    if (!stat.isFile() || stat.nlink !== 1 || stat.size > maxBytes)
      throw new Error("Invalid workflow command file");
    const bytes = Buffer.alloc(maxBytes + 1);
    let count = 0;
    for (;;) {
      const read = readSync(fd, bytes, count, bytes.length - count, null);
      count += read;
      if (count > maxBytes)
        throw new Error("Workflow command file exceeds 64 KiB");
      if (!read) break;
    }
    return new TextDecoder("utf-8", { fatal: true }).decode(
      bytes.subarray(0, count),
    );
  } catch (error) {
    if (error.code === "ENOENT") return "";
    throw error;
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

export function parseCommands(source) {
  if (Buffer.byteLength(source) > maxBytes || source.includes("\0"))
    throw new Error("Invalid workflow commands");
  const lines = source.replace(/\r\n/g, "\n").split("\n");
  const result = Object.create(null);
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i]) continue;
    const match = /^([A-Za-z_][A-Za-z0-9_-]{0,99})(=|<<)(.*)$/.exec(lines[i]);
    if (!match) throw new Error("Invalid workflow command syntax");
    const [, key, operator, rest] = match;
    let value = rest;
    if (operator === "<<") {
      if (!rest) throw new Error("Empty workflow command delimiter");
      const content = [];
      while (++i < lines.length && lines[i] !== rest) content.push(lines[i]);
      if (i === lines.length) throw new Error("Unterminated workflow command");
      value = content.join("\n");
    }
    if (Buffer.byteLength(value) > 16384)
      throw new Error("Workflow command value exceeds 16 KiB");
    result[key] = value;
    if (Object.keys(result).length > 100)
      throw new Error("Too many workflow commands");
  }
  return result;
}

export function environmentCommands(source) {
  const values = parseCommands(source);
  return validateEnvironment(values);
}

function validateEnvironment(values) {
  for (const key of Object.keys(values)) {
    if (
      !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key) ||
      reserved.test(key) ||
      forbidden.has(key)
    )
      throw new Error(`Reserved workflow environment name: ${key}`);
    if (
      typeof values[key] !== "string" ||
      values[key].includes("\0") ||
      Buffer.byteLength(values[key]) > 16384
    )
      throw new Error("Invalid persisted environment value");
  }
  return values;
}

export function pathCommands(source) {
  const paths = source.split(/\r?\n/).filter(Boolean);
  if (
    paths.length > 100 ||
    paths.some(
      (path) =>
        !path.startsWith("/") || /[:\0\r\n]/.test(path) || path.length > 4096,
    )
  )
    throw new Error("Invalid workflow PATH entry");
  return [...new Set(paths)];
}

export async function runStep({
  root,
  command,
  shell = "sh",
  id,
  env = {},
  outputKeys,
  workspace = process.cwd(),
}) {
  if (!["sh", "bash"].includes(shell))
    throw new Error("Unsupported workflow shell");
  mkdirSync(root, { recursive: true });
  const statePath = join(root, "state.json");
  const state = JSON.parse(
    readBounded(statePath) || '{"env":{},"paths":[],"outputs":{}}',
  );
  const files = Object.fromEntries(
    ["ENV", "PATH", "OUTPUT", "STATE", "STEP_SUMMARY"].map((key) => [
      key,
      join(root, key.toLowerCase()),
    ]),
  );
  for (const file of Object.values(files))
    writeFileSync(file, "", {
      flag:
        constants.O_WRONLY |
        constants.O_CREAT |
        constants.O_TRUNC |
        constants.O_NOFOLLOW,
      mode: 0o600,
    });
  const paths = pathCommands(state.paths.join("\n"));
  const persisted = validateEnvironment(state.env);
  const childEnv = {
    ...process.env,
    ...persisted,
    ...env,
    HOME: join(root, "home"),
    PATH: [...paths, process.env.PATH].join(":"),
    RUNNER_TEMP: join(root, "temp"),
    RUNNER_TOOL_CACHE: join(root, "tools"),
    GITHUB_WORKSPACE: workspace,
    CI: "true",
    GITHUB_ACTIONS: "true",
    RUNNER_OS: "Linux",
    RUNNER_ARCH: process.arch === "arm64" ? "ARM64" : "X64",
  };
  for (const [key, file] of Object.entries(files))
    childEnv[`GITHUB_${key}`] = file;
  mkdirSync(childEnv.RUNNER_TEMP, { recursive: true });
  mkdirSync(childEnv.HOME, { recursive: true });
  mkdirSync(childEnv.RUNNER_TOOL_CACHE, { recursive: true });
  command = expandWorkflow(command, state.outputs, childEnv);
  const args =
    shell === "bash"
      ? ["--noprofile", "--norc", "-eo", "pipefail", "-c", command]
      : ["-e", "-c", command];
  const masks = state.masks ?? [];
  if (
    !Array.isArray(masks) ||
    masks.length > 100 ||
    masks.some((value) => typeof value !== "string" || value.length > 4096)
  )
    throw new Error("Invalid workflow masks");
  const code = await new Promise((resolve, reject) => {
    const child = spawn(`/bin/${shell}`, args, {
      env: childEnv,
      stdio: ["ignore", "pipe", "pipe"],
    });
    child.on("error", reject);
    const drains = Promise.all([
      drain(child.stdout, process.stdout, masks),
      drain(child.stderr, process.stderr, masks),
    ]);
    drains.catch((error) => {
      child.kill("SIGKILL");
      reject(error);
    });
    child.on("close", async (code) => {
      try {
        await drains;
        resolve(code ?? 1);
      } catch (error) {
        reject(error);
      }
    });
  });
  state.masks = masks;
  Object.assign(state.env, environmentCommands(readBounded(files.ENV)));
  state.paths = [
    ...new Set([...pathCommands(readBounded(files.PATH)).reverse(), ...paths]),
  ];
  const outputs = parseCommands(readBounded(files.OUTPUT));
  if (id) state.outputs[id] = outputs;
  if (outputKeys && process.env.CO_ACTION_OUTPUT && code === 0)
    writeFileSync(
      process.env.CO_ACTION_OUTPUT,
      JSON.stringify(
        Object.fromEntries(
          Object.entries(outputs).filter(
            ([key, value]) =>
              outputKeys.includes(key) &&
              !masks.some(
                (mask) =>
                  value.includes(mask) ||
                  value
                    .toLowerCase()
                    .includes(encodeURIComponent(mask).toLowerCase()),
              ),
          ),
        ),
      ),
    );
  const serialized = JSON.stringify(state);
  if (Buffer.byteLength(serialized) > maxBytes)
    throw new Error("Workflow state exceeds 64 KiB");
  writeFileSync(statePath, serialized, {
    flag:
      constants.O_WRONLY |
      constants.O_CREAT |
      constants.O_TRUNC |
      constants.O_NOFOLLOW,
    mode: 0o600,
  });
  return code;
}

async function drain(stream, target, masks) {
  const decoder = new StringDecoder("utf8");
  let buffered = "";
  const line = (value) => {
    if (value.startsWith("::add-mask::")) {
      const secret = value
        .slice(12)
        .replace(/%0D/gi, "\r")
        .replace(/%0A/gi, "\n")
        .replace(/%25/gi, "%")
        .trimEnd();
      for (const part of secret.split(/\s+/).filter(Boolean)) {
        if (part.length > 4096 || masks.length >= 100)
          throw new Error("Workflow mask limit exceeded");
        if (!masks.includes(part)) masks.push(part);
      }
      return;
    }
    for (const mask of [...masks].sort((a, b) => b.length - a.length))
      value = value.split(mask).join("***");
    target.write(value);
  };
  for await (const chunk of stream) {
    buffered += decoder.write(chunk);
    let newline;
    while ((newline = buffered.indexOf("\n")) !== -1) {
      if (newline > maxBytes)
        throw new Error("Workflow log line exceeds 64 KiB");
      line(buffered.slice(0, newline + 1));
      buffered = buffered.slice(newline + 1);
    }
    if (buffered.length > maxBytes)
      throw new Error("Workflow log line exceeds 64 KiB");
  }
  line(buffered + decoder.end());
}

export function expandWorkflow(value, outputs, env) {
  const hashed = value.replace(
    /\$\{\{\s*hashFiles\(([^)]*)\)\s*\}\}/g,
    (_, arguments_) => {
      if (!/^\s*'[^']+'(?:\s*,\s*'[^']+')*\s*$/.test(arguments_))
        throw new Error("hashFiles requires literal path patterns");
      return hashFiles(
        [...arguments_.matchAll(/'([^']+)'/g)].map((match) => match[1]),
        env.GITHUB_WORKSPACE,
      );
    },
  );
  const result = hashed.replace(
    /\$\{\{\s*(steps\.([A-Za-z_][A-Za-z0-9_-]*)\.outputs\.([A-Za-z_][A-Za-z0-9_-]*)|env\.([A-Za-z_][A-Za-z0-9_]*)|github\.(workspace|sha)|runner\.(os|arch))\s*\}\}/g,
    (_, expression, step, output, variable, github, runner) =>
      step
        ? (outputs[step]?.[output] ?? "")
        : variable
          ? (env[variable] ?? "")
          : (env[
              `${runner ? "RUNNER" : "GITHUB"}_${(runner ?? github).toUpperCase()}`
            ] ?? ""),
  );
  if (result.includes("${{"))
    throw new Error("Unsupported workflow expression");
  return result;
}

export function hashFiles(patterns, workspace = process.cwd()) {
  if (patterns.length > 16) throw new Error("Too many hashFiles patterns");
  const files = new Set();
  let visited = 0;
  const root = resolve(workspace);
  for (const raw of patterns) {
    const pattern = raw.replace(/^\.\//, "");
    if (
      !pattern ||
      pattern.startsWith("/") ||
      pattern
        .split("/")
        .some(
          (part) => !part || part === ".." || part.toLowerCase() === ".git",
        ) ||
      /[\\\x00-\x1f\[\]{}!]/.test(pattern)
    )
      throw new Error("Unsupported hashFiles path pattern");
    let expression = "^";
    for (let i = 0; i < pattern.length; i++) {
      if (pattern.slice(i, i + 3) === "**/") {
        expression += "(?:.*/)?";
        i += 2;
      } else if (pattern.slice(i, i + 2) === "**") {
        expression += ".*";
        i++;
      } else if (pattern[i] === "*") expression += "[^/]*";
      else if (pattern[i] === "?") expression += "[^/]";
      else expression += pattern[i].replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    }
    const matcher = new RegExp(`${expression}$`);
    const add = (path) => {
      if (++visited > 8192) throw new Error("hashFiles exceeds entry limit");
      const metadata = lstatSync(path);
      if (metadata.isSymbolicLink()) {
        if (matcher.test(relative(root, path).split(sep).join("/")))
          throw new Error("hashFiles does not follow symbolic links");
        return;
      }
      if (
        metadata.isFile() &&
        matcher.test(relative(root, path).split(sep).join("/"))
      )
        files.add(path);
      else if (metadata.isDirectory())
        for (const entry of readdirSync(path))
          if (entry !== ".git") add(join(path, entry));
    };
    if (!/[*?]/.test(pattern)) {
      let parent = root;
      for (const part of pattern.split("/").slice(0, -1)) {
        parent = join(parent, part);
        try {
          if (lstatSync(parent).isSymbolicLink())
            throw new Error("hashFiles does not follow symbolic links");
        } catch (error) {
          if (error.code !== "ENOENT") throw error;
        }
      }
      try {
        add(join(root, pattern));
      } catch (error) {
        if (error.code !== "ENOENT") throw error;
      }
    } else add(root);
  }
  let total = 0;
  const combined = createHash("sha256");
  for (const path of [...files].sort()) {
    const fd = openSync(
      path,
      constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK,
    );
    try {
      const metadata = fstatSync(fd);
      if (!metadata.isFile())
        throw new Error("hashFiles requires regular files");
      const size = metadata.size;
      total += size;
      if (total > 16 * 1024 * 1024) throw new Error("hashFiles exceeds 16 MiB");
      const bytes = Buffer.alloc(size);
      let offset = 0;
      while (offset < size) {
        const count = readSync(fd, bytes, offset, size - offset, null);
        if (!count) throw new Error("hashFiles input changed");
        offset += count;
      }
      combined.update(createHash("sha256").update(bytes).digest());
    } finally {
      closeSync(fd);
    }
  }
  return files.size ? combined.digest("hex") : "";
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  try {
    const payload = JSON.parse(readBounded(process.argv[2]));
    process.exitCode = await runStep({ ...payload, root: "/run/co-job" });
  } catch (error) {
    console.error(`Workflow step failed: ${error.message}`);
    process.exitCode = 1;
  }
}
