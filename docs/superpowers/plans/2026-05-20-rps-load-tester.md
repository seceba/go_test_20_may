# RPS Load Tester Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Go ile, dalga halinde HTTP POST isteği üreten ve latency/hata raporu basan CLI yük testi aracı oluştur.

**Architecture:** Tek `main.go` dosyası. Runner dalga döngüsünü yönetir; her dalgada `batch` adet goroutine spawn eder, `sync.WaitGroup` ile bekler, sonuçları channel'dan toplar, Reporter dalga özetini ve final raporu basar. Hatalar düz metin `errors.log` dosyasına yazılır.

**Tech Stack:** Go 1.21+, stdlib only (`net/http`, `encoding/json`, `crypto/rand`, `sync`, `flag`, `time`, `sort`).

---

## Task 1: Proje iskeletini kur

**Files:**
- Create: `go.mod`
- Create: `.gitignore`

- [ ] **Step 1: Go module başlat**

Run: `cd /Users/bicirik/Desktop/gotest && go mod init rpstester`
Expected: `go.mod` dosyası oluşur, `module rpstester` satırı içerir.

- [ ] **Step 2: .gitignore yaz**

```
# Build artifacts
rpstester
*.exe

# Runtime files
errors.log
*.log

# OS
.DS_Store
```

- [ ] **Step 3: Doğrula**

Run: `ls -la /Users/bicirik/Desktop/gotest/`
Expected: `go.mod` ve `.gitignore` görünür.

---

## Task 2: Result, Config struct'ları ve hata sınıflandırma

**Files:**
- Create: `main.go` (sadece type'lar ve helper'lar)

- [ ] **Step 1: main.go'ya temel type'ları yaz**

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
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
	ErrType    string // "" = success
	ErrMsg     string
	Body       string
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
```

- [ ] **Step 2: Build edilebilir mi kontrol et**

Run: `cd /Users/bicirik/Desktop/gotest && go build ./...`
Expected: Hata yok, `rpstester` binary'si oluşur (main fonksiyonu olmadığı için aslında build başarısız olur — bunu Task 7'de ekleyeceğiz). Şimdilik `go vet ./...` ile syntax kontrolü:

Run: `cd /Users/bicirik/Desktop/gotest && go vet ./...`
Expected: "function main is undeclared in the main package" hatası — bu beklenen. Syntax'tan başka hata olmamalı.

---

## Task 3: Metrics hesaplama (TDD ile)

Bu saf fonksiyon — birim test yazmaya değer. Diğer concurrency/HTTP kodu için TDD overhead'i fazla, ama metric matematik hata yaparsa rapor yanıltıcı olur.

**Files:**
- Create: `main_test.go`
- Modify: `main.go` (Metrics fonksiyonunu ekle)

- [ ] **Step 1: main_test.go'da failing test yaz**

```go
package main

import (
	"testing"
	"time"
)

func TestPercentiles(t *testing.T) {
	latencies := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		400 * time.Millisecond,
		500 * time.Millisecond,
		600 * time.Millisecond,
		700 * time.Millisecond,
		800 * time.Millisecond,
		900 * time.Millisecond,
		1000 * time.Millisecond,
	}
	m := computeMetrics(latencies)
	if m.Min != 100*time.Millisecond {
		t.Errorf("Min: got %v, want 100ms", m.Min)
	}
	if m.Max != 1000*time.Millisecond {
		t.Errorf("Max: got %v, want 1000ms", m.Max)
	}
	if m.Avg != 550*time.Millisecond {
		t.Errorf("Avg: got %v, want 550ms", m.Avg)
	}
	// nearest-rank: p50 of n=10 → index ceil(0.5*10)=5 → latencies[4] = 500ms
	if m.P50 != 500*time.Millisecond {
		t.Errorf("P50: got %v, want 500ms", m.P50)
	}
	// p95 → ceil(0.95*10)=10 → latencies[9] = 1000ms
	if m.P95 != 1000*time.Millisecond {
		t.Errorf("P95: got %v, want 1000ms", m.P95)
	}
	// p99 → ceil(0.99*10)=10 → latencies[9] = 1000ms
	if m.P99 != 1000*time.Millisecond {
		t.Errorf("P99: got %v, want 1000ms", m.P99)
	}
}

func TestPercentilesEmpty(t *testing.T) {
	m := computeMetrics(nil)
	if m.Min != 0 || m.Max != 0 || m.Avg != 0 {
		t.Errorf("empty input should yield zero metrics, got %+v", m)
	}
}
```

- [ ] **Step 2: Test'i çalıştır, fail görmeli**

Run: `cd /Users/bicirik/Desktop/gotest && go test ./...`
Expected: FAIL — "undefined: computeMetrics" veya "undefined: Metrics".

- [ ] **Step 3: main.go'ya Metrics ve computeMetrics ekle**

```go
// main.go'nun en altına ekle, yeni import: "sort"

