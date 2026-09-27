#!/bin/sh
# SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
# SPDX-License-Identifier: Apache-2.0
#
# Installs a release binary of The Rabbit Hole on Linux or macOS, and offers a
# starter config in ./rabbithole/.
#
#   curl -fsSL https://raw.githubusercontent.com/DanielBlei/rabbithole/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --help
#
# The archive is checked against the release's checksums.txt before anything is
# installed. Nothing needs root, and no model or other software is installed.
#
# POSIX sh on purpose: it runs piped from curl under dash, bash and macOS sh.
# The body is one function called on the last line, so a download cut short
# runs nothing.

set -eu

REPO="DanielBlei/rabbithole"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh [--version TAG] [--dir PATH] [--yes | --no-config]

  --version TAG   release to install, e.g. v0.2.0 (default: the latest)
  --dir PATH      where the binary goes (default: ~/.local/bin)
  --yes           create the starter config in ./rabbithole without asking
  --no-config     skip the starter config

Environment: RABBITHOLE_VERSION and RABBITHOLE_INSTALL_DIR set the same as the
flags; RABBITHOLE_BASE_URL replaces the download location (a mirror, or a local
test build).
EOF
	exit 0
}

# The prompt reads from the terminal, because stdin is the script itself when
# it is piped from curl.
ask() {
	if (: </dev/tty) 2>/dev/null; then
		printf '%s [Y/n] ' "$1" >/dev/tty
		read -r reply </dev/tty || reply="n"
		case "$reply" in
		"" | y | Y | yes | YES) return 0 ;;
		*) return 1 ;;
		esac
	fi
	say "No terminal to ask; skipped the starter config (rerun with --yes to create it)."
	return 1
}

# Quote a value for YAML (single quotes, ' doubled), then escape it for the
# replacement side of a sed s|||.
yaml_sed() {
	quoted="'$(printf '%s' "$1" | sed -e "s/'/''/g")'"
	printf '%s' "$quoted" | sed -e 's/[\\|&]/\\&/g'
}

# Reads a top-level-indented scalar such as `  model: qwen3.5:4b  # comment`.
config_value() {
	sed -n "s/^  $1: *\\([^ #]*\\).*/\\1/p" "$2" | head -n 1
}

