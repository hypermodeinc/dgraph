#!/bin/bash

set -e
readonly ME=${0##*/}
readonly SRCDIR=$(dirname "$0")

BENCHMARKS_REPO="${BENCHMARKS_REPO:-$(go env GOPATH)/src/github.com/hypermodeinc/dgraph-benchmarks}"
SCHEMA_FILE="${BENCHMARKS_REPO}/data/21million.schema"
DATA_FILE="${BENCHMARKS_REPO}/data/21million.rdf.gz"

# this may be used to load a smaller data set when testing the test itself
#DATA_FILE="$BENCHMARKS_REPO/data/goldendata.rdf.gz"

function Info {
	echo -e "INFO: $*"
}

function DockerCompose {
	docker compose -p dgraph "$@"
}
function DgraphLive {
	dgraph live "$@"
}

HELP= LOADER=bulk CLEANUP= SAVEDIR= LOAD_ONLY= QUIET= MODE=

ARGS=$(/usr/bin/getopt -n"${ME}" -o"h" -l"help,loader:,cleanup:,savedir:,load-only,quiet,mode:" -- "$@") || exit 1
eval set -- "${ARGS}"
while true; do
	case "$1" in
	-h | --help) HELP=yes ;;
	--loader)
		LOADER=${2,,}
		shift
		;;
	--cleanup)
		CLEANUP=${2,,}
		shift
		;;
	--savedir)
		SAVEDIR=${2,,}
		shift
		;;
	--load-only) LOAD_ONLY=yes ;;
	--quiet) QUIET=yes ;;
	--mode)
		MODE=${2,,}
		shift
		;;
	--)
		shift
		break
		;;
	esac
	shift
done

if [[ -n ${HELP} ]]; then
	cat <<EOF
usage: ${ME} [-h|--help] [--loader=<bulk|live|none>] [--cleanup=<all|none|servers>] [--savedir=path] [--mode=<normal|none>]

options:

    --loader        bulk = use dgraph bulk (default)
                    live = use dgraph live
                    none = use data loaded by previous run
    --cleanup       all = take down containers and data volume (default)
                    servers = take down dgraph zero and alpha but leave data volume up
                    none = leave up containers and data volume
    --savedir=path  specify a directory to save test failure json in
                    for easier post-test review
    --load-only     load data but do not run tests
    --quiet         just report which queries differ, without a diff
    --mode          normal = run dgraph in normal mode
                    none = run dgraph in normal mode
EOF
	exit 0
fi

if [[ ${LOADER} != bulk && ${LOADER} != live && ${LOADER} != none ]]; then
	echo >&2 "${ME}: loader must be 'bulk' or 'live' or 'none' -- ${LOADER}"
	exit 1
fi

# default to leaving the data around for another run
# if already re-using it from a previous run
if [[ ${LOADER} == none && -z ${CLEANUP} ]]; then
	CLEANUP=servers
fi

# default to cleaning up both services and volume
if [[ -z ${CLEANUP} ]]; then
	CLEANUP=all
elif [[ ${CLEANUP} != all && ${CLEANUP} != servers && ${CLEANUP} != none ]]; then
	echo >&2 "${ME}: cleanup must be 'all' or 'servers' or 'none' -- ${LOADER}"
	exit 1
fi

# default to quiet mode if diffs are being saved in a directory
if [[ -n ${SAVEDIR} ]]; then
	QUIET=yes
fi

Info "entering directory ${SRCDIR}"
cd "${SRCDIR}"

if [[ ${LOADER} != none ]]; then
	Info "removing old data (if any)"
	DockerCompose down -v --remove-orphans
else
	Info "using previously loaded data"
fi

Info "bringing up zero container"
DockerCompose up -d --remove-orphans --force-recreate zero1

Info "waiting for zero to become leader"
DockerCompose logs -f zero1 | grep -q -m1 "I've become the leader"

if [[ ${LOADER} == bulk ]]; then
	Info "bulk loading data set"
	DockerCompose run --rm -v "${BENCHMARKS_REPO}":"${BENCHMARKS_REPO}" --name bulk_load zero1 \
		bash -s <<EOF
            mkdir -p /data/alpha1
            mkdir -p /data/alpha2
            mkdir -p /data/alpha3
            /gobin/dgraph bulk --schema=${SCHEMA_FILE} --files=${DATA_FILE} \
                               --format=rdf --zero=zero1:5180 --out=/data/zero1/bulk \
                               --reduce_shards 3 --map_shards 9
            mv /data/zero1/bulk/0/p /data/alpha1
            mv /data/zero1/bulk/1/p /data/alpha2
            mv /data/zero1/bulk/2/p /data/alpha3
EOF
fi

Info "bringing up alpha container"
DockerCompose up -d --force-recreate --remove-orphans alpha1 alpha2 alpha3

Info "waiting for alpha to be ready"
DockerCompose logs -f alpha1 | grep -q -m1 "Server is ready"
# after the server prints the log "Server is ready", it may be still loading data from badger
Info "sleeping for 10 seconds for the server to be ready"
sleep 10

if [[ ${LOADER} == live ]]; then
	Info "live loading data set"
	DgraphLive --schema="${SCHEMA_FILE}" --files="${DATA_FILE}" \
		--format=rdf --zero=:5180 --alpha=:9180 --logtostderr
fi

if [[ -n ${LOAD_ONLY} ]]; then
	Info "exiting after data load"
	exit 0
fi

# replace variables if set with the corresponding option
SAVEDIR=${SAVEDIR:+-savedir=${SAVEDIR}}
QUIET=${QUIET:+-quiet}

Info "running benchmarks/regression queries"

if [[ -n ${TEAMCITY_VERSION} ]]; then
	# Make TeamCity aware of Go tests
	export GOFLAGS="-json"
fi
go test -v -tags standalone "${SAVEDIR}" "${QUIET}" || FOUND_DIFFS=1

if [[ ${FOUND_DIFFS} -eq 0 ]]; then
	Info "no diffs found in query results"
else
	Info "Cluster logs for alpha1"
	docker logs alpha1
	Info "Cluster logs for alpha2"
	docker logs alpha2
	Info "Cluster logs for alpha3"
	docker logs alpha3
	Info "Cluster logs for zero1"
	docker logs zero1
	Info "found some diffs in query results"
fi

if [[ ${CLEANUP} == all ]]; then
	Info "bringing down zero and alpha and data volumes"
	DockerCompose down -v --remove-orphans
elif [[ ${CLEANUP} == none ]]; then
	Info "leaving up zero and alpha"
else
	Info "bringing down zero and alpha only"
	DockerCompose down --remove-orphans
fi

exit "${FOUND_DIFFS}"
