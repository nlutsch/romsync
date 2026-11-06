#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
    printf 'Usage: bash setup-skyscraper.sh [--prefix /absolute/path] [--bin-dir /absolute/path]\n'
}

prefix="$HOME/.local/share/romsync/skyscraper"
bin_dir="$HOME/.local/bin"
while (($#)); do
    case "$1" in
        --prefix|--bin-dir)
            (($# >= 2)) || { usage >&2; exit 1; }
            if [[ "$1" == --prefix ]]; then prefix="$2"; else bin_dir="$2"; fi
            shift 2 ;;
        --help) usage; exit 0 ;;
        *) usage >&2; exit 1 ;;
    esac
done
[[ "$(uname -s)" == Linux ]] || { echo 'This installer requires Linux (or WSL).' >&2; exit 1; }
[[ "$prefix" == /* && "$bin_dir" == /* ]] || { echo 'Install paths must be absolute.' >&2; exit 1; }
for tool in curl sha256sum mktemp chmod mkdir mv ln readlink getconf sort head; do
    command -v "$tool" >/dev/null || { echo "Missing prerequisite: $tool" >&2; exit 1; }
done
libc="$(getconf GNU_LIBC_VERSION)"
libc_version="${libc#glibc }"
if [[ "$libc" != glibc\ * || "$(printf '%s\n' 2.35 "$libc_version" | sort -V | head -n 1)" != 2.35 ]]; then
    echo "This Skyscraper binary requires glibc 2.35 or newer (found $libc)." >&2
    echo 'Use Ubuntu 22.04/24.04 or a current glibc-based Linux distribution.' >&2
    echo 'On Windows, install a separate distribution: wsl --install -d Ubuntu-24.04' >&2
    exit 1
fi

version=3.21.1
case "$(uname -m)" in
    x86_64) arch=x86_64; checksum=c4ba2a3714bdbb142426b8bfafee05ad281d1ab0996b612f7447edeec72d866f ;;
    aarch64|arm64) arch=aarch64; checksum=022b06733e2abe801c2119d60b08b8117766014be5f89ad823efa90004870943 ;;
    *) echo 'Only x86_64 and aarch64 are supported by this binary installer.' >&2; exit 1 ;;
esac
mkdir -p "$prefix/releases" "$bin_dir"
prefix="$(cd "$prefix" && pwd -P)"
bin_dir="$(cd "$bin_dir" && pwd -P)"
launcher="$bin_dir/Skyscraper"
managed="$prefix/Skyscraper"
if [[ -e "$launcher" || -L "$launcher" ]]; then
    [[ -L "$launcher" && "$(readlink "$launcher")" == "$managed" ]] || {
        echo "Refusing to replace existing $launcher. Choose another --bin-dir." >&2; exit 1;
    }
fi

release="$prefix/releases/$version-$arch"
if [[ ! -d "$release" ]]; then
    work="$(mktemp -d "$prefix/releases/.install.XXXXXXXX")"
    trap 'rm -rf -- "$work"' EXIT
    asset="Skyscraper-$version-$arch.AppImage"
    echo "Downloading official Skyscraper $version ($arch)..."
    curl --fail --location --retry 3 --proto '=https' --tlsv1.2 \
        "https://github.com/Gemba/skyscraper/releases/download/$version/$asset" -o "$work/Skyscraper.AppImage"
    printf '%s  %s\n' "$checksum" "$work/Skyscraper.AppImage" | sha256sum --check -
    chmod u+x "$work/Skyscraper.AppImage"
    echo 'Extracting bundled runtime (no FUSE or system package changes required)...'
    (cd "$work" && ./Skyscraper.AppImage --appimage-extract >/dev/null)
    [[ -x "$work/squashfs-root/AppRun" ]] || { echo 'AppImage extraction failed.' >&2; exit 1; }
    mv "$work/squashfs-root" "$release"
fi

config="$prefix/config.ini"
if [[ ! -e "$config" ]]; then
    escaped_cache="${prefix//\\/\\\\}/cache"
    escaped_cache="${escaped_cache//\"/\\\"}"
    # Noclobber prevents two setup processes from replacing a newly created config.
    (set -o noclobber; cat > "$config" <<EOF
[main]
cacheFolder="$escaped_cache"
unattend=true

[screenscraper]
; Create an account at https://www.screenscraper.fr/ and uncomment the next line.
; Escape a double quote as \\" and a backslash as \\\\ inside quoted INI values.
; userCreds="YOUR_USERNAME:YOUR_PASSWORD"
EOF
    )
fi
chmod 600 "$config"
wrapper="$(mktemp "$prefix/.launcher.XXXXXXXX")"
{
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'exec %q -c %q "$@"\n' "$release/AppRun" "$config"
} > "$wrapper"
chmod 700 "$wrapper"
mv "$wrapper" "$managed"
if [[ ! -L "$launcher" ]]; then ln -s "$managed" "$launcher"; fi

echo 'Verifying the installed CLI...'
if ! "$launcher" --version; then
    echo 'The CLI could not start. If libOpenGL.so.0 is missing, install the runtime:' >&2
    echo '  Debian/Ubuntu: sudo apt-get update && sudo apt-get install libopengl0' >&2
    echo '  Arch Linux:    sudo pacman -S libglvnd' >&2
    echo 'Then rerun this script. No package upgrades are performed by this installer.' >&2
    exit 1
fi
printf '\nInstalled: %s\nPrivate configuration: %s\n' "$launcher" "$config"
printf 'Edit [screenscraper] userCreds in that file; do not put credentials in this repository.\n'
printf 'Run romsync as this same user, with: -skyscraper %q\n' "$launcher"
printf 'Existing configuration is preserved on subsequent runs.\n'
