#!/usr/bin/env bash
# Prints the next semantic version, without a leading "v", based on the
# Conventional Commit messages since the last tag v*. Prints nothing if
# none of the commits requires a release.
#
#   feat: …                         -> minor
#   fix: … / perf: …                -> patch
#   type!: … or "BREAKING CHANGE:"  -> major, or minor before 1.0.0
#   anything else (docs, chore, …)  -> no release
set -euo pipefail

last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
if [[ -n "$last" ]]; then
	range="${last}..HEAD"
	base="${last#v}"
else
	range="HEAD"
	base="0.0.0"
fi
IFS=. read -r major minor patch <<<"${base%%-*}"

breaking='^[a-z]+(\([^)]*\))?!: '
feature='^feat(\([^)]*\))?: '
bugfix='^(fix|perf)(\([^)]*\))?: '

bump=""
while IFS= read -r line; do
	if [[ "$line" =~ $breaking || "$line" == "BREAKING CHANGE:"* || "$line" == "BREAKING-CHANGE:"* ]]; then
		bump=major
		break
	elif [[ "$line" =~ $feature ]]; then
		bump=minor
	elif [[ "$line" =~ $bugfix && -z "$bump" ]]; then
		bump=patch
	fi
done < <(git log --format='%s%n%b' "$range")

case "$bump" in
major)
	if ((major == 0)); then
		minor=$((minor + 1)) patch=0
	else
		major=$((major + 1)) minor=0 patch=0
	fi
	;;
minor) minor=$((minor + 1)) patch=0 ;;
patch) patch=$((patch + 1)) ;;
*) exit 0 ;;
esac
echo "${major}.${minor}.${patch}"
