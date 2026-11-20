package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var latencyBounds = []time.Duration{
	100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
}

type measurement struct {
	total, available, overload, failures int
	latencies                            [16]int
}

func (m *measurement) record(duration time.Duration, status, expected int) {
	m.total++
	switch status {
	case expected:
		m.available++
	case http.StatusServiceUnavailable:
		m.overload++
	default:
		m.failures++
	}
	for i, bound := range latencyBounds {
		if duration <= bound {
			m.latencies[i]++
			return
		}
	}
	m.latencies[len(latencyBounds)]++
}

func (m measurement) quantile(percent int) string {
	rank, count := (m.total*percent+99)/100, 0
	for i, bound := range latencyBounds {
		count += m.latencies[i]
		if count >= rank {
			return bound.String()
		}
	}
	return "over-4s"
}

func main() {
	base := flag.String("url", "http://127.0.0.1:18082", "Target base URL")
	workload := flag.String("workload", "mixed", "json, redis, crud or mixed")
	parallel := flag.Int("concurrency", 10, "Number of concurrent clients")
	iterations := flag.Int("iterations", 1000, "Total workload iterations without a duration")
	duration := flag.Duration("duration", 0, "Time-boxed workload instead of a fixed iteration count")
	overload := flag.Bool("allow-overload", false, "Allow 503 while still rejecting unexpected failures")
	flag.Parse()
	if *parallel < 1 || *parallel > 500 || *iterations < 1 || *duration < 0 ||
		(*workload != "json" && *workload != "redis" && *workload != "crud" && *workload != "mixed") {
		fmt.Fprintln(os.Stderr, "invalid workload, concurrency, duration or iteration count")
		os.Exit(2)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost = *parallel, *parallel, *parallel
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	endpoint := strings.TrimRight(*base, "/")
	send := func(m *measurement, method, path, body string, expected int) bool {
		start := time.Now()
		request, err := http.NewRequest(method, endpoint+path, strings.NewReader(body))
		status := 0
		if err == nil {
			if body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response, err := client.Do(request)
			if err == nil {
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr == nil && closeErr == nil {
					status = response.StatusCode
				}
			}
		}
		m.record(time.Since(start), status, expected)
		return status == expected
	}

	start := time.Now()
	measurements := make([]measurement, *parallel)
	var workers sync.WaitGroup
	for i := range *parallel {
		workers.Go(func() {
			result := &measurements[i]
			path := "/records/load-" + strconv.Itoa(i)
			for iteration := i; (*duration > 0 && time.Since(start) < *duration) ||
				(*duration == 0 && iteration < *iterations); iteration += *parallel {
				switch *workload {
				case "json":
					send(result, http.MethodGet, "/sample", "", 200)
				case "redis":
					send(result, http.MethodGet, "/cache", "", 200)
				case "crud":
					if send(result, http.MethodPost, path, `{"name":"Ada"}`, 201) {
						send(result, http.MethodGet, path, "", 200)
						send(result, http.MethodPut, path, `{"name":"Grace"}`, 204)
						send(result, http.MethodDelete, path, "", 204)
					}
				case "mixed":
					switch (iteration / *parallel) % 3 {
					case 0:
						send(result, http.MethodGet, "/sample", "", 200)
					case 1:
						send(result, http.MethodGet, "/records/seed", "", 200)
					case 2:
						send(result, http.MethodGet, "/cache", "", 200)
					}
				}
			}
		})
	}
	workers.Wait()

	var totals measurement
	for _, m := range measurements {
		totals.total += m.total
		totals.available += m.available
		totals.overload += m.overload
		totals.failures += m.failures
		for i, count := range m.latencies {
			totals.latencies[i] += count
		}
	}
	elapsed := time.Since(start)
	fmt.Printf("workload=%s concurrency=%d elapsed=%s requests=%d rps=%.1f ok=%d overload=%d errors=%d p50_le=%s p95_le=%s p99_le=%s\n",
		*workload, *parallel, elapsed.Round(time.Millisecond), totals.total, float64(totals.total)/elapsed.Seconds(),
		totals.available, totals.overload, totals.failures, totals.quantile(50), totals.quantile(95), totals.quantile(99))
	if totals.available == 0 || totals.failures != 0 || (!*overload && totals.overload != 0) {
		os.Exit(1)
	}
}
