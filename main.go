package main

import (
	"bytes"
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
	"sync/atomic"
	"syscall"
	"time"
)

// questions target için varsayılan Bearer token (test ortamı, deneme2 sınavı, ~1 yıl geçerli).
// -token flag'i ile override edilebilir. Geçersiz olursa yenisini buraya yapıştır veya -token ver.
const defaultToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1c2VySWQiOiI2YTI5OWEwNzJmODVjYWQwNTI4NzNjZDciLCJyb2xlIjoidXNlciIsInNlc3Npb25JZCI6ImNvNWQ3eDZ4cTVtcThib2xkZiIsImlhdCI6MTc4MTExMTMwMywiZXhwIjoxODEyNjQ3MzAzfQ.BobaIR1wt9Zz_gp9rcPCLnl8G3qTcIvVpI1U4QwcicE"

type Config struct {
	Mode         string // "burst" (default), "simulate", "rate"
	Target       string // "userindex" (default), "best-server", "login"
	URL          string
	Batch        int
	Waves        int // [burst] sabit wave sayısı (>0 ise Duration yok sayılır)
	Interval     time.Duration
	Duration     time.Duration
	Timeout      time.Duration
	MaxUserIndex int
	OkulKodu     string // login target için okul kodu
	OgrenciNo    string // login target için öğrenci no
	Phone        string // login target için telefon
	Token        string // questions target için Bearer token
	ErrorsLog    string

	// rate mode (sabit hız / open-loop)
	RPS int

	// simulate mode
	Students     int
	ExamDuration time.Duration
	Questions    int
	ThinkTime    time.Duration
	Rampup       time.Duration
}

type Result struct {
	Wave       int
	StatusCode int
	Latency    time.Duration
	ErrType    string
	ErrMsg     string
	Body       string
	ServerNum  int // best-server target'ında dönen serverNum (0 = yok)
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
	UserIndex int `json:"userIndex"`
}

type responseBody struct {
	Success   bool `json:"success"`
	ServerNum int  `json:"serverNum"`
}

type loginBody struct {
	OkulKodu    string `json:"okulKodu"`
	OgrenciNo   string `json:"ogrenciNo"`
	Phone       string `json:"phone"`
	Sinif       string `json:"sinif"`
	DogumTarihi string `json:"dogumTarihi"`
	Name        string `json:"name"`
	Surname     string `json:"surname"`
}

type loginResponse struct {
	User struct {
		Token string `json:"token"`
	} `json:"user"`
}

// buildRequest, target tipine göre HTTP method + body üretir.
// userIndex == 0 → rastgele 1..MaxUserIndex. > 0 → o değer kullanılır.
func buildRequest(cfg Config, rng *mathrand.Rand, userIndex int) (*http.Request, error) {
	if userIndex == 0 {
		userIndex = rng.Intn(cfg.MaxUserIndex) + 1
	}

	switch cfg.Target {
	case "best-server":
		// Auth gerektirmeyen hafif GET isteği, body yok.
		return http.NewRequest(http.MethodGet, cfg.URL, nil)

	case "questions":
		// Sınav sorularını çekme — GET + Bearer token. URL'de ?code=... olmalı.
		req, err := http.NewRequest(http.MethodGet, cfg.URL, nil)
		if err != nil {
			return nil, err
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		return req, nil

	case "login":
		lb := loginBody{
			OkulKodu:    cfg.OkulKodu,
			OgrenciNo:   cfg.OgrenciNo,
			Phone:       cfg.Phone,
			Sinif:       "9",
			DogumTarihi: "2005-01-01",
			Name:        "AA",
			Surname:     "BB",
		}
		payload, _ := json.Marshal(lb)
		req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil

	default: // "userindex"
		payload, _ := json.Marshal(requestBody{UserIndex: userIndex})
		req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}
}

// validateBody, 2xx gelen cevabın gövdesini target'a göre doğrular.
// Başarılıysa "" döner, aksi halde (errType, errMsg) döner.
func validateBody(cfg Config, bodyBytes []byte) (errType, errMsg string) {
	switch cfg.Target {
	case "login":
		var lr loginResponse
		if err := json.Unmarshal(bodyBytes, &lr); err != nil {
			return "invalid_response", "JSON parse failed"
		}
		if lr.User.Token == "" {
			return "app_error", "token yok"
		}
		return "", ""

	default: // best-server, userindex → {"success": true}
		var parsed responseBody
		if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
			return "invalid_response", "JSON parse failed"
		}
		if !parsed.Success {
			return "app_error", "success=false"
		}
		return "", ""
	}
}

