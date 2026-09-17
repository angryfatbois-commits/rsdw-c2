#!/usr/bin/env bash
set -euo pipefail
source_root=$(realpath -e "${1:?source directory required}")
relative=${2:?relative save path required}
destination=${3:?destination save directory required}
[[ $relative != /* && $relative == *.sav && ! $relative =~ (^|/)\.\.(/|$) ]] || {
    echo 'Save path must be relative, end in .sav, and contain no traversal' >&2
    exit 1
}
source_file=$(realpath -e "$source_root/$relative")
[[ $source_file == "$source_root/"* && -f $source_file ]] || { echo 'Save source escapes its volume or is not a file' >&2; exit 1; }
mkdir -p "$destination"
if [[ -e $destination/.seed-complete ]] || [[ -n $(find "$destination" -iname '*.sav' -print -quit) ]]; then
    echo 'Existing world found; skipping initial import'
    exit 0
fi
temporary=$(mktemp "$destination/.seed.XXXXXX")
trap 'rm -f "$temporary"' EXIT
cp "$source_file" "$temporary"
chmod 600 "$temporary"
mv -T "$temporary" "$destination/$(basename "$relative")"
touch "$destination/.seed-complete"
echo 'Initial world imported'
