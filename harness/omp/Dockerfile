# The bundle: the harness's own feature (provision.yaml) under
# /features/<name>/, and its manifest in labels, built FROM scratch because it
# is applied by the sandbox agent, never run (ADR 26-10-16-423 §2). A third
# party extends a harness by building FROM this one and adding a feature of
# its own beside it.
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which parses as a build
# that forgot rather than as a bundle declaring nothing, and only a running
# server would ever say so. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . harness/omp/image.json)\"}"
COPY harness/omp/agent.conf harness/omp/check-launcher.sh harness/omp/configure.sh harness/omp/discobox.yml harness/omp/hook-extension.js harness/omp/install-cli.sh harness/omp/launch.sh harness/omp/prompt.sh harness/omp/provision.yaml harness/omp/proxy-env.sh /features/omp/
COPY harness/omp/capture.sh harness/omp/image.json harness/omp/omp-normalize.cjs /features/omp/
# The exporter is owned by this harness; the generic platform has no JS runtime.
COPY harness/capture/runtime.cjs harness/capture/settings.cjs harness/capture/settings-LICENSE.txt /features/omp/harness-capture/
