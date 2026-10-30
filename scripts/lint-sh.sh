#! /bin/bash

SHELLCHECK_VERSION=$(cat .shellcheck-version)

SH_FILES=$(git ls-files **.sh)

docker run --rm -i \
  -v "$PWD:/mnt" \
  -w /mnt \
  koalaman/shellcheck:"$SHELLCHECK_VERSION" \
  -x -a $SH_FILES
