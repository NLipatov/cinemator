#!/usr/bin/env bash
set -euo pipefail

base_sha=${1:?base SHA is required}
head_sha=${2:?head SHA is required}
pattern='^[a-z][a-z0-9-]*(\([a-z0-9][a-z0-9._/-]*\))?!?: .+'
failed=0

if [[ -n ${PR_TITLE:-} && ! $PR_TITLE =~ $pattern ]]; then
  echo "::error::Pull request title is not a Conventional Commit: $PR_TITLE"
  failed=1
fi

while IFS=$'\t' read -r sha subject; do
  if [[ $subject == "Merge "* ]]; then
    continue
  fi
  if [[ ! $subject =~ $pattern ]]; then
    echo "::error::Commit ${sha:0:12} is not a Conventional Commit: $subject"
    failed=1
  fi
done < <(git log --format='%H%x09%s' "$base_sha..$head_sha")

if (( failed != 0 )); then
  echo "Expected format: type(scope)!: description"
  exit 1
fi
