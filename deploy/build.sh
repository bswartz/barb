#!/bin/sh -xe

[ -n "$1" ] || exit 1

C=$(buildah from gcr.io/distroless/static:98caaa0389fffd98ca3446403c9eaa1ff08f3131)
buildah add "$C" bin/barb /
buildah config --entrypoint='["/barb"]' "$C"
buildah commit "$C" "$1"
buildah rm "$C"
