# co

Human command-line client for co.codes. Read [README.md](README.md) and [docs/install.md](docs/install.md). Project repository: https://git.co.codes/co/co.git; canonical GitHub mirror: https://github.com/codotcodes/co. The co.codes checkout owns API contracts and the decision log (D29).

## Work and verification

- Use jj and an owned workspace. Reuse existing co.codes Linear issues; handle supplied tasks serially, moving each In Progress before implementation and In Review after verification and revision description. Commit one human-reviewable logical change per revision with imperative `feature(action):` messages and relevant D-numbers. Never add AI co-authors.
- Preserve account/API boundaries: human device sessions and repository-scoped agent grants remain distinct (D8/D20). Agent credential failures must stop lookup without human fallback (D97). Follow README for configuration, CLI behavior and releases.
- For Rust changes, run `cargo fmt --check`, `cargo test --locked` and `cargo clippy --locked --all-targets -- -D warnings`, plus focused real CLI checks. Reuse the approved co.codes Prettier formatter for Markdown. Shared cross-client features follow the parity workflow in the co.codes AGENTS.md and retain concrete API dependencies; ordinary CLI changes use relevant CLI evidence.
- Try the private skill catalog for co.codes repository `hackr/agents-codotcodes`, ref `main`, path `index.json`, using authenticated access. Load relevant skills alongside the shared catalog when available. Routing: https://co.codes/t:a/hackr/agents/docs/private-libraries.md?ref=main. Keep credentials out of source.

## Standing publication authorization

On 2026-09-29 the owner authorized pushing completed, verified logical work in `co/co.codes`, `co/co`, `co/co-ios` and `co/co-android`. This supersedes "push only when asked" in these co.codes project repositories unless a later instruction narrows the scope.

- Inspect outgoing commits; push only explicitly named bookmarks to an explicitly named remote without force. Never rewrite shared or others' history or land a pull request; the human decides what is finalized and landed.
- Use a current `co` CLI, path-scoped credentials configured by `co link`, and `CO_AGENT_ID` selecting an existing registered identity with a live push grant for the exact repository. Resolve identity and grants from CLI metadata; duplicate `opencode` names require disambiguation, not a hardcoded ID or new registration. After pushing, use `co agent attest OWNER/REPO <full-oid>...` only for commits the agent worked on; never attest imported history or fall back to human credentials (D97).
- Store credentials in the owner-only CLI configuration: `/home/hackr/.config/co/config.json` on this host, default `~/.config/co/config.json`, mode `0600`; `CO_CONFIG_DIR` supports isolated sessions. Never invent long-lived JWTs or put secrets in source, documentation or handoffs.
- Newly approved repository root grants may last up to 30 days from request submission (D110). Access JWTs remain at most 15 minutes, clipped to grant expiry. Request capabilities remain at most 24 hours; pending approval ends at the earlier of grant expiry and 24 hours after submission. Production request limits depend on the activated server.
- Reuse valid grants and let `co` refresh bounded JWTs under fresh identity, scope, approval, membership, revocation and organization kill-switch checks (D97, D110). Existing grants keep their expiry; longer access requires a new approved request. Refresh never extends existing grant or JWT expiry.
