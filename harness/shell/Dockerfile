# Nearly empty. Every other harness bundle carries its CLI's feature and
# declares what it needs; this one's harness *is* the shell the platform
# already ships, and its env, volumes and groups are the platform's own layer,
# which the server composes under every harness bundle's (ADR 26-10-16-423 §2).
#
# So the bundle holds no feature, only the label: its image.json names nothing
# but the platform API version it runs on, which every harness bundle must. It
# is registered under the reserved `shell` slug, which is also what withholds
# the harness-run convention from it — a command typed into this terminal would
# be a second shell inside the first (ADR 0086 §3).
#
# Nor does it install discobox-prompt, which every bundle that runs a model
# provides for the project's judge. A shell runs no model, so a project whose
# default harness is this one has no judge discobox (ADR 26-10-02-054 §2).
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which parses as a build
# that forgot rather than as a bundle declaring nothing, and only a running
# server would ever say so. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"
