#!/bin/sh
# discobox-prompt for the shell harness (ADR 0079's wrapper contract):
#
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON [--no-tools]
#   stdout: the answer, and, when a schema is given, one JSON document
#   exit 0: answered. Anything else: not.
#
# A shell discobox runs no model, so there is nothing to ask. Its one
# consumer that must be answered is discobox-access's command judge, which
# will not run a wrapped command without a verdict. The judge role gets a
# fixed one, and it fails closed as ADR 0079 requires: refused, unless a person
# set DISCOBOX_SHELL_JUDGE=allow for this discobox, which allows every judged
# command. Every other role is not answered.
#
# This is a stand-in until the pool judge decides these requests; the
# destination host a credential may go to is enforced either way.

set -eu

model=""
while [ $# -gt 0 ]; do
	case "$1" in
	--model) model="${2:-}"; shift 2 ;;
	--model=*) model="${1#--model=}"; shift ;;
	--system | --prompt | --output-schema) shift 2 ;;
	--system=* | --prompt=* | --output-schema=* | --no-tools) shift ;;
	*) shift ;;
	esac
done

if [ "$model" != "judge" ]; then
	echo "discobox-prompt: a shell discobox runs no model to ask" >&2
	exit 1
fi

case "${DISCOBOX_SHELL_JUDGE:-}" in
allow)
	printf '%s\n' '{"allow":true,"reason":"this shell discobox allows every judged command (DISCOBOX_SHELL_JUDGE=allow)"}'
	;;
*)
	printf '%s\n' '{"allow":false,"reason":"a shell discobox runs no model to judge commands, so it refuses them unless DISCOBOX_SHELL_JUDGE=allow"}'
	;;
esac