func doRequest(client *http.Client, cfg Config, wave int, rng *mathrand.Rand, userIndex int) Result {
	req, err := buildRequest(cfg, rng, userIndex)
	if err != nil {
		return Result{Wave: wave, ErrType: "unknown", ErrMsg: err.Error()}
	}

	start := time.Now()
	resp, err := client.Do(req)
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
		return r
	}

	// 2xx geldi — target'a göre body doğrula.
	// Limit 1MB: questions gibi büyük response'lar (35KB+) tam okunsun ki
	// JSON parse edilebilsin. Gerçek client de tüm body'yi okur (gerçekçi network yükü).
	limited := io.LimitReader(resp.Body, 1<<20)
	bodyBytes, _ := io.ReadAll(limited)
	if et, em := validateBody(cfg, bodyBytes); et != "" {
		r.ErrType = et
		r.ErrMsg = em
		r.Body = string(bodyBytes)
		return r
	}

	// best-server: dönen serverNum'ı kaydet (dağılım raporu için)
	if cfg.Target == "best-server" {
		var br responseBody
		if json.Unmarshal(bodyBytes, &br) == nil {
			r.ServerNum = br.ServerNum
		}
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
	resultsCh <- doRequest(client, cfg, wave, rng, 0)
}

type Reporter struct {
	allResults     []Result
	statusCount    map[int]int
	errorCount     map[string]int
	serverNumCount map[int]int
	errorsFile     *os.File
	totalErrors    int
}

func newReporter(errorsLogPath string) (*Reporter, error) {
	f, err := os.Create(errorsLogPath)
	if err != nil {
		return nil, fmt.Errorf("errors.log oluşturulamadı: %w", err)
	}
	return &Reporter{
		statusCount:    map[int]int{},
		errorCount:     map[string]int{},
		serverNumCount: map[int]int{},
		errorsFile:     f,
	}, nil
}

