#!/bin/bash

set -e
set -x
set -o pipefail

TAG="$EVG_TRIGGERED_BY_TAG"
if [ -z "$TAG" ]; then
    echo "Cannot regenerate the Augmented SBOM file without a tag"
    exit 0
fi

SBOM_FILE="./ssdlc/$TAG.bom.json"

cat <<EOF >silkbomb.env
SILK_CLIENT_ID=${SILK_CLIENT_ID}
SILK_CLIENT_SECRET=${SILK_CLIENT_SECRET}
EOF
# shellcheck disable=SC2068 # we don't want to quote `$@`
podman run \
    -it --rm \
    --platform linux/amd64 \
    -v "${PWD}":/pwd \
    --env-file silkbomb.env \
    artifactory.corp.mongodb.com/release-tools-container-registry-public-local/silkbomb:1.0 \
    download \
    --silk-asset-group database-tools \
    --sbom-out "/pwd/$SBOM_FILE" \
    $@

# TODO (TOOLS-3563): Remove this workaround.
#
# This is a gross workaround for an issue where the file generated by
# `silkbomb` has no `vulnerabilities` key at all when there are no
# vulnerabilities in our deps. This adds the key with an empty array -
# `"vulnerabilities": []` - which is required per our SSDLC policy.
#
# shellcheck disable=SC2046
if [ $(jq 'has("vulnerabilities")' "$SBOM_FILE") = "false" ]; then
    # shellcheck disable=SC2002 # this doesn't work without `cat`
    cat "$SBOM_FILE" | jq '.vulnerabilities += []' | tee "$SBOM_FILE"
fi
