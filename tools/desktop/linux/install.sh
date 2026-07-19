#!/usr/bin/env sh
# Install the Knomit desktop launcher from an extracted release tarball.
#
# The knomit-desktop binary resolves its ONNX/graphqlite libs from <exe>/lib,
# so it must keep running from THIS extracted directory — this script does not
# copy the binary anywhere. It only registers a .desktop launcher (with Exec
# pointing at the binary's absolute path here) and installs the app icon into
# the user's XDG dirs, so Knomit shows up in the GNOME/KDE app grid.
#
# Runtime prerequisites (install with your distro's package manager first):
#   GTK 4 + WebKitGTK 6.0 + libsoup 3 runtime libraries.
#
# Re-run this script if you move the extracted directory.
set -eu

DIR="$(cd "$(dirname "$0")" && pwd)"
XDG_DATA="${XDG_DATA_HOME:-$HOME/.local/share}"
APPS="$XDG_DATA/applications"
ICONS="$XDG_DATA/icons/hicolor/256x256/apps"

if [ ! -x "$DIR/knomit-desktop" ]; then
	echo "error: knomit-desktop not found in $DIR — run this from the extracted tarball." >&2
	exit 1
fi

mkdir -p "$APPS" "$ICONS"
install -m 0644 "$DIR/appicon.png" "$ICONS/knomit-desktop.png"
# {{EXEC}} is the placeholder in the committed .desktop template.
sed "s#{{EXEC}}#$DIR/knomit-desktop#" "$DIR/knomit-desktop.desktop" > "$APPS/knomit-desktop.desktop"

update-desktop-database "$APPS" 2>/dev/null || true
gtk-update-icon-cache -f -t "$XDG_DATA/icons/hicolor" 2>/dev/null || true

echo "Installed Knomit launcher. Launch it from your app menu, or run directly:"
echo "  $DIR/knomit-desktop"
