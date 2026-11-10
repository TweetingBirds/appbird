#!/bin/sh
# Install the appbird binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/TweetingBirds/appbird/main/install.sh | sh
#   curl -fsSL .../install.sh | APPBIRD_VERSION=0.1.0 APPBIRD_DIR=$HOME/bin sh
set -eu

repo="TweetingBirds/appbird"
dir="${APPBIRD_DIR:-/usr/local/bin}"
version="${APPBIRD_VERSION:-}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "appbird: unsupported OS $(uname -s); download a release by hand from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) echo "appbird: unsupported CPU $(uname -m)" >&2; exit 1 ;;
esac

if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || { echo "appbird: couldn't find the latest release" >&2; exit 1; }
fi

name="appbird_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/v$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$name.tar.gz" -o "$tmp/$name.tar.gz"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
(
  cd "$tmp"
  expected=$(grep " $name.tar.gz\$" checksums.txt | cut -d' ' -f1)
  if command -v sha256sum >/dev/null; then actual=$(sha256sum "$name.tar.gz" | cut -d' ' -f1)
  else actual=$(shasum -a 256 "$name.tar.gz" | cut -d' ' -f1); fi
  [ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo "appbird: checksum mismatch" >&2; exit 1; }
  tar -xzf "$name.tar.gz"
)
mkdir -p "$dir"
if [ -w "$dir" ]; then mv "$tmp/$name/appbird" "$dir/appbird"
else sudo mv "$tmp/$name/appbird" "$dir/appbird"; fi
echo "Installed appbird $version to $dir/appbird"
