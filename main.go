package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	URL           string
	Batch         int
	Interval      time.Duration
	Duration      time.Duration
	Timeout       time.Duration
	MaxQuestionID int
	AnswerChoices int
	ErrorsLog     string
}

type Result struct {
	Wave       int
	StatusCode int
	Latency    time.Duration
	ErrType    string
	ErrMsg     string
	Body       string
}

type Metrics struct {
	Min, Max, Avg, P50, P95, P99 time.Duration
}

func classifyError(err error, statusCode int) string {
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "timeout"
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return "connection_refused"
		}
		return "unknown"
	}
	if statusCode >= 500 {
		return "server_error"
	}
	if statusCode >= 400 {
		return "client_error"
	}
	if statusCode >= 200 && statusCode < 300 {
		return ""
	}
	return "unknown"
}

func randomStudentID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "stu_" + hex.EncodeToString(b)
}

func computeMetrics(latencies []time.Duration) Metrics {
	if len(latencies) == 0 {
		return Metrics{}
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	pct := func(p float64) time.Duration {
		idx := int(float64(len(sorted))*p + 0.999999)
		if idx < 1 {
			idx = 1
		}
		if idx > len(sorted) {
			idx = len(sorted)
		}
		return sorted[idx-1]
	}

	return Metrics{
		Min: sorted[0],
		Max: sorted[len(sorted)-1],
		Avg: sum / time.Duration(len(sorted)),
		P50: pct(0.50),
		P95: pct(0.95),
		P99: pct(0.99),
	}
}

type requestBody struct {
	QuestionID int    `json:"questionId"`
	Answer     int    `json:"answer"`
	StudentID  string `json:"studentId"`
}

func doRequest(client *http.Client, cfg Config, wave int, rng *mathrand.Rand) Result {
	body := requestBody{
		QuestionID: rng.Intn(cfg.MaxQuestionID) + 1,
		Answer:     rng.Intn(cfg.AnswerChoices),
		StudentID:  randomStudentID(),
	}
	payload, _ := json.Marshal(body)

	start := time.Now()
	resp, err := client.Post(cfg.URL, "application/json", bytes.NewReader(payload))
	latency := time.Since(start)

	r := Result{Wave: wave, Latency: latency}

	if err != nil {
		r.ErrType = classifyError(err, 0)
		r.ErrMsg = err.Error()
		return r
	}
	defer resp.Body.Close()

	r.StatusCode = resp.StatusCode
	r.ErrType = classifyError(nil, resp.StatusCode)

	if r.ErrType != "" {
		limited := io.LimitReader(resp.Body, 500)
		b, _ := io.ReadAll(limited)
		r.Body = string(b)
		r.ErrMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return r
}

func worker(client *http.Client, cfg Config, wave int, resultsCh chan<- Result, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			resultsCh <- Result{
				Wave:    wave,
				ErrType: "unknown",
				ErrMsg:  fmt.Sprintf("panic: %v", rec),
			}
		}
	}()
	rng := mathrand.New(mathrand.NewSource(time.Now().UnixNano() ^ int64(wave)))
	resultsCh <- doRequest(client, cfg, wave, rng)
}

type Reporter struct {
	allResults  []Result
	statusCount map[int]int
	errorCount  map[string]int
	errorsFile  *os.File
	totalErrors int
}

func newReporter(errorsLogPath string) (*Reporter, error) {
	f, err := os.Create(errorsLogPath)
	if err != nil {
		return nil, fmt.Errorf("errors.log oluşturulamadı: %w", err)
	}
	return &Reporter{
		statusCount: map[int]int{},
		errorCount:  map[string]int{},
		errorsFile:  f,
	}, nil
}

func (r *Reporter) record(results []Result) {
	for _, res := range results {
		r.allResults = append(r.allResults, res)
		if res.StatusCode != 0 {
			r.statusCount[res.StatusCode]++
		}
		if res.ErrType != "" {
			r.errorCount[res.ErrType]++
			r.totalErrors++
			ts := time.Now().UTC().Format(time.RFC3339)
			body := strings.ReplaceAll(res.Body, "\n", " ")
			fmt.Fprintf(r.errorsFile,
				"%s wave=%d type=%s latency=%dms status=%d err=%q body=%q\n",
				ts, res.Wave, res.ErrType, res.Latency.Milliseconds(), res.StatusCode, res.ErrMsg, body)
		}
	}
}

