#!/bin/bash

set -e
readonly ME=${0##*/}
readonly SRCDIR=$(dirname "$0")

BENCHMARKS_REPO="https://github.com/hypermodeinc/dgraph-benchmarks"
BENCHMARK_SIZE=${BENCHMARK_SIZE:=big}
SCHEMA_URL="${BENCHMARKS_REPO}/blob/main/data/21million.schema?raw=true"
DGRAPH_LOADER=${DGRAPH_LOADER:=bulk}

function Info {
	echo -e "INFO: $*"
}

function DockerCompose {
	docker compose -p dgraph "$@"
}

if [[ ${BENCHMARK_SIZE} != small && ${BENCHMARK_SIZE} != big ]]; then
	echo >&2 "${ME}: loader must be 'small' or 'big'  -- ${BENCHMARK_SIZE}"
	exit 1
fi

if [[ ${BENCHMARK_SIZE} == small ]]; then
	DATA_URL="${BENCHMARKS_REPO}/blob/main/data/1million.rdf.gz?raw=true"
else
	DATA_URL="${BENCHMARKS_REPO}/blob/main/data/21million.rdf.gz?raw=true"
fi

if [[ ${DGRAPH_LOADER} != bulk && ${DGRAPH_LOADER} != live ]]; then
	echo >&2 "${ME}: loader must be 'bulk' or 'live' -- ${DGRAPH_LOADER}"
	exit 1
fi

Info "entering directory ${SRCDIR}"
cd "${SRCDIR}"

Info "removing old data"
DockerCompose down -v --remove-orphans

Info "bringing up zero container"
DockerCompose up -d --remove-orphans zero1

Info "waiting for zero to become leader"
DockerCompose logs -f zero1 | grep -q -m1 "I've become the leader"

if [[ ${DGRAPH_LOADER} == bulk ]]; then
	Info "bulk loading 21million data set"
	DockerCompose run --rm dg1 \
		bash -s <<EOF
            /gobin/dgraph bulk --schema=<(curl -LSs ${SCHEMA_URL}) --files=<(curl -LSs ${DATA_URL}) \
                               --format=rdf --zero=zero1:5180 --out=/data/dg1/bulk
            mv /data/dg1/bulk/0/p /data/dg1
EOF
fi

Info "bringing up alpha container"
DockerCompose up -d --remove-orphans dg1

Info "waiting for alpha to be ready"
DockerCompose logs -f dg1 | grep -q -m1 "Server is ready"

if [[ ${DGRAPH_LOADER} == live ]]; then
	Info "live loading 21million data set"
	dgraph live --schema=<(curl -LSs "${SCHEMA_URL}") --files=<(curl -LSs "${DATA_URL}") \
		--format=rdf --zero=:5180 --alpha=:9180 --logtostderr
fi
