---
name: release
description: >-
  Cut a Jevonian GitHub release: bump package.json version, write CHANGELOG,
  commit, tag, and push. Use when the user asks to release, ship a version,
  bump version, write a changelog, cut a tag, or prepare npm publishing.
  Never npm publish unless the user explicitly asks in the same turn.
---

# Release

GitHub release only by default. **npm publish is a separate, explicit step.**

## Hard rules

- Never `npm publish` / `pnpm publish` unless the user said so in this turn.
- Never push `--force` to `main` or move an existing release tag.
- Never skip quality gates unless the user waives them for this release.
- Version in `package.json`, `internal/cli/cli.go` `Version`, git tag `vX.Y.Z`, and CHANGELOG heading must match.

## Workflow

Copy and track:

```
Release Progress:
- [ ] 1. Preconditions
- [ ] 2. Choose version
- [ ] 3. Quality gates
- [ ] 4. CHANGELOG + package.json
- [ ] 5. Commit, tag, push
- [ ] 6. GitHub Release
- [ ] 7. Remind publish checklist (do not publish yet)
```

### 1. Preconditions

Run in parallel:

```bash
git status
git branch -vv
git log --oneline -15
git tag -l 'v*' --sort=-v:refname | head -10
```

Done when:

- On `main` (or user named another release branch).
- Working tree clean, or only intentional release edits.
- `git fetch origin` then local `main` is not behind `origin/main`.

If behind: pull/rebase first. If unrelated dirty files: stop and ask.

### 2. Choose version

Read current `version` from `package.json`.

If the user gave an exact version, use it. Otherwise ask: **patch / minor / major** (semver).

Default suggestion when unclear:

- Fixes / docs / chore → patch
- New user-facing capability → minor
- Breaking CLI/API/config → major

Target tag is always `v` + version (e.g. `0.1.0` → `v0.1.0`).

### 3. Quality gates

From repo root, sequentially:

```bash
pnpm exec vp check
go test ./...
node --test test/*.test.js
pnpm build
pnpm smoke
```

Done when all exit 0. On failure: fix or stop; do not tag a red release.

### 4. CHANGELOG + package.json

Create `CHANGELOG.md` if missing. Prepend a section (Keep a Changelog style):

```markdown
## [X.Y.Z] - YYYY-MM-DD

### Added

- …

### Changed

- …

### Fixed

- …
```

Omit empty subsections. Derive bullets from commits/PRs since the previous `v*` tag (or all history for the first tagged release). Write for users, not commit hashes.

Set `package.json` `"version"` to `X.Y.Z` (no `v` prefix). Set `var Version` in
`internal/cli/cli.go` to the same value; `TestVersionMatchesPackageJSON` fails when they differ.

### 5. Commit, tag, push

Follow the repo git commit rules (status/diff/log, HEREDOC message, no secrets).

```bash
git add package.json CHANGELOG.md internal/cli/cli.go
# include any gate fixes from this release only
git commit -m "$(cat <<'EOF'
Release vX.Y.Z.

<summary of why this version ships>
EOF
)"
git tag -a "vX.Y.Z" -m "vX.Y.Z"
git push origin HEAD
git push origin "vX.Y.Z"
```

Prefer SSH remote if HTTPS OAuth lacks `workflow` scope (known for this repo).

Done when `git status` is clean and `origin` has the commit + tag.

### 6. GitHub Release

Build the native assets and upload them with the release. The npm shim and `jevonian update`
both download `jevonian-<os>-<arch>[.exe]` plus `checksums.txt` from the `vX.Y.Z` release, so
the assets are the release.

```bash
# CGO-free binaries for every supported platform. The version comes from
# package.json and is stamped into the binary; `jevonian version` and the
# update check read it, so a wrong stamp means a never-ending "update available".
version="$(node -p 'require("./package.json").version')"
mkdir -p /tmp/jevonian-release
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/amd64; do
  os="${target%/*}"; arch="${target#*/}"
  out="/tmp/jevonian-release/jevonian-$os-$arch"
  [ "$os" = "windows" ] && out="$out.exe"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/xinyao27/jevonian/internal/cli.Version=$version" \
    -o "$out" ./cmd/jevonian
done
# Every binary must report the package version.
[ "$(/tmp/jevonian-release/jevonian-$(go env GOOS)-$(go env GOARCH) version)" = "jevonian $version" ]
( cd /tmp/jevonian-release && shasum -a 256 jevonian-* | sed 's|  *|  |' > checksums.txt )
```

```bash
gh release create "vX.Y.Z" --title "vX.Y.Z" --notes-file - /tmp/jevonian-release/* <<'EOF'
## What's changed
<same bullets as CHANGELOG section, tightened>
EOF
```

Done when `gh release view vX.Y.Z` works and every asset in `checksums.txt` is attached. Return the release URL.

### 7. Stop and remind

Do **not** publish. Print the [publish checklist](publish-checklist.md) and ask whether to run **npm publish** now.

## Explicit npm publish branch

Only when the user asks to publish in this turn: follow [publish-checklist.md](publish-checklist.md) end-to-end, then publish from a clean checkout at the release tag.
