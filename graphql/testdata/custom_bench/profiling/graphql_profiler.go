/*
 * Copyright 2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/golang/glog"
)

type PprofProfile string
type ProfilingOpts struct {
	parameterName string
	value         string
}

const (
	allocs       PprofProfile = "allocs"
	block        PprofProfile = "block"
	goroutine    PprofProfile = "goroutine"
	memory       PprofProfile = "heap"
	mutex        PprofProfile = "mutex"
	cpu          PprofProfile = "profile"
	threadCreate PprofProfile = "threadCreate"
	trace        PprofProfile = "trace"

	hostAlphaAuthority   = "http://localhost:8080"
	dockerAlphaAuthority = "http://localhost:8180"
	dockerZeroAuthority  = "http://localhost:6180"
	graphqlServerUrl     = hostAlphaAuthority + "/graphql"
	pprofUrl             = hostAlphaAuthority + "/debug/pprof"

	baseDir              = "."
	dataDir              = baseDir + "/__data"
	resultsDir           = baseDir + "/results"
	benchmarksDir        = baseDir + "/dgraph-benchmarks"
	benchQueriesDirName  = "queries"
	benchSchemaFileName  = "schema.graphql"
	tempDir              = baseDir + "/temp"
	dockerDgraphDir      = tempDir + "/docker"
	hostDgraphDir        = tempDir + "/host"
	dockerComposeFile    = baseDir + "/docker-compose.yml"
	initialGraphQLSchema = dataDir + "/schema.graphql"

	maxTxnTs = 110000
)

func main() {
	defer func() {
		r := recover()
		if r != nil {
			log.Println("Panic!!!")
			log.Println(r)
		}
	}()

	logFileName := "graphql_profiler.log"
	fmt.Println("Started Profiling, logging in: ", logFileName)
	f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		fmt.Println("unable to create logger file: ", logFileName)
		return
	}

	log.SetOutput(f)
	if err := makeAllDirs(); err != nil {
		log.Fatal(err)
	}
	if err := bootstrapApiServer(); err != nil {
		log.Fatal(err)
	}
	if err := collectProfilingData(); err != nil {
		log.Fatal(err)
	}
	if err := tearDownApiServer(); err != nil {
		log.Fatal(err)
	}
	if err := cleanup(); err != nil {
		log.Fatal(err)
	}
	log.Println("Successfully saved all profiling data.")
	fmt.Println("Profiling Complete.")
}

func makeAllDirs() error {
	if err := os.MkdirAll(resultsDir, os.ModePerm); err != nil {
		return err
	}
	if err := os.MkdirAll(tempDir, os.ModePerm); err != nil {
		return err
	}
	if err := os.MkdirAll(dockerDgraphDir, os.ModePerm); err != nil {
		return err
	}
	log.Println("Created all required dirs\n")
	return nil
}

func bootstrapApiServer() error {
	log.Println("BootStrapping API server ...")
	// make a temp copy of data dir for docker to run
	if err := exec.Command("cp", "-r", dataDir+"/.", dockerDgraphDir).Run(); err != nil {
		return err
	}
	log.Println("Copied data to temp docker dir")
	// copy docker-compose.yml to temp docker dir
	if err := exec.Command("cp", dockerComposeFile, dockerDgraphDir).Run(); err != nil {
		return err
	}
	log.Println("Copied docker-compose.yml to temp docker dir")
	// start dgraph in docker
	composeUpCmd := exec.Command("docker-compose", "up")
	composeUpCmd.Dir = dockerDgraphDir
	if err := composeUpCmd.Start(); err != nil {
		return err
	}
	log.Println("docker-compose up")
	// wait for it to be up
	if err := checkGraphqlHealth(dockerAlphaAuthority); err != nil {
		return err
	}
	log.Println("GraphQL layer is up")
	// set the maxTxnTs
	var resp *http.Response
	var err error
	if resp, err = http.Get(dockerZeroAuthority + "/assign?what=timestamps&num=" + strconv.Itoa(
		maxTxnTs+1)); err != nil || not2xx(resp.StatusCode) {
		return fmt.Errorf("resp: %v, err: %w", resp, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	log.Println("maxTxnTs is set, got response status-code: ", resp.StatusCode, ", body: ",
		string(respBody))
	log.Println("waiting for 5 seconds before applying initial schema ...")
	time.Sleep(5 * time.Second)

	// apply GraphQL schema
	if err := applySchema(dockerAlphaAuthority, initialGraphQLSchema); err != nil {
		return err
	}
	log.Println("applied initial GraphQL schema")

	// TODO : start the custom API service now

	log.Println("BootStrapping API server finished.\n")
	return nil
}

func tearDownApiServer() error {
	log.Println("Tearing down API server ...")
	// TODO: stop the custom API service

	// stop dgraph in docker
	composeDownCmd := exec.Command("docker-compose", "down")
	composeDownCmd.Dir = dockerDgraphDir
	if err := composeDownCmd.Run(); err != nil {
		return err
	}

	log.Println("API server is down now.\n")
	return nil
}

func cleanup() error {
	log.Println("Starting cleanup ...")
	// just remove the temp dir
	if err := exec.Command("rm", "-rf", tempDir).Run(); err != nil {
		return err
	}
	log.Println("Finished cleanup.\n")
	return nil
}

func startHostDgraphForProfiling(benchmarkDirName string) (*os.Process, *os.Process, error) {
	log.Println("Starting Dgraph on host ...")
	if err := os.MkdirAll(hostDgraphDir, os.ModePerm); err != nil {
		return nil, nil, err
	}

	zeroLogDir := filepath.Join(resultsDir, benchmarkDirName, "zero_logs")
	alphaLogDir := filepath.Join(resultsDir, benchmarkDirName, "alpha_logs")
	if err := os.MkdirAll(zeroLogDir, os.ModePerm); err != nil {
		log.Println(err)
	}
	if err := os.MkdirAll(alphaLogDir, os.ModePerm); err != nil {
		log.Println(err)
	}

	// make a temp copy of data dir for dgraph to run
	log.Println("cp dataDir hostDgraphDir")
	if err := exec.Command("cp", "-r", dataDir+"/.", hostDgraphDir).Run(); err != nil {
		return nil, nil, err
	}
	// start zero
	log.Println("dgraph zero")
	zeroCmd := exec.Command("dgraph", "zero", "--log_dir", zeroLogDir)
	zeroCmd.Dir = hostDgraphDir
	zeroCmd.Stderr = io.Discard
	zeroCmd.Stdout = io.Discard
	zeroCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zeroCmd.Start(); err != nil {
		return zeroCmd.Process, nil, err
	}
	// start alpha
	log.Println("dgraph alpha")
	alphaCmd := exec.Command("dgraph", "alpha", "--log_dir", alphaLogDir)
	alphaCmd.Dir = hostDgraphDir
	alphaCmd.Stderr = io.Discard
	alphaCmd.Stdout = io.Discard
	alphaCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := alphaCmd.Start(); err != nil {
		return zeroCmd.Process, alphaCmd.Process, err
	}
	// wait for it to start
	if err := checkGraphqlHealth(hostAlphaAuthority); err != nil {
		return zeroCmd.Process, alphaCmd.Process, err
	}
	log.Println("GraphQL layer is up")
	// apply schema
	schemaFilePath := filepath.Join(benchmarksDir, benchmarkDirName, benchSchemaFileName)
	if err := applySchema(hostAlphaAuthority, schemaFilePath); err != nil {
		return zeroCmd.Process, alphaCmd.Process, err
	}
	log.Println("applied GraphQL schema: ", schemaFilePath)
	log.Println("Starting Dgraph on host finished.\n")
	return zeroCmd.Process, alphaCmd.Process, nil
}

func stopHostDgraph(zeroProc, alphaProc *os.Process) {
	log.Println("Stopping Dgraph on host ...")
	if alphaProc != nil {
		log.Println("alpha PID: ", alphaProc.Pid)
		if err := syscall.Kill(-alphaProc.Pid, syscall.SIGKILL); err != nil {
			log.Println(err)
		}
		log.Println("Sent kill to alpha")
	}

	if zeroProc != nil {
		log.Println("zero PID: ", zeroProc.Pid)
		if err := syscall.Kill(-zeroProc.Pid, syscall.SIGKILL); err != nil {
			log.Println(err)
		}
		log.Println("Sent kill to zero")
	}
	log.Println("waiting for alpha and zero processes to exit")
	if alphaProc != nil {
		if _, err := alphaProc.Wait(); err != nil {
			log.Println(err)
		}
		if _, err := syscall.Wait4(-alphaProc.Pid, nil, 0, nil); err != nil {
			log.Println(err)
		}
		log.Println("alpha exited")
	}
	if zeroProc != nil {
		if _, err := zeroProc.Wait(); err != nil {
			log.Println(err)
		}
		if _, err := syscall.Wait4(-zeroProc.Pid, nil, 0, nil); err != nil {
			log.Println(err)
		}
		log.Println("zero exited")
	}

	// now remove the host dgraph dir which was created when the zero and alpha were started
	removeDir(hostDgraphDir + "/p")
	removeDir(hostDgraphDir + "/w")
	removeDir(hostDgraphDir + "/zw")
	removeDir(hostDgraphDir)
	log.Println("Stopping Dgraph on host finished.\n")
}

func removeDir(dir string) {
	log.Println("rm -rf ", dir)
	b := &bytes.Buffer{}
	rmCmd := exec.Command("rm", "-rf", dir)
	rmCmd.Stdout = b
	rmCmd.Stderr = b
	if err := rmCmd.Run(); err != nil {
		log.Println(b.String())
		log.Println(err)
	}
}

func collectProfilingData() error {
	log.Println("Starting to collect profiling data ...\n")
	benchmarkDirs, err := os.ReadDir(benchmarksDir)
	if err != nil {
		return err
	}

	for _, benchmarkDir := range benchmarkDirs {
		log.Println("Going to profile benchmark: ", benchmarkDir.Name())

		skipBenchmark := func(err error) {
			log.Println(err)
			log.Println("Skipping benchmark: ", benchmarkDir.Name())
		}

		benchResultsDir := filepath.Join(resultsDir, benchmarkDir.Name())
		if err := os.MkdirAll(benchResultsDir, os.ModePerm); err != nil {
			skipBenchmark(err)
			continue
		}

		benchQueriesDir := filepath.Join(benchmarksDir, benchmarkDir.Name(), benchQueriesDirName)
		queryFiles, err := os.ReadDir(benchQueriesDir)
		if err != nil {
			skipBenchmark(err)
			continue
		}

		zeroProc, alphaProc, err := startHostDgraphForProfiling(benchmarkDir.Name())
		if err != nil {
			skipBenchmark(err)
			stopHostDgraph(zeroProc, alphaProc)
			continue
		}

		avgStats := make([]*DurationStats, 0, len(queryFiles))
		respDataSizes := make([]int, 0, len(queryFiles))

		for _, queryFile := range queryFiles {
			log.Println("Going to profile query: ", queryFile.Name())

			skipQuery := func(err error) {

				log.Println(err)
				log.Println("Skipping query file: ", queryFile.Name())
				avgStats = append(avgStats, nil)
				respDataSizes = append(respDataSizes, 0)
			}

			f, err := os.Open(filepath.Join(benchQueriesDir, queryFile.Name()))
			if err != nil {
				skipQuery(err)
				continue
			}

			b, err := io.ReadAll(f)
			if err != nil {
				skipQuery(err)
				continue
			}
			query := string(b)

			queryResultsDir := filepath.Join(benchResultsDir, queryFile.Name())
			if err := os.MkdirAll(queryResultsDir, os.ModePerm); err != nil {
				skipQuery(err)
				continue
			}

			// find the ideal CPU profiling time
			cpuProfilingOpts := getCpuProfilingOpts(query)

			n := 10
			rttDurations := make([]int64, 0, n)
			totalActualDurations := make([]int64, 0, n)
			totalExtDurations := make([]int64, 0, n)
			dgraphDurations := make([]int64, 0, n)
			respDataSize := 0
			// run each query n times, so we will judge based on the average of all these
			log.Printf("Now, Going to run this query %d times\n", n)
			for i := 0; i < n; i++ {
				log.Println("Starting Iteration: ", i)

				wg := sync.WaitGroup{}
				wg.Add(3)

				go func() {
					defer wg.Done()
					saveProfile(memory, filepath.Join(queryResultsDir, fmt.Sprintf("%02d",
						i)+"_"+string(memory)+"_pre.prof"), nil)
				}()
				go func() {
					defer wg.Done()
					saveProfile(cpu, filepath.Join(queryResultsDir, fmt.Sprintf("%02d",
						i)+"_"+string(cpu)+".prof"), cpuProfilingOpts)
				}()
				go func() {
					defer wg.Done()

					threadSync := int32(0)

					// this will make the graphql request async
					go func() {
						// we set it to 1 here to signal the exit of this thread
						defer atomic.StoreInt32(&threadSync, 1)

						resp, rtt, actualTime, err := makeGqlRequest(query)
						if err != nil {
							log.Println(err)
						} else if resp != nil && respDataSize <= 0 {
							respDataSize = resp.dataSize
						}
						saveProfile(memory, filepath.Join(queryResultsDir, fmt.Sprintf("%02d",
							i)+"_"+string(memory)+"_post.prof"), nil)
						totalDuration, dgraphDuration, err := saveTracing(resp, queryResultsDir, i)
						if err != nil {
							log.Println(err)
						}
						rttDurations = append(rttDurations, rtt)
						totalActualDurations = append(totalActualDurations, actualTime)
						totalExtDurations = append(totalExtDurations, totalDuration)
						dgraphDurations = append(dgraphDurations, dgraphDuration)
					}()

					// while graphql request doesn't come back, keep collecting profiling data
					for j := 0; !atomic.CompareAndSwapInt32(&threadSync, 1, 0); j++ {
						saveProfile(memory, filepath.Join(queryResultsDir, fmt.Sprintf("%02d",
							i)+"_"+string(memory)+"_during_"+fmt.Sprintf("%02d", j)+".prof"), nil)
						// two cpu profiles can't run together
						//saveProfile(cpu, filepath.Join(queryResultsDir, fmt.Sprintf("%02d",
						//	i)+"_"+string(cpu)+"_during_"+fmt.Sprintf("%02d", j)+".prof"), smallCpuProfileOpts)
						time.Sleep(5 * time.Second)
					}
				}()

				// wait till all the profiling data has been collected for this run
				wg.Wait()
				log.Println("Finished Iteration: ", i)
			}
			log.Println("Completed running this query 10 times")

			// save GraphQL layer durations for all runs, and their computed avg.
			qryAvg := saveQueryDurations(rttDurations, totalActualDurations,
				totalExtDurations, dgraphDurations, respDataSize, queryResultsDir)
			avgStats = append(avgStats, qryAvg)
			respDataSizes = append(respDataSizes, respDataSize)

			log.Println("Completed profiling query: ", queryFile.Name())
		}

		saveBenchmarkStats(queryFiles, respDataSizes, avgStats, benchResultsDir)

		stopHostDgraph(zeroProc, alphaProc)

		log.Println("Completed profiling benchmark: ", benchmarkDir.Name())
	}

	log.Println("Collected all profiling data.\n")
	return nil
}

func applySchema(alphaAuthority string, schemaFilePath string) error {
	f, err := os.Open(schemaFilePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	resp, err := http.Post(alphaAuthority+"/admin/schema", "", f)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var httpBody map[string]interface{}
	if err := json.Unmarshal(b, &httpBody); err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK || httpBody["data"] == nil {
		return fmt.Errorf("error while updating schema: status-code = %d, body = %s",
			resp.StatusCode, string(b))
	}

	return nil
}

func not2xx(statusCode int) bool {
	if statusCode < 200 || statusCode > 299 {
		return true
	}
	return false
}

func checkGraphqlHealth(alphaUrl string) error {
	healthCheckStart := time.Now()
	for {
		resp, err := http.Get(alphaUrl + "/probe/graphql")

		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}

		// make sure we wait only for 60 secs
		if time.Since(healthCheckStart).Seconds() > 60 {
			return fmt.Errorf("503 Service Unavailable: %s", alphaUrl)
		}

		// check health every second
		time.Sleep(time.Second)
	}
}

func getCpuProfilingOpts(query string) *ProfilingOpts {
	_, rtt, _, err := makeGqlRequest(query)
	if err != nil {
		// just trying twice if it errors for some random reason the first time
		_, rtt, _, _ = makeGqlRequest(query)
	}
	// convert nanosecs -> secs
	rtt /= 1000000000

	if rtt < 10 {
		// there should be at least 10 secs of CPU profile
		rtt = 10
	}
	// always collect 5 more secs of CPU profile data
	rtt += 5

	return &ProfilingOpts{parameterName: "seconds", value: strconv.FormatInt(rtt, 10)}
}

func saveProfile(profType PprofProfile, profilePath string, profileOpts *ProfilingOpts) {
	url := pprofUrl + "/" + string(profType)
	if profileOpts != nil {
		url += "?" + profileOpts.parameterName + "=" + profileOpts.value
	}
	log.Println("Sending profile request: ", url)
	resp, err := http.Get(url)
	if err != nil {
		log.Println(err)
		return
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		return
	}

	f, err := os.Create(profilePath)
	if err != nil {
		log.Println("could not create file: ", profilePath, ", err: ", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	_, err = f.Write(b)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Saved profile ", profilePath)
}

func saveTracing(resp *Response, outputDir string, iteration int) (int64, int64, error) {
	if resp == nil {
		return 0, 0, nil
	}

	f, err := os.OpenFile(filepath.Join(outputDir, fmt.Sprintf("%02d", iteration)+"_tracing.txt"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		log.Println(err)
		return 0, 0, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			glog.Warningf("error while closing fd: %v", err)
		}
	}()

	b, err := json.Marshal(resp)
	if err != nil {
		log.Println(err)
		return 0, 0, err
	}

	if _, err := f.Write(b); err != nil {
		log.Println(err)
		return 0, 0, err
	}

	// return total and dgraph durations
	totalDgraphDuration := int64(0)
	for _, reoslverTrace := range resp.Extensions.Tracing.Execution.Resolvers {
		for _, dgraphTrace := range reoslverTrace.Dgraph {
			totalDgraphDuration += dgraphTrace.Duration
		}
	}
	return resp.Extensions.Tracing.Duration, totalDgraphDuration, nil
}

// save total, Dgraph and GraphQL layer durations for all query runs, and their computed avg.
func saveQueryDurations(rttDurations, totalActualDurations, totalExtDurations, dgraphDurations []int64,
	dataSize int, outputDir string) *DurationStats {
	durationsFileName := filepath.Join(outputDir, "_Durations.txt")
	f, err := os.OpenFile(durationsFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		log.Println(err)
		return nil
	}

	var strBuilder strings.Builder
	strBuilder.WriteString(fmt.Sprintf("len(data) = %d bytes", dataSize))
	strBuilder.WriteString(`

|==========================================================================================================================================================================================================|
| Iteration | Round Trip Time (RTT) | Total Time (TT) | Trace Time (TrT) | Dgraph Time (DT) | GraphQL Time (GT=TT-DT) | % GraphQL Time (GT/TT*100) | Tracing Error (TT-TrT) | Round Trip Overhead (RTT-TT) |
|===========|=======================|=================|==================|==================|=========================|============================|========================|==============================|`)

	rttDurationSum := int64(0)
	totalActualDurationSum := int64(0)
	totalExtDurationSum := int64(0)
	dgraphDurationSum := int64(0)
	graphqlDurationSum := int64(0)
	tracingErrorSum := int64(0)
	rttOverheadSum := int64(0)

	for i, actualDuration := range totalActualDurations {
		graphqlDuration := actualDuration - dgraphDurations[i]
		tracingError := actualDuration - totalExtDurations[i]
		rttOverhead := rttDurations[i] - actualDuration

		strBuilder.WriteString(fmt.Sprintf(`
| %9d | %21d | %15d | %16d | %16d | %23d | %26.2f | %22d | %28d |`,
			i, rttDurations[i], actualDuration, totalExtDurations[i], dgraphDurations[i],
			graphqlDuration, float64(graphqlDuration)/float64(actualDuration)*100,
			tracingError, rttOverhead))

		rttDurationSum += rttDurations[i]
		totalActualDurationSum += actualDuration
		totalExtDurationSum += totalExtDurations[i]
		dgraphDurationSum += dgraphDurations[i]
		graphqlDurationSum += graphqlDuration
		tracingErrorSum += tracingError
		rttOverheadSum += rttOverhead
	}

	avg := &DurationStats{
		RoundTrip:         float64(rttDurationSum) / float64(len(rttDurations)),
		Total:             float64(totalActualDurationSum) / float64(len(totalActualDurations)),
		Trace:             float64(totalExtDurationSum) / float64(len(totalExtDurations)),
		Dgraph:            float64(dgraphDurationSum) / float64(len(dgraphDurations)),
		GraphQL:           float64(graphqlDurationSum) / float64(len(totalActualDurations)),
		TraceError:        float64(tracingErrorSum) / float64(len(totalActualDurations)),
		RoundTripOverhead: float64(rttOverheadSum) / float64(len(totalActualDurations)),
	}
	avg.GraphQLPercent = avg.GraphQL / avg.Total * 100

	strBuilder.WriteString(fmt.Sprintf(`
|===========|=======================|=================|==================|==================|=========================|============================|========================|==============================|
|    Avg    | %21.2f | %15.2f | %16.2f | %16.2f | %23.2f | %26.2f | %22.2f | %28.2f |
|==========================================================================================================================================================================================================|`,
		avg.RoundTrip, avg.Total, avg.Trace, avg.Dgraph, avg.GraphQL, avg.GraphQLPercent,
		avg.TraceError, avg.RoundTripOverhead))

	strBuilder.WriteString(`

All timing information is in nanoseconds, except '% GraphQL Time' which is a percentage.
`)

	if _, err := f.WriteString(strBuilder.String()); err != nil {
		log.Println(err)
	}
	log.Println("Saved GraphQL layer durations in: ", durationsFileName)
	return avg
}

func saveBenchmarkStats(queryFiles []os.DirEntry, respDataSizes []int, avgStats []*DurationStats,
	outputDir string) {
	statsFileName := filepath.Join(outputDir, "_Stats.txt")
	f, err := os.OpenFile(statsFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		log.Println(err)
		return
	}

	var strBuilder strings.Builder
	strBuilder.WriteString(`
|===================================================================================================================================================================================================================================================================|
| QueryFile Name | len(data) (bytes) | Avg Round Trip Time (RTT) | Avg Total Time (TT) | Avg Trace Time (TrT) | Avg Dgraph Time (DT) | Avg GraphQL Time (GT=TT-DT) | Avg % GraphQL Time (GT/TT*100) | Avg Tracing Error (TT-TrT) | Avg Round Trip Overhead (RTT-TT) |
|================|===================|===========================|=====================|======================|======================|=============================|================================|============================|==================================|`)

	for i, stat := range avgStats {
		strBuilder.WriteString(fmt.Sprintf(`
| %14s | %17d | %25.2f | %19.2f | %20.2f | %20.2f | %27.2f | %30.2f | %26.2f | %32.2f |`,
			queryFiles[i].Name(), respDataSizes[i], stat.RoundTrip, stat.Total, stat.Trace,
			stat.Dgraph, stat.GraphQL, stat.GraphQLPercent, stat.TraceError,
			stat.RoundTripOverhead))
	}

	strBuilder.WriteString(`
|===================================================================================================================================================================================================================================================================|

All timing information is in nanoseconds, except 'Avg % GraphQL Time' which is a percentage.
`)

	if _, err := f.WriteString(strBuilder.String()); err != nil {
		log.Println(err)
	}

	log.Println("Saved Benchmark stats in: ", statsFileName)
}

type DurationStats struct {
	RoundTrip         float64
	Total             float64
	Trace             float64
	Dgraph            float64
	GraphQL           float64
	GraphQLPercent    float64
	TraceError        float64
	RoundTripOverhead float64
}

type Response struct {
	Errors     interface{}
	Extensions struct {
		TouchedUids uint64 `json:"touched_uids,omitempty"`
		Tracing     struct {
			// Timestamps in RFC 3339 nano format.
			StartTime string `json:"startTime,"`
			EndTime   string `json:"endTime"`
			// Duration in nanoseconds, relative to the request start, as an integer.
			Duration  int64 `json:"duration"`
			Execution struct {
				Resolvers []struct {
					// the response path of the current resolver - same format as path in error
					// result format specified in the GraphQL specification
					Path       []interface{} `json:"path"`
					ParentType string        `json:"parentType"`
					FieldName  string        `json:"fieldName"`
					ReturnType string        `json:"returnType"`
					// Offset relative to request start and total duration or resolving
					OffsetDuration
					// Dgraph isn't in Apollo tracing.  It records the offsets and times
					// of Dgraph operations for the query/mutation (including network latency)
					// in nanoseconds.
					Dgraph []struct {
						Label string `json:"label"`
						OffsetDuration
					} `json:"dgraph"`
				} `json:"resolvers"`
			} `json:"execution,omitempty"`
		} `json:"tracing,omitempty"`
	}
	dataSize int
}

type OffsetDuration struct {
	// Offset in nanoseconds, relative to the request start, as an integer
	StartOffset int64 `json:"startOffset"`
	// Duration in nanoseconds, relative to start of operation, as an integer.
	Duration int64 `json:"duration"`
}

type GraphQLParams struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

// returns the GQL Response, return-trip time and error
func makeGqlRequest(query string) (*Response, int64, int64, error) {
	rtt := int64(0)
	params := GraphQLParams{
		Query: query,
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, rtt, 0, err
	}

	req, err := http.NewRequest(http.MethodPost, graphqlServerUrl, bytes.NewBuffer(b))
	if err != nil {
		return nil, rtt, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	httpClient := http.Client{}
	reqStartTime := time.Now()
	resp, err := httpClient.Do(req)
	rtt = time.Since(reqStartTime).Nanoseconds()
	if err != nil {
		return nil, rtt, 0, err
	}

	totalProcessingTime, _ := strconv.Atoi(resp.Header.Get("Graphql-Time"))

	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, rtt, int64(totalProcessingTime), err
	}

	dataStartIdx := bytes.Index(b, []byte(`"data":`))
	dataEndIdx := bytes.LastIndex(b, []byte(`,"extensions":`))

	gqlResp := Response{
		dataSize: dataEndIdx - dataStartIdx - 7,
	}
	err = json.Unmarshal(b, &gqlResp)

	return &gqlResp, rtt, int64(totalProcessingTime), err
}
