#!/bin/sh
# Install the Valiido CLI: curl -fsSL https://raw.githubusercontent.com/valiido/valiido-cli/main/install.sh | sh
# VALIIDO_VERSION=v1.2.3 pins a release; VALIIDO_INSTALL_DIR changes the target (default ~/.local/bin).
set -eu

repo=https://github.com/valiido/valiido-cli
dir=${VALIIDO_INSTALL_DIR:-$HOME/.local/bin}

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported system: $(uname -s). Download a release from $repo/releases." >&2; exit 1 ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)." >&2; exit 1 ;;
esac

if [ -n "${VALIIDO_VERSION:-}" ]; then
  base=$repo/releases/download/$VALIIDO_VERSION
else
  base=$repo/releases/latest/download
fi
archive=valiido_${os}_${arch}.tar.gz

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "Checksum mismatch for $archive. Nothing was installed." >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp" valiido
mkdir -p "$dir"
install -m 0755 "$tmp/valiido" "$dir/valiido"
echo "Installed valiido to $dir/valiido"

case :$PATH: in
  *:"$dir":*) ;;
  *) echo "Add $dir to your PATH." ;;
esac
echo "Next: valiido auth login && valiido setup"
