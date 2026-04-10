#!/bin/bash
# description: Load a skill's SKILL.md by name
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "Usage: load_skill <skill-name> [skill-name...]" >&2
  exit 1
fi

OLLIE="${OLLIE_9MOUNT:-$HOME/mnt/ollie}"

for arg in "$@"; do
  name="${arg##*/}"
  cat "$OLLIE/sk/${name}.md"
done