func (r *Reporter) printWaveSummary(wave int, results []Result, waveDuration time.Duration) {
	var success, failed int
	latencies := make([]time.Duration, 0, len(results))
	errs := map[string]int{}
	for _, res := range results {
		latencies = append(latencies, res.Latency)
		if res.ErrType == "" {
			success++
		} else {
			failed++
			errs[res.ErrType]++
		}
	}
	m := computeMetrics(latencies)
	rps := float64(len(results)) / waveDuration.Seconds()

	successPct := 0.0
	failedPct := 0.0
	if len(results) > 0 {
		successPct = 100.0 * float64(success) / float64(len(results))
		failedPct = 100.0 * float64(failed) / float64(len(results))
	}

	fmt.Printf("\n═══ Wave %d ═══ (%.1fs)\n", wave, waveDuration.Seconds())
	fmt.Printf("  Sent:        %d\n", len(results))
	fmt.Printf("  Success:     %d (%.1f%%)\n", success, successPct)
	fmt.Printf("  Failed:      %d (%.1f%%)\n", failed, failedPct)
	fmt.Printf("  Latency:     min=%s  avg=%s  p50=%s  p95=%s  p99=%s  max=%s\n",
		m.Min.Round(time.Millisecond), m.Avg.Round(time.Millisecond),
		m.P50.Round(time.Millisecond), m.P95.Round(time.Millisecond),
		m.P99.Round(time.Millisecond), m.Max.Round(time.Millisecond))
	if len(errs) > 0 {
		parts := []string{}
		for k, v := range errs {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v))
		}
		fmt.Printf("  Errors:      %s\n", strings.Join(parts, "  "))
	}
	fmt.Printf("  Throughput:  %.1f req/s\n", rps)
}

