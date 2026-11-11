#!/usr/bin/env sh
# Downloads the WANDS dataset (Wayfair, MIT licence) into ./data.
set -eu
mkdir -p data
for f in product query label; do
  [ -s "data/$f.csv" ] && continue
  echo "downloading $f.csv"
  curl -fsSL --retry 3 -o "data/$f.csv" "https://raw.githubusercontent.com/wayfair/WANDS/main/dataset/$f.csv"
done
