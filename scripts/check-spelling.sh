#!/usr/bin/env bash
# House style: British English in the docs we write. Flags common American
# spellings in Markdown prose, and a space between a number and a percent
# sign ("0.1 %" for "0.1%"). Inline code spans and double-quoted text are
# left out first, because API names, identifiers and quotations from sources
# keep their own spelling. docs/spec and docs/build are not checked.
# Usage: scripts/check-spelling.sh [Markdown files...]
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then
  files=()
  while IFS= read -r f; do files+=("$f"); done < <(
    find docs -name '*.md' -not -path 'docs/spec/*' -not -path 'docs/build/*' | sort)
  for f in README.md CONTRIBUTING.md SECURITY.md SUPPORT.md CODE_OF_CONDUCT.md CONTEXT.md; do
    if [ -f "$f" ]; then files+=("$f"); fi
  done
  set -- "${files[@]}"
fi

american='(de)?serializ(e|es|ed|ing|ation|ations|er|ers)|catalogs?|defense|behaviors?|behavioral|colors?|organiz(e|es|ed|ing|ation|ations)|analyz(e|es|ed|ing|er)|initializ(e|es|ed|ing|ation)|favor(s|ed|ite)?|center(s|ed)?|cancel(ed|ing)'

failed=0
for f in "$@"; do
  # shellcheck disable=SC2016 # the backticks are Markdown, not command substitution.
  if lines="$(sed -E 's/`[^`]*`//g; s/"[^"]*"//g' "$f" | grep -nwiE "$american" | cut -d: -f1)"; then
    for n in $lines; do echo "$f:$n: $(sed -n "${n}p" "$f")"; done
    failed=1
  fi
done
if [ "$failed" -ne 0 ]; then
  echo "House style: use British spellings in the lines above (quote or backtick names that keep their own)."
fi

spaced=0
for f in "$@"; do
  # shellcheck disable=SC2016 # the backticks are Markdown, not command substitution.
  if lines="$(sed -E 's/`[^`]*`//g; s/"[^"]*"//g' "$f" | grep -nE '[0-9] +%' | cut -d: -f1)"; then
    for n in $lines; do echo "$f:$n: $(sed -n "${n}p" "$f")"; done
    spaced=1
  fi
done
if [ "$spaced" -ne 0 ]; then
  echo "House style: write percentages with no space before the percent sign, such as 0.1%."
  failed=1
fi

if [ "$failed" -ne 0 ]; then
  exit 1
fi