type Metrics struct {
	Min, Max, Avg, P50, P95, P99 time.Duration
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
		// nearest-rank method
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
```

`import "sort"` satırını dosyanın en üstündeki import bloğuna ekle.

- [ ] **Step 4: Testleri çalıştır, geçmeli**

Run: `cd /Users/bicirik/Desktop/gotest && go test ./... -v`
Expected: `TestPercentiles` ve `TestPercentilesEmpty` PASS.

---

## Task 4: Worker fonksiyonu

**Files:**
- Modify: `main.go`

- [ ] **Step 1: doRequest ve worker fonksiyonlarını ekle**

`import` bloğuna ekle: `"bytes"`, `"encoding/json"`, `"fmt"`, `"io"`, `"math/rand"`, `"net/http"`, `"sync"`.

```go
// main.go'nun en altına ekle

type requestBody struct {
	QuestionID int    `json:"questionId"`
	Answer     int    `json:"answer"`
	StudentID  string `json:"studentId"`
}

func doRequest(client *http.Client, cfg Config, wave int, rng *rand.Rand) Result {
	body := requestBody{
		QuestionID: rng.Intn(cfg.MaxQuestionID) + 1, // 1..MaxQuestionID
		Answer:     rng.Intn(cfg.AnswerChoices),     // 0..AnswerChoices-1
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
		// hata durumunda body'nin ilk 500 byte'ını oku
		limited := io.LimitReader(resp.Body, 500)
		b, _ := io.ReadAll(limited)
		r.Body = string(b)
		r.ErrMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else {
		// success → body'yi tüket (connection reuse için)
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
	rng := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(wave)))
	resultsCh <- doRequest(client, cfg, wave, rng)
}
```

- [ ] **Step 2: Compile kontrolü**

Run: `cd /Users/bicirik/Desktop/gotest && go vet ./...`
Expected: Sadece "function main is undeclared" hatası kalmalı, başka error yok.

---

## Task 5: Runner ve Reporter

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Reporter ve Runner ekle**

`import` bloğuna: `"os"`, `"strings"` ekle.

```go
// main.go'nun en altına ekle

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
```

- [ ] **Step 2: Compile kontrolü**

Run: `cd /Users/bicirik/Desktop/gotest && go vet ./...`
Expected: Sadece "function main is undeclared" hatası kalır.

---

## Task 6: main() ve CLI flag'ler

**Files:**
- Modify: `main.go`

- [ ] **Step 1: import'a "flag", "log" ekle ve main fonksiyonunu yaz**

```go
// main.go'nun en altına ekle

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
```

- [ ] **Step 2: Build et**

Run: `cd /Users/bicirik/Desktop/gotest && go build -o rpstester .`
Expected: Hata yok, `rpstester` binary'si oluşur.

- [ ] **Step 3: Help mesajını gör**

Run: `cd /Users/bicirik/Desktop/gotest && ./rpstester -h`
Expected: Tüm flag'ler listelenir.

---

## Task 7: Smoke test (kullanıcının kendi endpoint'ine karşı)

- [ ] **Step 1: Küçük yük ile smoke test**

Run: `cd /Users/bicirik/Desktop/gotest && ./rpstester -batch=10 -interval=2s -duration=6s`
Expected:
- 2-3 dalga görünür
- Her dalga özeti yazılır
- Final rapor basılır
- Eğer hata varsa `errors.log` dosyası oluşur

- [ ] **Step 2: errors.log içeriğini kontrol et**

Run: `cd /Users/bicirik/Desktop/gotest && head -5 errors.log 2>/dev/null || echo "no errors logged"`
Expected: Ya hata satırları görünür ya da "no errors logged" çıktısı.

- [ ] **Step 3: Test'leri tekrar çalıştır**

Run: `cd /Users/bicirik/Desktop/gotest && go test ./...`
Expected: PASS.

---

## Notlar

- **Concurrency limit:** İlk versiyon `batch` parametresine kadar eş zamanlı goroutine açar. macOS varsayılan ulimit (256 file descriptor) bunu sınırlayabilir — kullanıcı 2500+ batch deneyecekse `ulimit -n 65535` çalıştırmalı.
- **Kendi makinenden test:** Yerel ağ darboğazı, NAT, DNS resolver kuyruğu gibi etmenler servisten önce darboğaz olabilir. Sonuçlar yorumlanırken bunlar akılda tutulmalı.
- **Sunucuya saygı:** Bu araç DDoS amaçlı değildir — kendi servisini test etmek için. Yetkisiz hedeflere kullanma.
