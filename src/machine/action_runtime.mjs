import {
  appendFileSync,
  chmodSync,
  existsSync,
  mkdirSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve, sep } from "node:path";
import { createHash } from "node:crypto";
import { gunzipSync } from "node:zlib";
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { expandWorkflow, readBounded, runStep } from "./command_files.mjs";

const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");
const arch = process.arch === "arm64" ? "arm64" : "x64";
const exec = (program, args, options = {}) => {
  const result = spawnSync(program, args, { stdio: "inherit", ...options });
  if (result.error || result.status !== 0)
    throw new Error(`Command failed: ${program}`);
};

export async function download(url, limit = 64 * 1024 * 1024) {
  if (!url.startsWith("https://")) throw new Error("Downloads require HTTPS");
  const response = await fetch(url, {
    signal: AbortSignal.timeout(120000),
    headers: { "User-Agent": "co-codes-runner" },
  });
  if (!response.ok || !response.body || !response.url.startsWith("https://"))
    throw new Error(`Download failed (${response.status})`);
  const chunks = [];
  let size = 0;
  for await (const chunk of response.body) {
    size += chunk.length;
    if (size > limit) {
      await response.body.cancel().catch(() => {});
      throw new Error("Download exceeds limit");
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

export function extractTar(compressed, directory, allowLinks = false) {
  const bytes = gunzipSync(compressed, {
    maxOutputLength: (allowLinks ? 384 : 128) * 1024 * 1024,
  });
  let count = 0;
  let longName;
  let paxPath;
  for (let offset = 0; offset + 512 <= bytes.length;) {
    const header = bytes.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) break;
    const text = (start, end) =>
      header.subarray(start, end).toString("utf8").split("\0")[0];
    const size = Number.parseInt(text(124, 136).trim() || "0", 8);
    if (
      !Number.isSafeInteger(size) ||
      size < 0 ||
      offset + 512 + size > bytes.length
    )
      throw new Error("Invalid action archive size");
    const content = bytes.subarray(offset + 512, offset + 512 + size);
    const type = text(156, 157);
    offset += 512 + Math.ceil(size / 512) * 512;
    if (type === "g") continue;
    if (type === "x") {
      paxPath = content
        .toString("utf8")
        .split("\n")
        .map((line) => /^\d+ path=(.*)$/.exec(line)?.[1])
        .find(Boolean);
      continue;
    }
    if (type === "L") {
      longName = content.toString("utf8").split("\0")[0];
      continue;
    }
    const name =
      paxPath ??
      longName ??
      [text(345, 500), text(0, 100)].filter(Boolean).join("/");
    longName = undefined;
    paxPath = undefined;
    const parts = name.replace(/\/$/, "").split("/");
    if (
      parts.some(
        (part) => !part || part === "." || part === ".." || /[\\\0]/.test(part),
      ) ||
      name.startsWith("/") ||
      ++count > (allowLinks ? 32768 : 4096)
    )
      throw new Error("Unsafe action archive path");
    const relative = parts.slice(1).join("/");
    if (!relative) continue;
    const target = join(directory, relative);
    if (type === "5") mkdirSync(target, { recursive: true });
    else if (type === "0" || type === "") {
      mkdirSync(dirname(target), { recursive: true });
      writeFileSync(target, content, {
        flag: "wx",
        mode: Number.parseInt(text(100, 108), 8) & 0o111 ? 0o755 : 0o644,
      });
    } else if (type === "2" && allowLinks) {
      const link = text(157, 257);
      const destination = resolve(dirname(target), link);
      if (!destination.startsWith(`${resolve(directory)}${sep}`))
        throw new Error("Unsafe tool archive link");
      mkdirSync(dirname(target), { recursive: true });
      symlinkSync(link, target);
    } else
      throw new Error(
        "Action archives must contain only regular files and directories",
      );
  }
}

const json = async (url) =>
  JSON.parse((await download(url, 2 * 1024 * 1024)).toString("utf8"));

export function selectNode(releases, requested) {
  const request = requested.replace(/^v/, "");
  const match = (release) =>
    request === "latest" ||
    request === "node" ||
    (request === "lts/*" && release.lts) ||
    (request.startsWith("lts/") &&
      String(release.lts).toLowerCase() === request.slice(4).toLowerCase()) ||
    (/^\d+(?:\.\d+){0,2}$/.test(request) &&
      (release.version.slice(1) === request ||
        release.version.slice(1).startsWith(`${request}.`)));
  const found = releases.find(
    (release) => match(release) && release.files.includes(`linux-${arch}`),
  );
  if (!found) throw new Error(`Unsupported Node version: ${requested}`);
  return found.version.slice(1);
}

export function validToolVersion(tool, requested) {
  if (typeof requested !== "string" || requested.length > 64) return false;
  if (tool === "node")
    return /^(?:v?\d+(?:\.\d+){0,2}|latest|node|lts\/(?:\*|[A-Za-z]+))$/.test(
      requested,
    );
  return (
    requested === "latest" ||
    /^v?\d+\.\d+\.\d+(?:-[A-Za-z0-9.]+)?$/.test(requested)
  );
}

async function installTool(tool, requested, publish = true) {
  if (!validToolVersion(tool, requested))
    throw new Error("Invalid tool version");
  const root = process.env.RUNNER_TOOL_CACHE;
  if (!root) throw new Error("Missing job-local tool cache");
  if (
    tool === "node" &&
    !publish &&
    process.versions.node.split(".")[0] === requested
  )
    return process.execPath;
  let version, url, checksum, binary;
  if (tool === "node") {
    version = selectNode(
      await json("https://nodejs.org/dist/index.json"),
      requested,
    );
    const name = `node-v${version}-linux-${arch}.tar.gz`;
    url = `https://nodejs.org/dist/v${version}/${name}`;
    const sums = (
      await download(
        `https://nodejs.org/dist/v${version}/SHASUMS256.txt`,
        65536,
      )
    ).toString("utf8");
    checksum = sums
      .split("\n")
      .find((line) => line.endsWith(`  ${name}`))
      ?.slice(0, 64);
    binary = "bin/node";
  } else {
    const repo = tool === "bun" ? "oven-sh/bun" : "opentofu/opentofu";
    const tag =
      tool === "bun"
        ? `bun-v${requested.replace(/^v/, "")}`
        : `v${requested.replace(/^v/, "")}`;
    const release = await json(
      `https://api.github.com/repos/${repo}/releases/${requested === "latest" ? "latest" : `tags/${tag}`}`,
    );
    version = release.tag_name.replace(/^(?:bun-)?v/, "");
    const name =
      tool === "bun"
        ? `bun-linux-${arch}.zip`
        : `tofu_${version}_linux_${arch === "x64" ? "amd64" : arch}.zip`;
    const asset = release.assets.find((asset) => asset.name === name);
    url = asset?.browser_download_url;
    checksum = asset?.digest?.replace(/^sha256:/, "");
    binary = tool === "bun" ? `bun-linux-${arch}/bun` : "tofu";
  }
  if (
    !/^\d+\.\d+\.\d+(?:[-.][A-Za-z0-9.-]+)?$/.test(version) ||
    !url ||
    !/^[a-f0-9]{64}$/.test(checksum ?? "")
  )
    throw new Error("Tool release lacks a verified SHA-256 asset");
  const directory = join(root, tool, version, arch);
  const executable = join(directory, binary);
  if (!existsSync(executable)) {
    const bytes = await download(url, 128 * 1024 * 1024);
    if (digest(bytes) !== checksum)
      throw new Error("Tool download checksum mismatch");
    mkdirSync(directory, { recursive: true });
    if (tool === "node") extractTar(bytes, directory, true);
    else {
      const archive = join(directory, "tool.zip");
      writeFileSync(archive, bytes);
      exec("unzip", ["-q", archive, "-d", directory]);
    }
    chmodSync(executable, 0o755);
  }
  if (publish) {
    appendFileSync(process.env.GITHUB_PATH, `${dirname(executable)}\n`);
    appendFileSync(process.env.GITHUB_OUTPUT, `${tool}-version=${version}\n`);
  }
  return executable;
}

export function expand(value, inputs, env = process.env, outputs = {}) {
  if (typeof value !== "string")
    throw new Error("Action values must be strings");
  const result = value.replace(
    /\$\{\{\s*(inputs\.([A-Za-z_][A-Za-z0-9_-]*)|env\.([A-Za-z_][A-Za-z0-9_]*)|github\.action_path)\s*\}\}/g,
    (_, expression, input, variable) =>
      input
        ? (inputs[input] ?? "")
        : variable
          ? (env[variable] ?? "")
          : env.GITHUB_ACTION_PATH,
  );
  return expandWorkflow(result, outputs, env);
}

export async function runAction(plan) {
  const state = JSON.parse(
    readBounded(join(dirname(process.env.RUNNER_TOOL_CACHE), "state.json")) ||
      '{"outputs":{}}',
  );
  const inputs = Object.fromEntries(
    Object.entries(plan.inputs).map(([key, value]) => [
      key,
      expandWorkflow(value, state.outputs, process.env),
    ]),
  );
  if (plan.adapter === "actions/checkout") {
    if (inputs.clean !== "false") {
      exec("git", ["reset", "--hard", process.env.GITHUB_SHA]);
      exec("git", ["clean", "-ffdx"]);
    }
    const current = spawnSync("git", ["rev-parse", "HEAD"], {
      encoding: "utf8",
    });
    if (
      current.status !== 0 ||
      current.stdout.trim() !== process.env.GITHUB_SHA
    )
      throw new Error("Checkout does not match the assigned commit");
    console.log(
      "Using the exact assigned checkout; no credentials are persisted.",
    );
    return;
  }
  if (plan.adapter === "actions/setup-node") {
    await installTool("node", inputs["node-version"] || "lts/*");
    return;
  }
  if (plan.adapter === "oven-sh/setup-bun") {
    await installTool("bun", inputs["bun-version"] || "latest");
    return;
  }
  if (plan.adapter === "opentofu/setup-opentofu") {
    await installTool("tofu", inputs.tofu_version || "latest");
    return;
  }
  if (
    !/^[A-Za-z0-9_-]+\/[A-Za-z0-9_.-]+$/.test(plan.repository) ||
    !/^[a-f0-9]{40}$/.test(plan.commit)
  )
    throw new Error("Invalid pinned action source");
  const directory = join(
    process.env.RUNNER_TEMP,
    `action-${digest(Buffer.from(`${plan.repository}/${plan.commit}`))}-${Date.now()}`,
  );
  mkdirSync(directory, { recursive: true });
  extractTar(
    await download(
      `https://codeload.github.com/${plan.repository}/tar.gz/${plan.commit}`,
    ),
    directory,
  );
  const action = resolve(directory, plan.path || ".");
  if (action !== directory && !action.startsWith(`${directory}${sep}`))
    throw new Error("Invalid action directory");
  const env = { ...process.env, GITHUB_ACTION_PATH: action };
  for (const [key, value] of Object.entries(inputs))
    env[`INPUT_${key.toUpperCase()}`] = value;
  if (plan.runs.using === "composite") {
    await runComposite(plan, inputs, action);
  } else {
    const node = await installTool(
      "node",
      plan.runs.using === "node20" ? "20" : "24",
      false,
    );
    const main = resolve(action, plan.runs.main);
    if (!main.startsWith(`${action}${sep}`))
      throw new Error("Invalid action entrypoint");
    exec(node, [main], { env });
  }
}

export async function runComposite(plan, inputs, action) {
  const root = join(action, "commands");
  const workspace = process.cwd();
  for (const step of plan.runs.steps) {
    const state = JSON.parse(
      readBounded(join(root, "state.json")) || '{"env":{},"outputs":{}}',
    );
    const context = {
      ...process.env,
      ...state.env,
      GITHUB_ACTION_PATH: action,
    };
    const stepEnv = { GITHUB_ACTION_PATH: action };
    for (const [key, value] of Object.entries(step.env ?? {}))
      stepEnv[key] = expand(value, inputs, context, state.outputs);
    const cwd = step["working-directory"]
      ? resolve(
          workspace,
          expand(step["working-directory"], inputs, context, state.outputs),
        )
      : workspace;
    try {
      process.chdir(cwd);
      const code = await runStep({
        root,
        command: expand(
          step.run,
          inputs,
          { ...context, ...stepEnv },
          state.outputs,
        ),
        shell: step.shell,
        id: step.id,
        env: stepEnv,
        workspace,
      });
      if (code !== 0) throw new Error("Composite action step failed");
    } finally {
      process.chdir(workspace);
    }
    for (const key of ["ENV", "PATH"])
      appendFileSync(
        process.env[`GITHUB_${key}`],
        readBounded(join(root, key.toLowerCase())),
      );
  }
  const state = JSON.parse(readBounded(join(root, "state.json")));
  for (const mask of state.masks ?? [])
    process.stdout.write(
      `::add-mask::${mask.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A")}\n`,
    );
  for (const [key, definition] of Object.entries(plan.outputs ?? {})) {
    if (typeof definition.value !== "string")
      throw new Error("Composite output requires a value");
    const value = expand(definition.value, inputs, process.env, state.outputs);
    const delimiter = `CO_${digest(Buffer.from(value))}`;
    appendFileSync(
      process.env.GITHUB_OUTPUT,
      `${key}<<${delimiter}\n${value}\n${delimiter}\n`,
    );
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  try {
    await runAction(
      JSON.parse(Buffer.from(process.argv[2], "base64").toString("utf8")),
    );
  } catch (error) {
    console.error(`Action failed: ${error.message}`);
    process.exitCode = 1;
  }
}
