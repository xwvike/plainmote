#!/bin/sh
# Rebuilds internal/web/static/vendor/codemirror.js.
#
# The bundle is committed and shipped with the application image, so the
# editor never loads code from a third-party CDN. package-lock.json pins every
# dependency and its integrity hash. Installation still happens in a temporary
# directory, keeping node_modules out of both the repository and image build.
#
# Usage: tools/codemirror/build.sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
out="$root/internal/web/static/vendor/codemirror.js"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cp "$root/tools/codemirror/entry.js" "$work/entry.js"
cp "$root/tools/codemirror/package.json" "$work/package.json"
cp "$root/tools/codemirror/package-lock.json" "$work/package-lock.json"
cd "$work"

npm ci --silent --no-audit --no-fund

./node_modules/.bin/esbuild entry.js \
	--bundle --format=esm --minify --legal-comments=none --target=es2020 \
	--outfile=bundle.js

{
	echo '// Editor dependencies bundled for PlainMote; see NOTICE.md in'
	echo '// tools/codemirror for licensing and package-lock.json for versions.'
	echo '// Regenerate with tools/codemirror/build.sh - do not edit by hand.'
	cat bundle.js
} >"$out"

wc -c <"$out" | awk '{printf "codemirror.js: %d bytes (%.0f KB)\n", $1, $1/1024}'