main() {
	VERSION="${RABBITHOLE_VERSION:-}"
	INSTALL_DIR="${RABBITHOLE_INSTALL_DIR:-}"
	BASE_URL="${RABBITHOLE_BASE_URL:-}"
	CONFIG="ask"

	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || fail "--version needs a tag, e.g. v0.2.0"
			VERSION="$2"
			shift 2
			;;
		--dir)
			[ $# -ge 2 ] || fail "--dir needs a path"
			INSTALL_DIR="$2"
			shift 2
			;;
		--yes) CONFIG="yes" && shift ;;
		--no-config) CONFIG="no" && shift ;;
		-h | --help) usage ;;
		*) fail "unknown option $1 (try --help)" ;;
		esac
	done

	if [ -z "$INSTALL_DIR" ]; then
		[ -n "${HOME:-}" ] || fail "HOME is not set; pass --dir"
		INSTALL_DIR="$HOME/.local/bin"
	fi

	command -v curl >/dev/null 2>&1 || fail "curl is required"
	command -v tar >/dev/null 2>&1 || fail "tar is required"

	# The release names archives after GOOS/GOARCH; map uname onto those.
	case "$(uname -s)" in
	Linux) OS="linux" ;;
	Darwin) OS="darwin" ;;
	*) fail "no prebuilt binary for $(uname -s); build from source: https://github.com/${REPO}#from-source" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) ARCH="amd64" ;;
	arm64 | aarch64) ARCH="arm64" ;;
	*) fail "no prebuilt binary for $(uname -m); build from source: https://github.com/${REPO}#from-source" ;;
	esac

	# The /releases/latest page redirects to the newest tag, which avoids the
	# API and its rate limit.
	if [ -z "$VERSION" ]; then
		[ -z "$BASE_URL" ] || fail "RABBITHOLE_BASE_URL is set; pass --version too"
		latest="$(curl -fsSLI --retry 3 -o /dev/null -w '%{url_effective}' \
			"https://github.com/${REPO}/releases/latest")" ||
			fail "could not look up the latest release"
		VERSION="${latest##*/}"
		case "$VERSION" in
		v*) ;;
		*) fail "no release published yet" ;;
		esac
	else
		case "$VERSION" in
		v*) ;;
		*) VERSION="v${VERSION}" ;;
		esac
	fi
	BASE_URL="${BASE_URL:-https://github.com/${REPO}/releases/download}"

	ARCHIVE="rabbithole_${VERSION#v}_${OS}_${ARCH}.tar.gz"

	TMP="$(mktemp -d 2>/dev/null || mktemp -d -t rabbithole)"
	# EXIT cleans up; INT and TERM must exit too, or sh carries on after them.
	trap 'rm -rf "$TMP"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	say "Downloading The Rabbit Hole ${VERSION} for ${OS}/${ARCH}"
	curl -fsSL --retry 3 -o "$TMP/$ARCHIVE" "${BASE_URL}/${VERSION}/${ARCHIVE}" ||
		fail "${VERSION} has no ${OS}/${ARCH} binary (prebuilt binaries start at v0.2.0); build from source: https://github.com/${REPO}#from-source"
	curl -fsSL --retry 3 -o "$TMP/checksums.txt" "${BASE_URL}/${VERSION}/checksums.txt" ||
		fail "could not download checksums.txt for ${VERSION}"

	# Only this archive's line, so the check does not trip over the others.
	awk -v f="$ARCHIVE" '$2 == f' "$TMP/checksums.txt" >"$TMP/sum.txt"
	[ -s "$TMP/sum.txt" ] || fail "checksums.txt has no entry for ${ARCHIVE}"
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$TMP" && sha256sum -c sum.txt >/dev/null 2>&1) ||
			fail "checksum mismatch for ${ARCHIVE}; not installing"
	elif command -v shasum >/dev/null 2>&1; then
		(cd "$TMP" && shasum -a 256 -c sum.txt >/dev/null 2>&1) ||
			fail "checksum mismatch for ${ARCHIVE}; not installing"
	else
		fail "need sha256sum or shasum to verify the download"
	fi
	say "Checksum verified"

	tar -xzf "$TMP/$ARCHIVE" -C "$TMP" || fail "could not unpack ${ARCHIVE}"
	[ -f "$TMP/rabbithole" ] || fail "the archive holds no rabbithole binary"

	{ mkdir -p "$INSTALL_DIR" && [ -w "$INSTALL_DIR" ]; } ||
		fail "cannot write to ${INSTALL_DIR}; pass --dir a folder you own (no need for sudo)"
	INSTALL_DIR="$(cd "$INSTALL_DIR" && pwd)"
	if [ -x "$INSTALL_DIR/rabbithole" ]; then
		old="$("$INSTALL_DIR/rabbithole" --version 2>/dev/null || echo "an unknown version")"
		say "Replacing ${old}"
	fi
	# Copy then rename: a new file, so a running or signed binary is never
	# overwritten in place.
	if ! { cp "$TMP/rabbithole" "$INSTALL_DIR/.rabbithole.new" &&
		chmod 0755 "$INSTALL_DIR/.rabbithole.new" &&
		mv -f "$INSTALL_DIR/.rabbithole.new" "$INSTALL_DIR/rabbithole"; }; then
		rm -f "$INSTALL_DIR/.rabbithole.new"
		fail "could not install into ${INSTALL_DIR}"
	fi
	say "Installed $("$INSTALL_DIR/rabbithole" --version 2>/dev/null || echo rabbithole) to ${INSTALL_DIR}"

	# Off PATH, the commands printed below use the full path so they work as
	# pasted.
	BIN="rabbithole"
	case ":${PATH}:" in
	*":${INSTALL_DIR}:"*) ;;
	*)
		BIN="${INSTALL_DIR}/rabbithole"
		say ""
		say "${INSTALL_DIR} is not on your PATH. Add this line to your shell profile"
		say "(~/.zshrc on macOS, ~/.bashrc or ~/.profile on Linux):"
		say "  export PATH=\"${INSTALL_DIR}:\$PATH\""
		;;
	esac

	TARGET="${PWD%/}/rabbithole"
	WROTE_CONFIG=""
	if [ "$CONFIG" != "no" ]; then
		say ""
		if [ ! -f "$TMP/configs/config.example.yaml" ]; then
			say "This release ships no example config; skipped the starter config."
		elif [ "$CONFIG" = "yes" ] ||
			ask "Create a rabbithole/ folder here (${TARGET}) with a starter config?"; then
			mkdir -p "$TARGET" ||
				fail "cannot create ${TARGET}; run from a folder you can write to, or pass --no-config"
			if [ -e "$TARGET/config.yaml" ]; then
				say "Kept the existing ${TARGET}/config.yaml"
			else
				# Relative paths resolve against wherever serve runs, so the
				# two written here are made absolute and serve works from
				# anywhere.
				sed -e '1s|.*|# The Rabbit Hole configuration, written by install.sh.|' \
					-e "s|feeds: ./configs/feeds.example.yaml|feeds: $(yaml_sed "$TARGET/feeds.yaml")|" \
					-e "s|db_path: ./data/rabbithole.db|db_path: $(yaml_sed "$TARGET/data/rabbithole.db")|" \
					"$TMP/configs/config.example.yaml" >"$TARGET/config.yaml"
				grep -q "^  db_path: '/" "$TARGET/config.yaml" || {
					rm -f "$TARGET/config.yaml"
					fail "could not point db_path at ${TARGET}/data"
				}
				say "Created ${TARGET}/config.yaml"
			fi
			if [ -e "$TARGET/feeds.yaml" ]; then
				say "Kept the existing ${TARGET}/feeds.yaml"
			else
				cp "$TMP/configs/feeds.example.yaml" "$TARGET/feeds.yaml"
				say "Created ${TARGET}/feeds.yaml"
			fi
			WROTE_CONFIG="$TARGET/config.yaml"
		fi
	fi

	say ""
	if [ -n "$WROTE_CONFIG" ]; then
		# Read from the config in place, which may be one kept from earlier.
		provider="$(config_value provider "$WROTE_CONFIG")"
		model="$(config_value model "$WROTE_CONFIG")"
		if [ "${provider:-ollama}" = "ollama" ]; then
			say "The config scores with Ollama, using the model ${model:-qwen3.5:4b}."
			if command -v ollama >/dev/null 2>&1; then
				say "Ollama is installed. Pull the model once before the first ingest:"
				say "  ollama pull ${model:-qwen3.5:4b}"
			else
				say "Ollama was not found. Either:"
				say "  - install it from https://ollama.com, then run: ollama pull ${model:-qwen3.5:4b}"
				say "  - point inference at vLLM or another OpenAI-compatible endpoint in ${WROTE_CONFIG}"
				say "    (https://github.com/${REPO}/blob/main/docs/configuration.md)"
				say "  - or, as a last resort, set provider: heuristic; it needs no model but ranks far less well"
			fi
		fi
		say ""
		say "Start it with:"
		say "  ${BIN} serve --config ${WROTE_CONFIG}"
		say "then open http://localhost:8080"
	else
		if ! command -v ollama >/dev/null 2>&1; then
			say "Ollama was not found on PATH; the default config expects it (https://ollama.com)."
		fi
		say "${BIN} needs a config to start; see https://github.com/${REPO}#release-binary"
	fi
}

main "$@"
