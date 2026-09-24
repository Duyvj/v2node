#!/bin/bash
set -euo pipefail

root=$(cd -- "${BASH_SOURCE[0]%/*}/.." && pwd)
shell=${BASH:-bash}
for file in "$root"/script/*.sh; do
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ "$line" == *$'\r'* ]]; then
            printf 'CRLF is not permitted: %s\n' "$file" >&2
            exit 1
        fi
    done < "$file"
    "$shell" -n "$file"
done

# Test the installed 0.4.19 manager's installer arguments without executing
# package installation, file replacement, or service commands.
parser="" reading=false
while IFS= read -r line; do
    [[ "$line" != 'parse_args() {' ]] || reading=true
    if $reading; then
        parser+="$line"$'\n'
        [[ "$line" != '}' ]] || break
    fi
done < "$root/script/install.sh"
[[ -n "$parser" ]]
parse_fixture() (
    VERSION_ARG="" API_HOST_ARG="" NODE_ID_ARG="" API_KEY_ARG=""
    RELEASE_REPO_ARG="Duyvj/v2node" RELEASE_BRANCH_ARG="main"
    eval "$parser"
    parse_args "$@" || exit 1
    printf '%s|%s|%s\n' "$VERSION_ARG" "$RELEASE_REPO_ARG" "$RELEASE_BRANCH_ARG"
)
[[ $(parse_fixture v0.4.20 --release-repo Duyvj/v2node --release-branch main) == 'v0.4.20|Duyvj/v2node|main' ]]
[[ $(parse_fixture --release-repo Duyvj/v2node --release-branch main v0.4.20) == 'v0.4.20|Duyvj/v2node|main' ]]
[[ $(parse_fixture --release-repo Duyvj/v2node --release-branch main) == '|Duyvj/v2node|main' ]]
if parse_fixture --release-repo >/dev/null 2>&1; then exit 1; fi
if parse_fixture --release-branch ../bad >/dev/null 2>&1; then exit 1; fi
if parse_fixture v0.4.20 --unknown value >/dev/null 2>&1; then exit 1; fi

installer="" reading=false
while IFS= read -r line; do
    [[ "$line" != 'install_v2node() (' ]] || reading=true
    if $reading; then
        installer+="$line"$'\n'
        [[ "$line" != ')' ]] || break
    fi
done < "$root/script/install.sh"
[[ -n "$installer" ]]
# A failed download must never move the installed runtime or restart a service.
events=$(
    RELEASE_REPO_ARG=Duyvj/v2node RELEASE_BRANCH_ARG=main arch=64
    mkdir() { :; }
    mktemp() { printf '/tmp/v2node-download-test\n'; }
    curl() { return 22; }
    rm() { [[ "$*" == '-rf -- /tmp/v2node-download-test' ]]; }
    mv() { printf 'UNEXPECTED_MUTATION\n'; return 1; }
    systemctl() { printf 'UNEXPECTED_MUTATION\n'; return 1; }
    service() { printf 'UNEXPECTED_MUTATION\n'; return 1; }
    eval "$installer"
    if install_v2node v0.4.20; then printf 'UNEXPECTED_SUCCESS\n'; fi
)
[[ -z "$events" ]]
printf 'Shell syntax, LF encoding, legacy update arguments and download failure: PASS\n'
