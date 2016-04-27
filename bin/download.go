package main

import (
	"os"
	"net"
	"log"
	"time"
	"sync"
	"strings"
	"net/url"
	"net/http"
	"io/ioutil"
	"encoding/csv"
	"path/filepath"
	"encoding/json"
)

// Track the hosts that we've already checked
var cache = make(map[string]bool)

// Track the pending jobs, 100000 max before blocking
var jobs = make(chan string, 100000)

// Track the paths for a given host
var paths = make(map[string][]string)

// Track the results of a test lookup
var testResults = make(map[string]bool)

// Holds the wait group before exiting
var wg sync.WaitGroup

// Hold the global loggers
var infoLogger *log.Logger
var errorLogger *log.Logger

// Global http client
var client = http.Client{
	// Timeout requests after 20 seconds
	Timeout: time.Duration(20 * time.Second),
}

// Global splunk encoder
var splunkEncoder *json.Encoder

// Test's structures
type Test struct {
	LastUpdated int                 `json:"last_updated"`
	Destination string              `json:"destination"`
	DestinationIp string            `json:"destination_ip"`
	DestinationHost string          `json:"destination_host"`
	Source string                   `json:"source"`
	SourceIp string                 `json:"source_ip"`
	SourceHost string               `json:"source_host"`
}

// TestResults's structures
type TestResult struct {
	ThroughputDstMin int64          `json:"throughput_dst_min"`
	ThroughputDstMax int64          `json:"throughput_dst_max"`
	ThroughputSrcMin int64          `json:"throughput_src_min"`
	ThroughputSrcMax int64          `json:"throughput_src_max"`
	ThroughputProtocol string       `json:"throughput_protocol"`
	ThroughputWeekMax int64         `json:"throughput_week_max"`
	ThroughputWeekMin int64         `json:"throughput_week_min"`
	ThroughputSrcAverage int64      `json:"throughput_src_average"`
	ThroughputDstAverage int64      `json:"throughput_dst_average"`
	SourceHost string               `json:"source_host"`
	SourceIp string                 `json:"source_ip"`
	DestinationHost string          `json:"destination_host"`
	DestinationIp string            `json:"destination_ip"`
	ThroughputBidirectional int64   `json:"throughput_bidirectional"`
	ThroughputLastUpdate int64      `json:"throughput_last_update"`
	ThroughputDuration int64        `json:"throughput_duration"`
}

// The output structure
type SplunkOutput struct {
	Target string                   `json:"target"`
	Via string                      `json:"via"`
	Tests []Test                    `json:"tests"`
	TestResults []TestResult        `json:"testResults"`
	Time string                     `json:"time"`
}

// Gets all IPs associated with a string
func getIPs(shost string) (results []net.IP) {
	// Ignore empty strings
	if shost == "" {
		return
	}
	// Try to parse it as an IP, if fails look it up
	if addr := net.ParseIP(shost); addr == nil {
		// Try to lookup the host
		addresses, err := net.LookupHost(shost)
		if err != nil {
			errorLogger.Println(err)
			return
		}
		// Loop each address
		for _, addr := range addresses {
			// Loop it up recursively
			results = append(results, getIPs(addr)...)
		}
	} else {
		// Add to results and return
		results = append(results, addr)
	}
	return
}

// Queues a job if not already in cache
func queueJob(shost string, via string) {
	// Lookup the shost
	addrs := getIPs(shost)
	// Loop each address
	for _, addr := range addrs {
		// Wrap in brackets as needed for IPv6
		target := addr.String()
		if addr.To4() == nil {
			target = "[" + target + "]"
		}
		// If a via was provided, add to list of paths to a host
		if via != "" {
			// Add to paths as needed
			if _, ok := paths[target]; !ok {
				paths[target] = []string{}
			}
			// Add to paths
			paths[target] = append(paths[target], via)
		}
		// Check if it's in the cache
		if _, ok := cache[target]; !ok {
			// Save it to the cache
			cache[target] = true
			// Queue a job for each
			infoLogger.Printf("Queueing host (%s) from addr: %s\n", shost, target)
			// Add to jobs queue
			jobs <- target
			wg.Add(1)
		}
	}
}

// Load up the cache files and add to the job queue
func readCache() {
	// Defer
	defer wg.Done()
	// Get the cache files
	cacheDir := "/var/data/cache/"
	caches, _ := ioutil.ReadDir(cacheDir)
	// Find the newest file
	newest := int64(0)
	dir := ""
	for _, entry := range caches {
		if (entry.Mode().IsDir()) {
			if (entry.ModTime().Unix() > newest) {
				newest = entry.ModTime().Unix()
				dir = entry.Name()
			}
		}
	}
	// Get all lists
	files, _ := filepath.Glob(cacheDir + dir + "/*/list.*")
	// Loop each cache file
	for _, file := range files {
		// Open the cache file
		f, err := os.Open(file)
		if err != nil {
			errorLogger.Println(err)
			continue
		}
		defer f.Close()
		infoLogger.Printf("Reading cache file: %s\n", file)
		// Load it as a PSV file
		r := csv.NewReader(f)
		r.Comma = '|'
		r.LazyQuotes = true
		records, err := r.ReadAll()
		if err != nil {
			errorLogger.Println(err)
			continue
		}
		// Loop each record
		for _, record := range records {
			// Parse the url
			url, err := url.Parse(record[0])
			if err != nil {
				errorLogger.Println(err)
				continue
			}
			// If there was a host/port
			if url.Host != "" {
				// Extract just the host
				shost, _, err := net.SplitHostPort(url.Host)
				if err != nil {
					errorLogger.Println(err)
					continue
				}
				// Queue a job for the host
				infoLogger.Printf("Queueing host (%s) from cache: %s\n", shost, file)
				queueJob(shost, "")
			}
		}
	}
}

