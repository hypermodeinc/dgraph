<picture>
      <source
        srcset="/logo-dark.png"
        media="(prefers-color-scheme: dark)"
      />
      <source
        srcset="/logo.png"
        media="(prefers-color-scheme: light), (prefers-color-scheme: no-preference)"
      />
      <img alt="Dgraph Logo" src="/logo.png">
</picture>

[![chat](https://img.shields.io/discord/1267579648657850441)](https://discord.hypermode.com)
[![GitHub Repo stars](https://img.shields.io/github/stars/hypermodeinc/dgraph)](https://github.com/hypermodeinc/dgraph/stargazers)
[![GitHub commit activity](https://img.shields.io/github/commit-activity/m/hypermodeinc/dgraph)](https://github.com/hypermodeinc/dgraph/commits/main/)
[![Go Report Card](https://goreportcard.com/badge/github.com/dgraph-io/dgraph)](https://goreportcard.com/report/github.com/dgraph-io/dgraph)

Dgraph is a horizontally scalable and distributed GraphQL database with a graph backend. It provides
ACID transactions, consistent replication, and linearizable reads. It's built from the ground up to
perform a rich set of queries. Being a native GraphQL database, it tightly controls how the data is
arranged on disk to optimize for query performance and throughput, reducing disk seeks and network
calls in a cluster.

Dgraph's goal is to provide Google production-level scale and throughput, with low enough latency to
serve real-time user queries over terabytes of structured data. Dgraph supports
[GraphQL query syntax](https://docs.hypermode.com/dgraph/graphql/overview), and responds in
[JSON](http://www.json.org/) and [Protocol Buffers](https://developers.google.com/protocol-buffers/)
over [GRPC](http://www.grpc.io/) and HTTP. Dgraph is written using the Go Programming Language.

## Status

Dgraph is at [version v24.0.5][rel] and is production-ready. Apart from the vast open source
community, it is being used in production at multiple Fortune 500 companies, and by
[Intuit Katlas](https://github.com/intuit/katlas) and
[VMware Purser](https://github.com/vmware/purser). A hosted version of Dgraph is available at
[https://cloud.dgraph.io](https://cloud.dgraph.io).

[rel]: https://github.com/hypermodeinc/dgraph/releases/tag/v24.0.5

## Supported Platforms

Dgraph officially supports the Linux/amd64 architecture. Support for Linux/arm64 is in development.
In order to take advantage of memory performance gains and other architecture-specific advancements
in Linux, we dropped official support Mac and Windows in 2021, see
[this blog post](https://discuss.hypermode.com/t/dropping-support-for-windows-and-mac/12913) for more
information. You can still build and use Dgraph on other platforms (for live or bulk loading for
instance), but support for platforms other than Linux/amd64 is not available.

Running Dgraph in a Docker environment is the recommended testing and deployment method.

## Install with Docker

If you're using Docker, you can use the
[official Dgraph image](https://hub.docker.com/r/dgraph/dgraph/).

```bash
docker pull dgraph/dgraph:latest
```

For more information on a variety Docker deployment methods including Docker Compose and Kubernetes,
see the [docs](https://docs.hypermode.com/dgraph/self-managed/overview).

## Run a Quick Standalone Cluster

```bash
docker run -it -p 8080:8080 -p 9080:9080 -v ~/dgraph:/dgraph dgraph/standalone:latest
```

## Install from Source

If you want to install from source, install Go 1.19+ or later and the following dependencies:

### Ubuntu

```bash
sudo apt-get update
sudo apt-get install build-essential
```

### Build and Install

Then clone the Dgraph repository and use `make install` to install the Dgraph binary in the
directory named by the GOBIN environment variable, which defaults to $GOPATH/bin or $HOME/go/bin if
the GOPATH environment variable is not set.

```bash
git clone https://github.com/hypermodeinc/dgraph.git
cd dgraph
make install
```

## Get Started

**To get started with Dgraph, follow:**

- [Installation to queries in 3 steps](https://docs.hypermode.com/dgraph/quickstart).
- Tutorial and presentation videos on
  [YouTube channel](https://www.youtube.com/channel/UCghE41LR8nkKFlR3IFTRO4w/featured).

## Is Dgraph the right choice for me?

- Do you have more than 10 SQL tables connected via foreign keys?
- Do you have sparse data, which doesn't elegantly fit into SQL tables?
- Do you want a simple and flexible schema, which is readable and maintainable over time?
- Do you care about speed and performance at scale?

If the answers to the above are YES, then Dgraph would be a great fit for your application. Dgraph
provides NoSQL like scalability while providing SQL like transactions and the ability to select,
filter, and aggregate data points. It combines that with distributed joins, traversals, and graph
operations, which makes it easy to build applications with it.

## Dgraph compared to other graph DBs

| Features                            | Dgraph                        | Neo4j                                                    | Janus Graph                           |
| ----------------------------------- | ----------------------------- | -------------------------------------------------------- | ------------------------------------- |
| Architecture                        | Sharded and Distributed       | Single server (+ replicas in enterprise)                 | Layer on top of other distributed DBs |
| Replication                         | Consistent                    | None in community edition (only available in enterprise) | Via underlying DB                     |
| Data movement for shard rebalancing | Automatic                     | Not applicable (all data lies on each server)            | Via underlying DB                     |
| Language                            | GraphQL inspired              | Cypher                                                   | Gremlin                               |
| Protocols                           | Grpc / HTTP + JSON / RDF      | Bolt + Cypher                                            | Websocket / HTTP                      |
| Transactions                        | Distributed ACID transactions | Single server ACID transactions                          | Not typically ACID                    |
| Full-Text Search                    | Native support                | Native support                                           | Via External Indexing System          |
| Regular Expressions                 | Native support                | Native support                                           | Via External Indexing System          |
| Geo Search                          | Native support                | External support only                                    | Via External Indexing System          |
| License                             | Apache 2.0                    | GPL v3                                                   | Apache 2.0                            |

## Users

- **Dgraph official documentation is present at [docs.hypermode.com/dgraph](https://docs.hypermode.com/dgraph).**
- For feature requests or questions, visit [https://discuss.hypermode.com](https://discuss.hypermode.com).
- Please see [releases tab](https://github.com/hypermodeinc/dgraph/releases) to find the latest
  release and corresponding release notes.
- Read about the latest updates from the Dgraph team [on our blog](https://hypermode.com/blog).
- Watch tech talks on our
  [YouTube channel](https://www.youtube.com/channel/UCghE41LR8nkKFlR3IFTRO4w/featured).

## Developers

Please see
  [Contributing to Dgraph](https://github.com/hypermodeinc/dgraph/blob/main/CONTRIBUTING.md) for
  guidelines on contributions.

## Client Libraries

The Dgraph team maintains several
[officially supported client libraries](https://docs.hypermode.com/dgraph/sdks/overview). There are also libraries
contributed by the community
[unofficial client libraries](https://docs.hypermode.com/dgraph/sdks/unofficial-clients#unofficial-dgraph-clients).

##

## Contact

- Please use [discuss.hypermode.com](https://discuss.hypermode.com) for documentation, questions, feature
  requests and discussions.
- Please use [GitHub Issues](https://github.com/hypermodeinc/dgraph/issues) for filing bugs or
  feature requests.
- Follow us on Twitter [@dgraphlabs](https://twitter.com/dgraphlabs).
