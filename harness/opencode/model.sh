#!/bin/sh
# opencode-model: prints the model an opencode session starts on, or nothing
# when opencode would pick one itself. discobox-prompt's judge follows it when
# Discobox's settings name no judge, and configure checks it, because that is
# the model a sandbox's first session runs on.
#
# The TUI starts on opencode's `model` setting, else the last model picked
# with /models (measured on opencode 2.0.29). `opencode run` given no --model
# does not read the /models pick, so whoever runs one on this model passes it.
set -eu

MODEL_PREFERENCE="${XDG_STATE_HOME:-$HOME/.local/state}/opencode/model.json"
# configured_model is opencode's `model` setting, read the way opencode layers
# its global configuration: config.json, then opencode.json, then
# opencode.jsonc, the later winning — so they are read in reverse, and the
# first that names a model is the one. opencode reads them as JSONC, so they
# are read the same way here: comments, line and block, and trailing commas
# are allowed. A file that still is not JSON is skipped, as one that is not
# there is. Only the model's name is taken from it.
configured_model() {
	dir="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
	for file in "$dir/opencode.jsonc" "$dir/opencode.json" "$dir/config.json"; do
		[ -f "$file" ] || continue
		configured=$(read_config_model "$file") || configured=""
		if [ -n "$configured" ]; then
			printf '%s' "$configured"
			return
		fi
	done
}

# read_config_model prints a JSONC file's top-level `model`, if it is a string.
# Comments and trailing commas are dropped outside strings, by walking the text
# rather than by pattern, so a URL or a "//" inside a string is left alone.
# node is what the image has for this; without it, plain JSON still reads.
read_config_model() {
	if ! command -v node >/dev/null 2>&1; then
		jq -r '.model // "" | strings' "$1" 2>/dev/null
		return
	fi
	node -e '
		// Two walks, each outside strings: comments out first, then trailing
		// commas, so a comment between a comma and its bracket cannot hide it.
		const walk = (src, visit) => {
			let out = "", i = 0, inString = false;
			while (i < src.length) {
				const c = src[i];
				if (inString) {
					out += c;
					if (c === "\\") { out += src[i + 1] ?? ""; i += 2; continue; }
					if (c === "\"") inString = false;
					i++;
					continue;
				}
				if (c === "\"") { inString = true; out += c; i++; continue; }
				const skip = visit(src, i);
				if (skip > 0) { i += skip; continue; }
				out += c;
				i++;
			}
			return out;
		};
		const noComments = walk(require("fs").readFileSync(process.argv[1], "utf8"), (src, i) => {
			if (src[i] === "/" && src[i + 1] === "/") { const end = src.indexOf("\n", i); return (end < 0 ? src.length : end) - i; }
			if (src[i] === "/" && src[i + 1] === "*") { const end = src.indexOf("*/", i + 2); return (end < 0 ? src.length : end + 2) - i; }
			return 0;
		});
		const json = walk(noComments, (src, i) => {
			if (src[i] !== ",") return 0;
			let j = i + 1;
			while (j < src.length && /\s/.test(src[j])) j++;
			return src[j] === "}" || src[j] === "]" ? 1 : 0;
		});
		const model = JSON.parse(json).model;
		if (typeof model === "string") process.stdout.write(model);
	' "$1" 2>/dev/null
}

named=$(configured_model)
if [ -z "$named" ] && [ -f "$MODEL_PREFERENCE" ]; then
	named=$(jq -r '.recent[0] // empty | "\(.providerID)/\(.modelID)"' "$MODEL_PREFERENCE" 2>/dev/null) || named=""
fi
printf '%s' "$named"