// Get tests for a given target via path
func getTests(via string, target string) bool {
	// Let's get the hosts it tests against
	infoLogger.Printf("Getting tests for (%s) via: %s\n", target, via)
	testsResp, err := client.Get("http://" + via + "/perfsonar-graphs/graphData.cgi?action=test_list&url=http%3A%2F%2F" + target + "%2Fesmond%2Fperfsonar%2Farchive%2F")
	if err != nil {
		errorLogger.Println(err)
		return false
	}
	// If it wasn't a json response skip this host
	if !strings.Contains(testsResp.Header.Get("Content-Type"), "text/json") {
		errorLogger.Println("Not JSON")
		return false
	}
	defer testsResp.Body.Close()
	// Make a object for the tests to be stored in
	tests := []Test{}
	// Parse the body
	err = json.NewDecoder(testsResp.Body).Decode(&tests)
	if err != nil {
		errorLogger.Println(err)
		return false
	}
	// Loop each test
	for _, test := range tests {
		// Queue jobs for each known value
		infoLogger.Printf(
			"Queueing test hosts (%s %s %s %s %s %s) from host (%s) via: %s\n",
			test.Destination,
			test.DestinationIp,
			test.DestinationHost,
			test.Source,
			test.SourceIp,
			test.SourceHost,
			target,
			via,
		)
		queueJob(test.Source, target)
		queueJob(test.SourceIp, target)
		queueJob(test.SourceHost, target)
		queueJob(test.Destination, target)
		queueJob(test.DestinationIp, target)
		queueJob(test.DestinationHost, target)
	}
	testResultsResp, err := client.Get("http://" + via + "/perfsonar-graphs/graphData.cgi?action=tests&url=http%3A%2F%2F" + target + "%2Fesmond%2Fperfsonar%2Farchive%2F")
	if err != nil {
		errorLogger.Println(err)
		return false
	}
	// If it wasn't a json response skip this host
	if !strings.Contains(testResultsResp.Header.Get("Content-Type"), "text/json") {
		errorLogger.Println("Not JSON")
		return false
	}
	defer testResultsResp.Body.Close()
	// Make a object for the tests to be stored in
	testResults := []TestResult{}
	// Parse the body
	err = json.NewDecoder(testResultsResp.Body).Decode(&testResults)
	if err != nil {
		errorLogger.Println(err)
		return false
	}
	// Loop each test
	for _, testResult := range testResults {
		// Queue jobs for each known value
		infoLogger.Printf(
			"Queueing test result hosts (%s %s %s %s) from host (%s) via: %s\n",
			testResult.DestinationIp,
			testResult.DestinationHost,
			testResult.SourceIp,
			testResult.SourceHost,
			target,
			via,
		)
		queueJob(testResult.SourceIp, target)
		queueJob(testResult.SourceHost, target)
		queueJob(testResult.DestinationIp, target)
		queueJob(testResult.DestinationHost, target)
	}
	// Write it to the output file
	splunkEncoder.Encode(SplunkOutput{
		Target: target,
		Via: via,
		Tests: tests,
		TestResults: testResults,
		Time: time.Now().Format(time.UnixDate),
	})
	// Return success
	return true
}

// Worker handles each job as the come in
func worker(id int, jobs <-chan string) {
	for target := range jobs {
		infoLogger.Printf("Working job: %s\n", target)
		// Get the tests of localhost via target
		testResults[target] = getTests(target, "localhost")
		// Mark job done
		wg.Done()
	}
}

// Entry point
func main() {
	// Setup the loggers
	infoLogger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)
	errorLogger = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
	// Setup the output file file
	timeStamp := time.Now().Format(time.UnixDate) + ".json"
	outputFile, err := os.OpenFile(timeStamp, os.O_CREATE | os.O_RDWR, 0666)
	if err != nil {
		errorLogger.Fatal(err)
	}
	// Setup a JSON encoder on the file
	splunkEncoder = json.NewEncoder(outputFile)
	// Begin reading the cache and queueing jobs
	wg.Add(1)
	go readCache()
	// Spawn workers to complete the lookups concurrently
	for w := 1; w <= 50; w++ {
		go worker(w, jobs)
	}
	// Wait for all jobs to complete before continuing
	wg.Wait()
	// Loop each failed test lookup
	for target, testResult := range testResults {
		if !testResult {
			// If we have alternate paths to it
			if _, ok := paths[target]; ok {
				// Loop all the paths to it and try to lookup by that
				for _, path := range paths[target] {
					queueJob(target, path)
				}
			}
		}
	}
	// Wait for all alternate path jobs to complete before continuing
	wg.Wait()
}