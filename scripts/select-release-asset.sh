#!/usr/bin/env bash
# Select the newest published release asset by walking version tags. The
# releases collection API can lag behind the tags API after publishing.

set -euo pipefail

repository="${1:-}"
channel="${2:-}"
asset_prefix="${3:-}"
if [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ || ( "$channel" != "stable" && "$channel" != "beta" ) || -z "$asset_prefix" ]]; then
  echo "usage: select-release-asset.sh OWNER/REPO stable|beta ASSET_PREFIX" >&2
  exit 2
fi

base="https://api.github.com/repos/${repository}"
releases_json="$(curl -fsSL -H 'Accept: application/vnd.github+json' "${base}/releases?per_page=100")"
tags_json="$(curl -fsSL -H 'Accept: application/vnd.github+json' "${base}/tags?per_page=100")"

select_asset() {
  local payload="$1" tag="$2"
  printf '%s\n' "$payload" | awk -v channel="$channel" -v want="$asset_prefix" -v want_tag="$tag" '
    /"tag_name":/ {
      split($0, t, "\""); current_tag = t[4]
      active = (current_tag == want_tag)
      draft = 0; pre = 0; digest = ""
    }
    /"draft": true/ { if (active) draft = 1 }
    /"prerelease": true/ { if (active) pre = 1 }
    /"digest":/ { if (active) { split($0, d, "\""); digest = d[4] } }
    /"browser_download_url":/ {
      if (active) {
        split($0, u, "\""); url = u[4]; n = split(url, parts, "/"); name = parts[n]
        if (!draft && (channel == "beta" || !pre) && index(name, want) == 1 && name !~ /\.(sig|json)$/) {
          print url, (digest == "" ? "-" : digest)
          exit
        }
      }
      digest = ""
    }
  '
}

direct_attempts=0
while IFS= read -r tag; do
  [[ "$tag" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]] || continue
  version="${tag#v}"
  if [[ "$channel" == "stable" && "$version" == *-* ]]; then
    continue
  fi

  if printf '%s\n' "$releases_json" | grep -Fq "\"tag_name\": \"${tag}\""; then
    asset="$(select_asset "$releases_json" "$tag")"
  elif ((direct_attempts < 5)); then
    direct_attempts=$((direct_attempts + 1))
    release_json="$(curl -fsSL -H 'Accept: application/vnd.github+json' "${base}/releases/tags/${tag}" 2>/dev/null)" || continue
    asset="$(select_asset "$release_json" "$tag")"
  else
    continue
  fi

  if [[ -n "$asset" ]]; then
    printf '%s\n' "$asset"
    exit 0
  fi
done < <(printf '%s\n' "$tags_json" | awk -F '"' '/"name":/ { print $4 }')

echo "no published ${channel} release has an asset named ${asset_prefix}*" >&2
exit 1
