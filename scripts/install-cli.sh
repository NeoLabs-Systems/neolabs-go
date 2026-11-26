#!/usr/bin/env bash
# The NeoLabs bootstrap installer: downloads an app's CLI binary for this
# computer from its newest GitHub release and runs `<command> install`. Each
# app's install.sh sets these and runs this script:
#
#   APP_NAME     product name, e.g. NeoRecall
#   APP_CMD      command and data folder name, e.g. neorecall
#   APP_REPO     owner/repository
#   APP_ENV      environment variable prefix, e.g. NEORECALL
#   APP_DATA     file under the data folder that marks an existing install
#   APP_TARGETS  space separated os-arch builds the app publishes

set -euo pipefail

: "${APP_NAME:?}" "${APP_CMD:?}" "${APP_REPO:?}" "${APP_ENV:?}" "${APP_DATA:?}" "${APP_TARGETS:?}"
SELECTOR_URL="${NEOLABS_SELECTOR_URL:-https://raw.githubusercontent.com/NeoLabs-Systems/neolabs-go/7d84f7fdafa98e04f5e9de4ad32e3dd30904d804/scripts/select-release-asset.sh}"

if [[ -t 1 ]]; then
  BOLD='\033[1m'; RESET='\033[0m'; RED='\033[1;31m'; GRN='\033[1;32m'
  CYN='\033[1;36m'; DIM='\033[2m'
else
  BOLD=''; RESET=''; RED=''; GRN=''; CYN=''; DIM=''
fi

ok()   { echo -e "  ${GRN}✓${RESET}  $*"; }
info() { echo -e "  ${CYN}→${RESET}  $*"; }
err()  { echo -e "  ${RED}✗${RESET}  $*" >&2; }
env_value() { local name="${APP_ENV}_$1"; printf '%s' "${!name:-}"; }

echo -e "${CYN}${BOLD}${APP_NAME} installer${RESET}"
echo

case "$(uname -s)" in
  Darwin) OS="macos" ;;
  Linux)  OS="linux" ;;
  *) err "This installer supports macOS and Linux. On Windows, use the desktop app."; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH="x64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "${APP_NAME} is not available for this processor ($(uname -m))."; exit 1 ;;
esac
case " ${APP_TARGETS} " in
  *" ${OS}-${ARCH} "*) ;;
  *) err "${APP_NAME} has no release build for ${OS}-${ARCH}."; exit 1 ;;
esac
command -v curl &>/dev/null || { err "curl is required."; exit 1; }

CHANNEL="$(env_value RELEASE_CHANNEL)"
if [[ -z "$CHANNEL" && -r /dev/tty && -t 1 ]]; then
  echo -ne "  ${CYN}?${RESET}  Release channel (stable/beta) ${DIM}[stable]${RESET} "
  read -r CHANNEL </dev/tty || true
fi
CHANNEL="$(echo "${CHANNEL:-stable}" | tr '[:upper:]' '[:lower:]')"
[[ "$CHANNEL" == "beta" ]] || CHANNEL="stable"
ok "Channel: ${CHANNEL}"

RUNTIME_DIR="$(env_value HOME)"
RUNTIME_DIR="${RUNTIME_DIR:-$HOME/.${APP_CMD}}"
if [[ -f "$RUNTIME_DIR/$APP_DATA" ]]; then
  info "Existing ${APP_NAME} data found at ${RUNTIME_DIR}; it is backed up and kept."
fi

info "Looking up the latest release..."
if ! ASSET="$(curl -fsSL "$SELECTOR_URL" | bash -s -- "$APP_REPO" "$CHANNEL" "${APP_CMD}-cli-${OS}-${ARCH}")" || [[ -z "$ASSET" ]]; then
  err "Could not find a published ${CHANNEL} build for ${OS}-${ARCH}. Check the network and try again."
  exit 1
fi
read -r URL DIGEST <<< "$ASSET"

BIN_DIR="$(env_value BIN_DIR)"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
mkdir -p "$BIN_DIR"
TMP="$(mktemp "${BIN_DIR}/.${APP_CMD}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT
info "Downloading ${URL##*/}..."
curl -fSL --proto '=https' --progress-bar -o "$TMP" "$URL"
if [[ "${DIGEST:-}" == sha256:* ]]; then
  if command -v sha256sum &>/dev/null; then SUM="$(sha256sum "$TMP" | awk '{print $1}')"; else SUM="$(shasum -a 256 "$TMP" | awk '{print $1}')"; fi
  if [[ "sha256:${SUM}" != "$DIGEST" ]]; then
    err "The download did not match its published checksum; nothing was installed."
    exit 1
  fi
  ok "Checksum verified"
fi
chmod 755 "$TMP"
if [[ "$OS" == "macos" ]]; then
  xattr -d com.apple.quarantine "$TMP" 2>/dev/null || true
fi
mv -f "$TMP" "$BIN_DIR/$APP_CMD"
trap - EXIT
ok "Installed ${BIN_DIR}/${APP_CMD}"

# The Node.js CLI was an npm link to bin/<command>.js; it would shadow the new
# binary on PATH. Only that link is removed, never the old checkout.
OLD_CLI="$(command -v "$APP_CMD" 2>/dev/null || true)"
if [[ -n "$OLD_CLI" && "$OLD_CLI" != "$BIN_DIR/$APP_CMD" && -L "$OLD_CLI" ]]; then
  case "$(readlink "$OLD_CLI")" in
    *"bin/${APP_CMD}.js") rm -f "$OLD_CLI"; info "Removed the Node.js ${APP_CMD} command at ${OLD_CLI}" ;;
  esac
fi

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
      if [[ -f "$rc" || "$rc" == "$HOME/.bashrc" ]] && ! grep -qs "${BIN_DIR}" "$rc"; then
        printf '\n# Added by the %s installer\nexport PATH="%s:$PATH"\n' "$APP_CMD" "$BIN_DIR" >> "$rc"
      fi
    done
    if [[ -d "$HOME/.config/fish" ]]; then
      mkdir -p "$HOME/.config/fish/conf.d"
      printf 'fish_add_path %s\n' "$BIN_DIR" > "$HOME/.config/fish/conf.d/${APP_CMD}.fish"
    fi
    export PATH="${BIN_DIR}:$PATH"
    info "Added ${BIN_DIR} to PATH (open a new terminal to use ${APP_CMD} there)"
    ;;
esac
hash -r

echo
# Another older copy earlier on PATH would run instead of this one.
RESOLVED="$(command -v "$APP_CMD" 2>/dev/null || true)"
if [[ -n "$RESOLVED" && "$RESOLVED" != "$BIN_DIR/$APP_CMD" ]]; then
  info "An older ${APP_CMD} at ${RESOLVED} comes first on PATH; remove it to use the new one."
fi
exec "$BIN_DIR/$APP_CMD" install --channel "$CHANNEL"