func (r *Reporter) record(results []Result) {
	for _, res := range results {
		r.allResults = append(r.allResults, res)
		if res.StatusCode != 0 {
			r.statusCount[res.StatusCode]++
		}
		if res.ServerNum > 0 {
			r.serverNumCount[res.ServerNum]++
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
	if len(r.serverNumCount) > 0 {
		fmt.Println("║                                                          ║")
		fmt.Println("║ Server Distribution (serverNum dağılımı):                ║")
		nums := make([]int, 0, len(r.serverNumCount))
		for n := range r.serverNumCount {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			cnt := r.serverNumCount[n]
			pct := 100.0 * float64(cnt) / float64(len(r.allResults))
			fmt.Printf("║   sinav%-2d             : %-32s║\n", n, fmt.Sprintf("%d (%.1f%%)", cnt, pct))
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
	successfulRPS := float64(success) / totalDuration.Seconds()
	fmt.Printf("║ Effective RPS:        %-35.1f║\n", rps)
	fmt.Printf("║ Successful RPS:       %-35.1f║\n", successfulRPS)
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
	if cfg.Waves > 0 {
		fmt.Printf("  Waves:    %d (sabit)\n", cfg.Waves)
	} else {
		fmt.Printf("  Duration: %s total\n", cfg.Duration)
	}
	fmt.Printf("  Timeout:  %s per request\n", cfg.Timeout)

	start := time.Now()
	wave := 0

	// Döngü koşulu: Waves > 0 ise sabit wave sayısı, değilse süreye bağlı.
	for (cfg.Waves > 0 && wave < cfg.Waves) || (cfg.Waves <= 0 && time.Since(start) < cfg.Duration) {
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

		// Wave arası bekleme. Sabit-wave modunda son wave'den sonra bekleme.
		if cfg.Waves > 0 {
			if wave < cfg.Waves {
				time.Sleep(cfg.Interval)
			}
		} else if time.Since(start)+cfg.Interval < cfg.Duration {
			time.Sleep(cfg.Interval)
		} else {
			break
		}
	}

	totalDuration := time.Since(start)
	reporter.printFinalReport(totalDuration, wave)
	return nil
}

// ─── Simulation mode ────────────────────────────────────────────────────────

type simStats struct {
	totalSent         atomic.Int64
	inFlight          atomic.Int64
	peakInFlight      atomic.Int64
	completedStudents atomic.Int64
}

func studentSession(client *http.Client, cfg Config, examEnd time.Time, stats *simStats, resultsCh chan<- Result, wg *sync.WaitGroup, userIndex int, seed int64) {
	defer wg.Done()
	defer func() {
		if rec := recover(); rec != nil {
			resultsCh <- Result{ErrType: "unknown", ErrMsg: fmt.Sprintf("panic: %v", rec)}
		}
	}()

	rng := mathrand.New(mathrand.NewSource(seed))

	// Rampup: rastgele 0..Rampup arası bekle
	if cfg.Rampup > 0 {
		delay := time.Duration(rng.Int63n(int64(cfg.Rampup)))
		time.Sleep(delay)
	}

	answered := 0
	for answered < cfg.Questions && time.Now().Before(examEnd) {
		// Düşünme süresi: think-time ± %30 jitter
		jitter := 0.7 + rng.Float64()*0.6 // 0.7 .. 1.3
		think := time.Duration(float64(cfg.ThinkTime) * jitter)

		// Eğer think süresinin sonu sınav bitişini aşıyorsa bitir
		if time.Now().Add(think).After(examEnd) {
			return
		}
		time.Sleep(think)

		// Cevap gönder
		inFlight := stats.inFlight.Add(1)
		// Peak güncelle (CAS loop)
		for {
			peak := stats.peakInFlight.Load()
			if inFlight <= peak || stats.peakInFlight.CompareAndSwap(peak, inFlight) {
				break
			}
		}

		res := doRequest(client, cfg, 0, rng, userIndex)
		stats.inFlight.Add(-1)
		stats.totalSent.Add(1)

		resultsCh <- res
		answered++
	}
	stats.completedStudents.Add(1)
}

func runSimulation(cfg Config) error {
	transport := &http.Transport{
		MaxIdleConns:        cfg.Students,
		MaxIdleConnsPerHost: cfg.Students,
		MaxConnsPerHost:     cfg.Students,
		IdleConnTimeout:     5 * time.Minute,
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

	fmt.Printf("Starting EXAM SIMULATION:\n")
	fmt.Printf("  URL:           %s\n", cfg.URL)
	fmt.Printf("  Students:      %d\n", cfg.Students)
	fmt.Printf("  Exam duration: %s\n", cfg.ExamDuration)
	fmt.Printf("  Questions/stu: %d\n", cfg.Questions)
	fmt.Printf("  Think time:    %s (±30%% jitter)\n", cfg.ThinkTime)
	fmt.Printf("  Rampup:        %s\n", cfg.Rampup)
	fmt.Printf("  Timeout:       %s per request\n", cfg.Timeout)
	fmt.Printf("  Expected avg:  ~%.0f RPS\n\n",
		float64(cfg.Students)/cfg.ThinkTime.Seconds())

	stats := &simStats{}
	var wg sync.WaitGroup
	resultsCh := make(chan Result, cfg.Students)

	// Sonuç tüketici goroutine
	allResults := make([]Result, 0, cfg.Students*cfg.Questions)
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for res := range resultsCh {
			allResults = append(allResults, res)
		}
	}()

	// Per-second RPS sampler
	rpsSamples := make([]int64, 0, int(cfg.ExamDuration.Seconds())+10)
	stopSampler := make(chan struct{})
	var samplerWg sync.WaitGroup
	samplerWg.Add(1)
	go func() {
		defer samplerWg.Done()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		var lastTotal int64
		var lastPrint time.Time = time.Now()
		for {
			select {
			case <-ticker.C:
				now := stats.totalSent.Load()
				delta := now - lastTotal
				rpsSamples = append(rpsSamples, delta)
				lastTotal = now

				// Her 10 saniyede bir canlı durum yazdır
				if time.Since(lastPrint) >= 10*time.Second {
					lastPrint = time.Now()
					fmt.Printf("[%s] sent=%d  in-flight=%d  this-sec=%d  peak-in-flight=%d  completed=%d\n",
						time.Now().Format("15:04:05"),
						now, stats.inFlight.Load(), delta,
						stats.peakInFlight.Load(), stats.completedStudents.Load())
				}
			case <-stopSampler:
				return
			}
		}
	}()

	start := time.Now()
	examEnd := start.Add(cfg.ExamDuration)

	// Öğrencileri başlat — her öğrenci sabit userIndex=i+1 alır (1..Students)
	for i := 0; i < cfg.Students; i++ {
		wg.Add(1)
		go studentSession(client, cfg, examEnd, stats, resultsCh, &wg, i+1, time.Now().UnixNano()^int64(i))
	}

	wg.Wait()
	close(resultsCh)
	consumerWg.Wait()
	close(stopSampler)
	samplerWg.Wait()

	totalDuration := time.Since(start)

	reporter.record(allResults)
	reporter.printSimulationReport(cfg, totalDuration, allResults, rpsSamples, stats)
	return nil
}

func (r *Reporter) printSimulationReport(cfg Config, totalDuration time.Duration, all []Result, rpsSamples []int64, stats *simStats) {
	total := len(all)
	var success, failed int
	latencies := make([]time.Duration, 0, total)
	for _, res := range all {
		latencies = append(latencies, res.Latency)
		if res.ErrType == "" {
			success++
		} else {
			failed++
		}
	}
	m := computeMetrics(latencies)

	successPct, failedPct := 0.0, 0.0
	if total > 0 {
		successPct = 100.0 * float64(success) / float64(total)
		failedPct = 100.0 * float64(failed) / float64(total)
	}

	// RPS dağılımı (saniye başı)
	rpsAvg, rpsP50, rpsP95, rpsPeak := int64(0), int64(0), int64(0), int64(0)
	if len(rpsSamples) > 0 {
		sorted := make([]int64, len(rpsSamples))
		copy(sorted, rpsSamples)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		var sum int64
		for _, v := range sorted {
			sum += v
		}
		rpsAvg = sum / int64(len(sorted))
		rpsP50 = sorted[len(sorted)/2]
		idx95 := int(float64(len(sorted))*0.95 + 0.999999)
		if idx95 > len(sorted) {
			idx95 = len(sorted)
		}
		if idx95 < 1 {
			idx95 = 1
		}
		rpsP95 = sorted[idx95-1]
		rpsPeak = sorted[len(sorted)-1]
	}

	completedStu := stats.completedStudents.Load()
	completedPct := 0.0
	if cfg.Students > 0 {
		completedPct = 100.0 * float64(completedStu) / float64(cfg.Students)
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║              EXAM SIMULATION REPORT                      ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║ Duration:               %-33s║\n", totalDuration.Round(time.Second))
	fmt.Printf("║ Students simulated:     %-33d║\n", cfg.Students)
	fmt.Printf("║ Students completed:     %-33s║\n",
		fmt.Sprintf("%d (%.1f%%)", completedStu, completedPct))
	fmt.Printf("║ Total answers sent:     %-33d║\n", total)
	fmt.Printf("║ Successful:             %-33s║\n",
		fmt.Sprintf("%d (%.1f%%)", success, successPct))
	fmt.Printf("║ Failed:                 %-33s║\n",
		fmt.Sprintf("%d (%.1f%%)", failed, failedPct))
	fmt.Println("║                                                          ║")
	fmt.Println("║ RPS per second (across test):                            ║")
	fmt.Printf("║   avg     = %-45s║\n", fmt.Sprintf("%d req/s", rpsAvg))
	fmt.Printf("║   p50     = %-45s║\n", fmt.Sprintf("%d req/s", rpsP50))
	fmt.Printf("║   p95     = %-45s║\n", fmt.Sprintf("%d req/s", rpsP95))
	fmt.Printf("║   peak    = %-45s║\n", fmt.Sprintf("%d req/s (en yoğun saniye)", rpsPeak))
	fmt.Println("║                                                          ║")
	fmt.Printf("║ Peak concurrent in-flight: %-30d║\n", stats.peakInFlight.Load())
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
	if r.totalErrors > 0 {
		fmt.Println("║                                                          ║")
		fmt.Printf("║ Errors logged to:     %-35s║\n", fmt.Sprintf("errors.log (%d entries)", r.totalErrors))
	}
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
}

// ─── Rate mode (sabit hız / open-loop) ──────────────────────────────────────
// Her saniye tam RPS kadar istek fırlatır (önceki bitmese bile). Server'ın
// "saniyede şu kadar kaldırıyor" rakamını net ölçmek için.

func runRate(cfg Config) error {
	transport := &http.Transport{
		MaxIdleConns:        cfg.RPS * 4,
		MaxIdleConnsPerHost: cfg.RPS * 4,
		MaxConnsPerHost:     cfg.RPS * 4,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Timeout: cfg.Timeout, Transport: transport}

	reporter, err := newReporter(cfg.ErrorsLog)
	if err != nil {
		return err
	}
	defer reporter.close()

	totalSeconds := int(cfg.Duration.Seconds())
	if totalSeconds < 1 {
		totalSeconds = 1
	}

	fmt.Printf("Starting RATE test (sabit hız / open-loop):\n")
	fmt.Printf("  URL:       %s\n", cfg.URL)
	fmt.Printf("  Target:    %d req/s sabit\n", cfg.RPS)
	fmt.Printf("  Duration:  %ds (toplam ~%d istek)\n", totalSeconds, cfg.RPS*totalSeconds)
	fmt.Printf("  Timeout:   %s\n\n", cfg.Timeout)

	stats := &simStats{}
	var wg sync.WaitGroup
	resultsCh := make(chan Result, cfg.RPS*4)

	allResults := make([]Result, 0, cfg.RPS*totalSeconds)
	var consumerWg sync.WaitGroup
	consumerWg.Add(1)
	go func() {
		defer consumerWg.Done()
		for res := range resultsCh {
			allResults = append(allResults, res)
		}
	}()

	start := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for sec := 0; sec < totalSeconds; sec++ {
		// Bu saniye için RPS kadar istek fırlat (beklemeden)
		for i := 0; i < cfg.RPS; i++ {
			wg.Add(1)
			seed := time.Now().UnixNano() ^ int64(sec*cfg.RPS+i)
			go func(seed int64) {
				defer wg.Done()
				defer func() {
					if rec := recover(); rec != nil {
						resultsCh <- Result{ErrType: "unknown", ErrMsg: fmt.Sprintf("panic: %v", rec)}
					}
				}()
				rng := mathrand.New(mathrand.NewSource(seed))
				inFlight := stats.inFlight.Add(1)
				for {
					peak := stats.peakInFlight.Load()
					if inFlight <= peak || stats.peakInFlight.CompareAndSwap(peak, inFlight) {
						break
					}
				}
				res := doRequest(client, cfg, 0, rng, 0)
				stats.inFlight.Add(-1)
				stats.totalSent.Add(1)
				resultsCh <- res
			}(seed)
		}

		// Canlı durum: bu saniye sonunda kaç in-flight birikmiş
		fmt.Printf("[%2ds/%ds] gönderildi=%d  in-flight=%d  (in-flight birikiyorsa server yetişemiyor)\n",
			sec+1, totalSeconds, stats.totalSent.Load(), stats.inFlight.Load())

		<-ticker.C // bir sonraki saniyeyi bekle
	}

	wg.Wait()
	close(resultsCh)
	consumerWg.Wait()

	totalDuration := time.Since(start)
	reporter.record(allResults)
	reporter.printRateReport(cfg, totalDuration, allResults, stats)
	return nil
}

func (r *Reporter) printRateReport(cfg Config, totalDuration time.Duration, all []Result, stats *simStats) {
	total := len(all)
	var success, failed int
	latencies := make([]time.Duration, 0, total)
	for _, res := range all {
		latencies = append(latencies, res.Latency)
		if res.ErrType == "" {
			success++
		} else {
			failed++
		}
	}
	m := computeMetrics(latencies)

	secs := totalDuration.Seconds()
	successfulRPS := float64(success) / secs
	failedPct := 0.0
	if total > 0 {
		failedPct = 100.0 * float64(failed) / float64(total)
	}

	// Verdict: server hedef hızı kaldırdı mı?
	var verdict string
	switch {
	case successfulRPS >= float64(cfg.RPS)*0.98 && m.P95 < cfg.Timeout/2:
		verdict = "✅ KALDIRIYOR — server bu hızı rahat işliyor"
	case successfulRPS >= float64(cfg.RPS)*0.95:
		verdict = "⚠️  SINIRDA — kaldırıyor ama latency yükseliyor, tavana yakın"
	default:
		verdict = "❌ KALDIRAMIYOR — bu hız server kapasitesinin üstünde"
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║                   RATE TEST REPORT                       ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║ Target RPS:        %-38d║\n", cfg.RPS)
	fmt.Printf("║ Duration:          %-38s║\n", totalDuration.Round(time.Second))
	fmt.Printf("║ Total Sent:        %-38d║\n", total)
	fmt.Printf("║ Successful:        %-38s║\n", fmt.Sprintf("%d", success))
	fmt.Printf("║ Failed:            %-38s║\n", fmt.Sprintf("%d (%.1f%%)", failed, failedPct))
	fmt.Println("║                                                          ║")
	fmt.Printf("║ >>> Başarılı RPS:  %-38s║\n", fmt.Sprintf("%.0f req/s", successfulRPS))
	fmt.Println("║                                                          ║")
	fmt.Println("║ Latency:                                                 ║")
	fmt.Printf("║   p50     = %-45s║\n", m.P50.Round(time.Millisecond))
	fmt.Printf("║   p95     = %-45s║\n", m.P95.Round(time.Millisecond))
	fmt.Printf("║   p99     = %-45s║\n", m.P99.Round(time.Millisecond))
	fmt.Printf("║   max     = %-45s║\n", m.Max.Round(time.Millisecond))
	fmt.Printf("║ Peak in-flight:    %-38d║\n", stats.peakInFlight.Load())
	if len(r.errorCount) > 0 {
		fmt.Println("║                                                          ║")
		fmt.Println("║ Error Distribution:                                      ║")
		for k, v := range r.errorCount {
			fmt.Printf("║   %-20s : %-32d║\n", k, v)
		}
	}
	fmt.Println("║                                                          ║")
	fmt.Println("║ SONUÇ:                                                   ║")
	fmt.Printf("║   %-54s║\n", verdict)
	if r.totalErrors > 0 {
		fmt.Printf("║   %-54s║\n", fmt.Sprintf("errors.log: %d kayıt", r.totalErrors))
	}
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Mode, "mode", "burst", "Test modu: burst (dalga) | simulate (sınav) | rate (sabit hız)")
	flag.StringVar(&cfg.Target, "target", "best-server", "Hedef tipi: best-server | login | userindex")
	flag.StringVar(&cfg.URL, "url", "https://giris.sinavkutusu.com/api/best-server", "Hedef endpoint URL")
	flag.IntVar(&cfg.Batch, "batch", 500, "[burst] Her dalgadaki eşzamanlı istek sayısı")
	flag.IntVar(&cfg.Waves, "waves", 0, "[burst] Sabit wave sayısı (>0 ise -duration yok sayılır)")
	flag.DurationVar(&cfg.Interval, "interval", 5*time.Second, "[burst] Dalgalar arası bekleme")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "[burst] Toplam test süresi")
	flag.DurationVar(&cfg.Timeout, "timeout", 10*time.Second, "Her isteğin HTTP timeout süresi")
	flag.IntVar(&cfg.MaxUserIndex, "max-user-index", 15000, "userIndex/ogrenciNo 1..N rastgele üretilir")
	flag.StringVar(&cfg.OkulKodu, "okul-kodu", "311", "[login] Login için okul kodu")
	flag.StringVar(&cfg.OgrenciNo, "ogrenci-no", "123", "[login] Login için öğrenci no (kayıtlı olmalı)")
	flag.StringVar(&cfg.Phone, "phone", "123", "[login] Login için telefon")
	flag.StringVar(&cfg.Token, "token", defaultToken, "[questions] Bearer token (Authorization header)")
	flag.StringVar(&cfg.ErrorsLog, "errors-log", "errors.log", "Hata log dosyası yolu")

	// simulate mode parametreleri
	flag.IntVar(&cfg.Students, "students", 15000, "[simulate] Sınava giren öğrenci sayısı")
	flag.DurationVar(&cfg.ExamDuration, "exam-duration", 60*time.Minute, "[simulate] Sınav süresi")
	flag.IntVar(&cfg.Questions, "questions", 25, "[simulate] Her öğrencinin cevaplayacağı soru sayısı")
	flag.DurationVar(&cfg.ThinkTime, "think-time", 30*time.Second, "[simulate] Cevaplar arası ortalama düşünme süresi (±30%% jitter)")
	flag.DurationVar(&cfg.Rampup, "rampup", 2*time.Minute, "[simulate] Öğrencilerin sınava giriş süresi (kademeli)")

	// rate mode parametresi
	flag.IntVar(&cfg.RPS, "rps", 300, "[rate] Saniyede gönderilecek sabit istek sayısı")

	flag.Parse()

	if cfg.MaxUserIndex < 1 {
		log.Fatal("max-user-index en az 1 olmalı")
	}
	switch cfg.Target {
	case "best-server", "login", "questions", "userindex":
		// geçerli
	default:
		log.Fatalf("bilinmeyen target: %q (best-server | login | questions | userindex)", cfg.Target)
	}

	switch cfg.Mode {
	case "burst":
		if cfg.Batch < 1 {
			log.Fatal("batch en az 1 olmalı")
		}
		if err := runTest(cfg); err != nil {
			log.Fatal(err)
		}
	case "simulate":
		if cfg.Students < 1 {
			log.Fatal("students en az 1 olmalı")
		}
		if cfg.Questions < 1 {
			log.Fatal("questions en az 1 olmalı")
		}
		if cfg.ThinkTime <= 0 {
			log.Fatal("think-time pozitif olmalı")
		}
		if err := runSimulation(cfg); err != nil {
			log.Fatal(err)
		}
	case "rate":
		if cfg.RPS < 1 {
			log.Fatal("rps en az 1 olmalı")
		}
		if err := runRate(cfg); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("bilinmeyen mode: %q (burst | simulate | rate)", cfg.Mode)
	}
}
