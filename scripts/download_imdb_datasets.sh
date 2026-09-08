#!/usr/bin/env bash
set -Eeuo pipefail

# IMDb publishes these TSV snapshots for non-commercial use. Downloads are
# resumable and written atomically so an interrupted run never leaves a file
# that looks complete to catalog-sync.
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd "$script_dir/.." && pwd)
dataset_dir=${IMDB_DATASET_DIR:-"$repo_dir/data/imdb"}
base_url=${IMDB_DATASET_BASE_URL:-https://datasets.imdbws.com}

if ! command -v aria2c >/dev/null 2>&1 && ! command -v curl >/dev/null 2>&1; then
	printf 'aria2c or curl is required\n' >&2
	exit 1
fi

mkdir -p "$dataset_dir"
dataset_list=${IMDB_DATASETS:-"title.basics.tsv.gz title.ratings.tsv.gz title.principals.tsv.gz name.basics.tsv.gz title.akas.tsv.gz title.crew.tsv.gz"}
download_connections=${IMDB_DOWNLOAD_CONNECTIONS:-8}
read -r -a datasets <<< "$dataset_list"
for dataset in "${datasets[@]}"; do
	destination="$dataset_dir/$dataset"
	temporary="$destination.part"
	if [[ -s "$destination" ]] && gzip -t "$destination" >/dev/null 2>&1; then
		printf 'already complete %s\n' "$dataset"
		continue
	fi
	printf 'downloading %s\n' "$dataset"
	if command -v aria2c >/dev/null 2>&1; then
		# Only resume a partial file that aria2c owns. A .part created by curl
		# has no range bookkeeping and must be replaced from byte zero.
		if [[ -e "$temporary" && ! -e "$temporary.aria2" ]]; then
			mv -f "$temporary" "$temporary.curl-part"
		fi
		aria2c --continue=true --allow-overwrite=true --auto-file-renaming=false \
			--file-allocation=none --check-integrity=true \
			--max-connection-per-server="$download_connections" \
			--split="$download_connections" --min-split-size=1M \
			--max-tries=5 --retry-wait=2 --timeout=30 --connect-timeout=10 \
			--dir="$(dirname "$temporary")" --out="$(basename "$temporary")" \
			"$base_url/$dataset"
	else
		curl --fail --location --retry 5 --retry-delay 2 --retry-all-errors \
			--continue-at - --output "$temporary" "$base_url/$dataset"
	fi
	if ! gzip -t "$temporary" >/dev/null 2>&1; then
		printf 'downloaded file failed gzip validation: %s\n' "$dataset" >&2
		exit 1
	fi
	mv -f "$temporary" "$destination"
done
printf 'IMDb datasets ready in %s\n' "$dataset_dir"
