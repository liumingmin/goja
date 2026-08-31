#!/bin/sh
# Install the goja-debug VS Code extension (macOS / Linux).
#
# Default: symlink from the VS Code extensions directory to this folder so
# edits take effect immediately. Use --copy to copy the files instead.
#
# Usage:
#   ./install.sh            # symlink
#   ./install.sh --copy     # copy
#   ./install.sh --uninstall

set -eu

EXT_NAME="goja-debug"
SRC="$(cd "$(dirname "$0")" && pwd)"
DEST="$HOME/.vscode/extensions/$EXT_NAME"

mode="link"
case "${1:-}" in
    --copy) mode="copy" ;;
    --uninstall)
        if [ -e "$DEST" ] || [ -L "$DEST" ]; then
            rm -rf "$DEST"
            echo "Removed $DEST"
        else
            echo "Nothing to remove: $DEST does not exist"
        fi
        echo "Restart VS Code to complete uninstallation."
        exit 0
        ;;
    "") ;;
    *) echo "unknown option: $1 (use --copy or --uninstall)" >&2; exit 2 ;;
esac

mkdir -p "$HOME/.vscode/extensions"

if [ -e "$DEST" ] || [ -L "$DEST" ]; then
    echo "Removing existing $DEST ..."
    rm -rf "$DEST"
fi

if [ "$mode" = "copy" ]; then
    cp -R "$SRC" "$DEST"
    echo "Copied extension to $DEST"
else
    ln -s "$SRC" "$DEST"
    echo "Symlinked $DEST -> $SRC"
fi

echo "Done. Restart VS Code, then open a .js file and press F5."
