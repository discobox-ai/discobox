#!/usr/bin/env bash
# Explicit one-formula override. Automatic updates use brew-refresh instead.
set -euo pipefail
export GIT_CONFIG_COUNT=2
export GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=
export GIT_CONFIG_KEY_1=credential.helper GIT_CONFIG_VALUE_1='!gh auth git-credential'
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git clone --depth 1 "https://github.com/${TAP_REPOSITORY:?tap required}.git" "$work/tap"
mkdir -p "$work/tap/Formula"
cp /formula.rb "$work/tap/Formula/${FORMULA_NAME:?formula required}.rb"
git -C "$work/tap" add Formula
if git -C "$work/tap" diff --cached --quiet; then echo 'The tap already serves this formula'; exit 0; fi
git -C "$work/tap" -c user.name='discobox release' -c user.email=noreply@discobox.ai -c commit.gpgsign=false commit -qm "${FORMULA_NAME} ${RELEASE_VERSION:?version required}"
git -C "$work/tap" push origin HEAD:main
