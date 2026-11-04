package cli

import "embed"

//go:embed runtime/command_files.mjs runtime/action_runtime.mjs runtime/cache_runtime.mjs
var runnerRuntime embed.FS
