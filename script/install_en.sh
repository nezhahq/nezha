#!/bin/sh

#========================================================
# master install script redirect
#========================================================

# Default: Guide users to use master branch v1 new panel installation script
# However, if install_agent parameter is used, redirect to v0 by default
if echo "$@" | grep -q "install_agent"; then
    echo "Detected v0 panel install_agent parameter, will use v0 branch script..."
    echo "Warning: v0 panel is no longer maintained, please upgrade to v1 panel ASAP. See docs: https://nezha.wiki/, script will continue in 5s"
    sleep 5
    is_v1=false
else
    echo "v1 panel has been officially released, v0 is no longer maintained. If you have v0 panel installed, please upgrade to v1 ASAP"
    echo "v1 differs significantly from v0, see documentation: https://nezha.wiki/"
    echo "If you don't want to upgrade now, enter 'n' and press Enter to continue using v0 panel script"
    read -p "Execute v1 panel installation script? [y/n] " choice
    case "$choice" in
        n|N)
            is_v1=false
            ;;
        *)
            is_v1=true
            ;;
    esac
fi

if [ "$is_v1" = true ]; then
    echo "Will use v1 panel installation script..."
    shell_url="https://raw.githubusercontent.com/nezhahq/scripts/main/install_en.sh"
    file_name="nezha.sh"
else
    echo "Will use v0 panel installation script, script will be downloaded as nezha_v0.sh"
    shell_url="https://raw.githubusercontent.com/nezhahq/scripts/refs/heads/v0/install_en.sh"
    file_name="nezha_v0.sh"
fi


# Download into a private directory: a fixed name in /tmp can be created in advance by another local user
# An explicit template: BSD mktemp (macOS) ignores TMPDIR without one.
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/nezha.XXXXXX")" || exit 1

download_failed() {
    echo "Error: failed to download the installation script, aborting"
    rm -rf "$tmp_dir"
    exit 1
}

if command -v wget >/dev/null 2>&1; then
    wget -O "$tmp_dir/nezha.sh" "$shell_url" || download_failed
elif command -v curl >/dev/null 2>&1; then
    curl -f -o "$tmp_dir/nezha.sh" "$shell_url" || download_failed
else
    echo "Error: wget or curl not found, please install either one and try again"
    rm -rf "$tmp_dir"
    exit 1
fi

chmod +x "$tmp_dir/nezha.sh" || download_failed
mv "$tmp_dir/nezha.sh" "./$file_name" || download_failed
rm -rf "$tmp_dir"
# Run the new script with the original parameters
exec ./"$file_name" "$@"
