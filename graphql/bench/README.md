Compare performance of Auth vs Non-Auth Queries and Mutation. Queries were benchmarked against pre
generated dataset. We had two cases: Single Level Query and Deep Query For Mutation we benchmarked
add, delete and Multi level Mutation. We also compared the overhead of adding auth rules. Results
and other details are mentioned
<a href="https://discuss.dgraph.io/t/graphql-query-mutation-benchmarking-result/8604/"> here </a>

To regenerate the benchmark results run it once with Non-Auth schema `schema.graphql` and compare
the result by generating the benchmark with Auth schema `schema_auth.graphql`.

**GraphQL pre and post processing time:**<br>

```bash
Auth:
Benchmark Name                   | Pre Time      | Post Time  | Ratio of Processing Time by Actual Time
BenchmarkNestedQuery               144549ns        1410978ns     0.14%
BenchmarkOneLevelMutation          29422440ns      113091520ns   3.31%
BenchmarkMultiLevelMutation        19717340ns      7690352ns     2.24%

Non-Auth:
Benchmark Name                   | Pre Time      | Post Time  | Ratio of Processing Time by Actual Time
BenchmarkNestedQuery               117319ns        716261089ns    26.65%
BenchmarkOneLevelMutation          29643908ns      83077638ns     2.6%
BenchmarkMultiLevelMutation        20579295ns      53566488ns     6.2%
```

**Summary**:

```bash
Query:
Running the Benchmark:
Command:  go test -bench=. -benchtime=60s
  go test -bench=. -benchtime=60s
  goos: linux
  goarch: amd64
  pkg: github.com/hypermodeinc/dgraph/graphql/e2e/auth/bench
Auth
  BenchmarkNestedQuery-8                88         815315761 ns/op
  BenchmarkOneLevelQuery-8            4357          15626384 ns/op
Non-Auth
  BenchmarkNestedQuery-8                33        2218877846 ns/op
  BenchmarkOneLevelQuery-8            4446          16100509 ns/op


Mutation:
BenchmarkMutation: 100 owners, each having 100 restaurants
BenchmarkMultiLevelMutation: 20 restaurants, each having 20 cuisines, each cuisine having 20 dishes
BenchmarkOneLevelMutation: 10000 nodes

Auth:
BenchmarkMutation: 0.380893400s
BenchmarkMultiLevelMutation: 1.392922056s
BenchmarkOneLevelMutation:
Add Time: 9.42224304s
Delete Time: 1.150111483s

Non-Auth:
BenchmarkMutation: 0.464559706s
BenchmarkMultiLevelMutation: 1.440681796s
BenchmarkOneLevelMutation:
Add Time: 9.549761333s
Delete Time: 1.200276696s
```