func (r *Reporter) printFinalReport(totalDuration time.Duration, waves int) {
	total := len(r.allResults)
	var success, failed int
	latencies := make([]time.Duration, 0, total)
	for _, res := range r.allResults {
		latencies = append(latencies, res.Latency)
		if res.ErrType == "" {
			success++
		} else {
			failed++
		}
	}
	m := computeMetrics(latencies)
	rps := float64(total) / totalDuration.Seconds()

	successPct := 0.0
	failedPct := 0.0
	if total > 0 {
		successPct = 100.0 * float64(success) / float64(total)
		failedPct = 100.0 * float64(failed) / float64(total)
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║                   FINAL REPORT                           ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║ Duration:      %-42s║\n", totalDuration.Round(100*time.Millisecond))
	fmt.Printf("║ Waves:         %-42d║\n", waves)
	fmt.Printf("║ Total Sent:    %-42d║\n", total)
	fmt.Printf("║ Total Success: %-42s║\n", fmt.Sprintf("%d (%.1f%%)", success, successPct))
	fmt.Printf("║ Total Failed:  %-42s║\n", fmt.Sprintf("%d (%.1f%%)", failed, failedPct))
	fmt.Println("║                                                          ║")
	fmt.Println("║ Latency (overall):                                       ║")
	fmt.Printf("║   min     = %-45s║\n", m.Min.Round(time.Millisecond))
	fmt.Printf("║   avg     = %-45s║\n", m.Avg.Round(time.Millisecond))
	fmt.Printf("║   p50     = %-45s║\n", m.P50.Round(time.Millisecond))
	fmt.Printf("║   p95     = %-45s║\n", m.P95.Round(time.Millisecond))
	fmt.Printf("║   p99     = %-45s║\n", m.P99.Round(time.Millisecond))
	fmt.Printf("║   max     = %-45s║\n", m.Max.Round(time.Millisecond))
	if len(r.statusCount) > 0 {
		fmt.Println("║                                                          ║")
		fmt.Println("║ HTTP Status Distribution:                                ║")
		for code, count := range r.statusCount {
			fmt.Printf("║   %-3d                 : %-32d║\n", code, count)
		}
	}
	if len(r.errorCount) > 0 {
		fmt.Println("║                                                          ║")
		fmt.Println("║ Error Distribution:                                      ║")
		for k, v := range r.errorCount {
			fmt.Printf("║   %-20s : %-32d║\n", k, v)
		}
	}
	fmt.Println("║                                                          ║")
	fmt.Printf("║ Effective RPS:        %-35.1f║\n", rps)
	if r.totalErrors > 0 {
		fmt.Printf("║ Errors logged to:     %-35s║\n", fmt.Sprintf("errors.log (%d entries)", r.totalErrors))
	}
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
}

func (r *Reporter) close() {
	if r.errorsFile != nil {
		_ = r.errorsFile.Close()
	}
}

func runTest(cfg Config) error {
	transport := &http.Transport{
		MaxIdleConns:        cfg.Batch * 2,
		MaxIdleConnsPerHost: cfg.Batch * 2,
		MaxConnsPerHost:     cfg.Batch * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}

	reporter, err := newReporter(cfg.ErrorsLog)
	if err != nil {
		return err
	}
	defer reporter.close()

	fmt.Printf("Starting load test:\n")
	fmt.Printf("  URL:      %s\n", cfg.URL)
	fmt.Printf("  Batch:    %d requests/wave\n", cfg.Batch)
	fmt.Printf("  Interval: %s between waves\n", cfg.Interval)
	fmt.Printf("  Duration: %s total\n", cfg.Duration)
	fmt.Printf("  Timeout:  %s per request\n", cfg.Timeout)

	start := time.Now()
	wave := 0

	for time.Since(start) < cfg.Duration {
		wave++
		var wg sync.WaitGroup
		resultsCh := make(chan Result, cfg.Batch)

		waveStart := time.Now()
		for i := 0; i < cfg.Batch; i++ {
			wg.Add(1)
			go worker(client, cfg, wave, resultsCh, &wg)
		}
		wg.Wait()
		close(resultsCh)

		waveResults := make([]Result, 0, cfg.Batch)
		for res := range resultsCh {
			waveResults = append(waveResults, res)
		}
		waveDuration := time.Since(waveStart)

		reporter.printWaveSummary(wave, waveResults, waveDuration)
		reporter.record(waveResults)

		if time.Since(start)+cfg.Interval < cfg.Duration {
			time.Sleep(cfg.Interval)
		} else {
			break
		}
	}

	totalDuration := time.Since(start)
	reporter.printFinalReport(totalDuration, wave)
	return nil
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.URL, "url", "https://api.canavar.online/api/tester/answer", "Hedef endpoint URL")
	flag.IntVar(&cfg.Batch, "batch", 500, "Her dalgadaki eşzamanlı istek sayısı")
	flag.DurationVar(&cfg.Interval, "interval", 5*time.Second, "Dalgalar arası bekleme")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "Toplam test süresi")
	flag.DurationVar(&cfg.Timeout, "timeout", 10*time.Second, "Her isteğin timeout süresi")
	flag.IntVar(&cfg.MaxQuestionID, "max-question-id", 25, "questionId 1..N rastgele üretilir")
	flag.IntVar(&cfg.AnswerChoices, "answer-choices", 4, "answer 0..N-1 rastgele üretilir")
	flag.StringVar(&cfg.ErrorsLog, "errors-log", "errors.log", "Hata log dosyası yolu")
	flag.Parse()

	if cfg.Batch < 1 {
		log.Fatal("batch en az 1 olmalı")
	}
	if cfg.MaxQuestionID < 1 {
		log.Fatal("max-question-id en az 1 olmalı")
	}
	if cfg.AnswerChoices < 1 {
		log.Fatal("answer-choices en az 1 olmalı")
	}

	if err := runTest(cfg); err != nil {
		log.Fatal(err)
	}
}
