# Every built-in harness's bundle (ADR 26-11-06-596 §1). Its build context is
# the directory discobox-harness-bundle stages from the harness's declaration,
# never the repository: the feature under features/<directory>/ and the
# manifest label beside it. Built FROM scratch because the sandbox agent
# applies a bundle and never runs it (ADR 26-10-16-423 §2); a third party
# extends a harness by building FROM its bundle and adding a feature of its own.
#
# The label stage is the whole bundle of a harness with no feature (the shell):
# a bundle with nothing to provision has no layer at all. The staged context's
# `target` file names the stage to build.
FROM scratch AS label
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which parses as a build
# that forgot rather than as a bundle declaring nothing, and only a running
# server would ever say so. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\" from the staged context}"

FROM label AS bundle
COPY features/ /features/
