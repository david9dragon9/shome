#!/bin/sh
# Build and install shome.
#
# One binary does everything: controller, node agent, and client. This builds
# it for this machine, and -- so that other machines can join with a single
# command -- cross-builds the Linux binaries the controller hands out.
#
#   ./install.sh              install to ~/.local/bin
#   ./install.sh /usr/local/bin
set -e

bindir="${1:-${SHOME_BIN:-$HOME/.local/bin}}"
repo=$(cd "$(dirname "$0")" && pwd)
cd "$repo"

if ! command -v go >/dev/null 2>&1; then
  cat >&2 <<'ERR'
shome needs Go 1.25 or newer to build, and it is not on your PATH.

  macOS:  brew install go
  Debian: sudo apt install golang-go     (check the version; 1.25+ is required)
  Other:  https://go.dev/dl/
ERR
  exit 1
fi

echo "building shome for this machine"
mkdir -p "$bindir"
go build -o "$bindir/shome" ./cmd/shome
# The login shell is only used on a controller acting as an SSH login node,
# but it is small and building it here avoids a second step later.
go build -o "$bindir/shome-shell" ./cmd/shome-shell
echo "  installed $bindir/shome"

# Binaries the controller serves to machines that join. Building them here is
# what makes adding a machine a single pasted command rather than a build.
dist="${SHOME_HOME:-$HOME/.shome}/dist"

# Linux cross-compiles cleanly with cgo off.
for target in linux-amd64 linux-arm64; do
  goos=${target%-*}; goarch=${target#*-}
  mkdir -p "$dist/$target"
  if GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 \
       go build -o "$dist/$target/shome" ./cmd/shome 2>/dev/null; then
    echo "  built $target agent for machines that join"
  else
    rm -rf "$dist/$target"
    echo "  note: could not cross-build $target; a Linux machine will need to run this script itself"
  fi
done

# The other macOS architecture, so an Intel Mac can join an Apple Silicon
# cluster and vice versa.
#
# This needs cgo -- the darwin backend uses Metal for GPU detection and libproc
# for memory accounting -- and cgo does not cross-compile with the Go toolchain
# alone. It does work on macOS, because Xcode's clang targets both
# architectures: passing -arch is enough. Without this, joining a Mac of the
# other kind means installing Go on it and building from source.
if [ "$(go env GOOS)" = "darwin" ]; then
  case "$(go env GOARCH)" in
    arm64) other_arch=amd64; other_cc="clang -arch x86_64" ;;
    amd64) other_arch=arm64; other_cc="clang -arch arm64" ;;
    *)     other_arch="" ;;
  esac
  if [ -n "$other_arch" ]; then
    target="darwin-$other_arch"
    mkdir -p "$dist/$target"
    if GOOS=darwin GOARCH=$other_arch CGO_ENABLED=1 CC="$other_cc" \
         go build -o "$dist/$target/shome" ./cmd/shome 2>/dev/null; then
      echo "  built $target agent for machines that join"
    else
      rm -rf "$dist/$target"
      echo "  note: could not cross-build $target (needs Xcode command line tools);"
      echo "        a Mac of that kind will need to run this script itself"
    fi
  fi
fi

# This machine's own architecture, so a Mac of the SAME kind is served a
# purpose-built binary rather than a copy of the running controller. The
# controller falls back to its own executable if this is missing, so a failure
# here is not fatal.
host_target="$(go env GOOS)-$(go env GOARCH)"
mkdir -p "$dist/$host_target"
cp "$bindir/shome" "$dist/$host_target/shome" 2>/dev/null || true

case ":$PATH:" in
  *":$bindir:"*) ;;
  *)
    echo
    echo "$bindir is not on your PATH. Add it:"
    echo "    echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.profile"
    echo "    export PATH=\"$bindir:\$PATH\""
    ;;
esac

cat <<DONE

Done. Start a cluster on this machine:

    shome up

DONE
