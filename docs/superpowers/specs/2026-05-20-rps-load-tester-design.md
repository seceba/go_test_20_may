# RPS Load Tester — Tasarım Dokümanı

**Tarih:** 2026-05-20
**Hedef:** Sınav sistemi (`canavar.online`) için Go ile yazılmış, dalga halinde yük üreten ve raporlayan bir RPS test aracı.

## 1. Amaç

15000 öğrencinin eş zamanlı sınava girdiği senaryoyu test etmek. Hedef endpoint hem MongoDB SELECT (soru çekme) hem INSERT (cevap kaydetme) yaptığından, gerçek prod davranışını yansıtır.

**Ölçmek istediklerimiz:**
- Eş zamanlı bağlantı altında latency dağılımı (avg, p50, p95, p99, max)
- Başarı oranı / hata tipleri (timeout, 5xx, connection refused)
- Servisin çökme noktası — kademeli arttırarak (500 → 1000 → 2500 → ...)

## 2. Hedef Endpoint

```
POST https://api.canavar.online/api/tester/answer
Content-Type: application/json

Body:
{
  "questionId": 7,            // 1..25 rastgele
  "answer": 2,                // 0..3 rastgele (4 şık)
  "studentId": "stu_a1b2c3d4" // her istek için benzersiz
}
```

Cevap doğrulama: 2xx başarılı, 4xx/5xx/timeout/connection-error başarısız sayılır. Response body sadece hata durumunda errors.log'a yazılır.

## 3. Yaklaşım

**Dalga-başına-spawn modeli.** Her dalgada `batch` adet goroutine `go func()` ile başlatılır, hepsi paralel istek atar, `sync.WaitGroup` ile beklenir, dalga özeti basılır, `interval` kadar uyunur, sonraki dalga başlar. Toplam test süresi `duration` ile sınırlandırılır.

Alternatif olarak değerlendirilen ve reddedilen yaklaşım: kalıcı worker pool (channel ile iş dağıtımı). Reddedilme sebebi: Go'da goroutine spawn maliyeti bu ölçekte ihmal edilebilir, ve "her öğrenci kendi cihazından bağlanır" senaryosunu dalga-başına-spawn daha iyi yansıtır. Kod da daha okunaklı.

HTTP client tek instance, tüm goroutineler paylaşır. `http.Transport` zaten bağlantı havuzu yönetir; `MaxIdleConnsPerHost = batch` ayarıyla keep-alive bağlantıları yeniden kullanılır.

## 4. CLI Parametreleri

```
go run main.go \
  -url=https://api.canavar.online/api/tester/answer \
  -batch=500 \             # her dalgada eşzamanlı istek sayısı
  -interval=5s \           # dalgalar arası bekleme (önceki dalga BİTTİKTEN sonra)
  -duration=30s \          # toplam test süresi
  -timeout=10s \           # her isteğin HTTP timeout süresi
  -max-question-id=25 \    # questionId 1..N rastgele üretilir
  -answer-choices=4 \      # answer 0..N-1 rastgele üretilir
  -errors-log=errors.log   # başarısız istek detayları için dosya yolu
```

**Davranış kuralları:**
- `interval` önceki dalga bittikten sonra ölçülür (sabit periyot değil).
- `time.Since(start) >= duration` olunca yeni dalga başlatılmaz; o ana kadar başlamış dalga tamamlanır.
- `studentId` üretimi: `stu_` + 8 karakter rastgele alfanumerik (`crypto/rand` ile). Çakışma pratikte sıfır.
- Tüm parametrelerin makul default değerleri vardır; sadece `-url` zorunlu sayılabilir veya hardcoded default olabilir.

## 5. Bileşenler

Tek dosyada (`main.go`), mantıksal birimler:

| Birim | Sorumluluk |
|---|---|
| **Config** | CLI flag parse + validation |
| **Runner** | Dalga döngüsü orkestrasyonu |
| **Worker** | Tek HTTP isteği: payload üret → POST → latency ölç → Result döndür |
| **Reporter** | Result'ları toplar, dalga özeti + final rapor üretir, errors.log yazar |
| **Metrics** | Latency listesinden p50/p95/p99/min/max/avg hesaplar |

**Result struct'ı:**
```go
type Result struct {
    Wave       int
    StatusCode int           // 0 = HTTP'ye ulaşmadan hata (timeout, conn refused)
    Latency    time.Duration
    ErrType    string        // "" = başarılı, aksi halde "timeout", "connection_refused", "server_error", "client_error", "unknown"
    ErrMsg     string        // hata mesajı (errors.log için)
    Body       string        // sadece hata durumunda doldurulur (debug için, ilk 500 byte)
}
```

## 6. Akış

**Runner ana döngüsü (pseudo-code):**
```
start = time.Now()
waveNum = 0
for time.Since(start) < duration:
    waveNum++
    var wg sync.WaitGroup
    resultsCh := make(chan Result, batch)

    waveStart = time.Now()
    for i := 0; i < batch; i++:
        wg.Add(1)
        go worker(client, config, waveNum, resultsCh, &wg)

    wg.Wait()
    close(resultsCh)

    waveResults := drainChannel(resultsCh)
    waveDuration = time.Since(waveStart)
    reporter.PrintWaveSummary(waveNum, waveResults, waveDuration)
    reporter.AccumulateGlobal(waveResults)

    if time.Since(start) + interval < duration:
        time.Sleep(interval)

reporter.PrintFinalReport()
reporter.FlushErrorLog()
```

**Worker adımları:**
1. Rastgele `questionId`, `answer`, `studentId` üret.
2. JSON body'yi marshal et.
3. `t0 = time.Now()` kaydet.
4. `client.Post(url, "application/json", body)` çağır.
5. `latency = time.Since(t0)`.
6. Hata sınıflandır:
   - `net.Error` ve `Timeout()` → `"timeout"`
   - `errors.Is(err, syscall.ECONNREFUSED)` → `"connection_refused"`
   - HTTP 5xx → `"server_error"`
   - HTTP 4xx → `"client_error"`
   - HTTP 2xx → `""` (başarı)
   - Diğer → `"unknown"`
7. Hata durumunda response body'nin ilk 500 byte'ını oku, Result'a koy.
8. Result'ı channel'a gönder, `wg.Done()` çağır.

**Latency tanımı:** Request gönderim öncesi - response body okuma sonrası. DNS + TCP + TLS + HTTP roundtrip + body okuma dahil. Gerçek kullanıcı deneyimine en yakın ölçüm.

## 7. Çıktı Formatı

**Dalga sonu özeti:**
```
═══ Wave 3/6 ═══ (5.2s)
  Sent:        500
  Success:     487 (97.4%)
  Failed:       13 (2.6%)
  Latency:     min=89ms  avg=412ms  p50=380ms  p95=890ms  p99=1240ms  max=1890ms
  Errors:      timeout=8  server_error=5
  Throughput:  96.1 req/s
```

**Final rapor:**
```
╔══════════════════════════════════════════════════════════╗
║                   FINAL REPORT                           ║
╠══════════════════════════════════════════════════════════╣
║ Duration:      30.4s                                     ║
║ Waves:         6                                         ║
║ Total Sent:    3000                                      ║
║ Total Success: 2891 (96.4%)                              ║
║ Total Failed:  109 (3.6%)                                ║
║                                                          ║
║ Latency (overall):                                       ║
║   min     = 78ms                                         ║
║   avg     = 445ms                                        ║
║   p50     = 390ms                                        ║
║   p95     = 920ms                                        ║
║   p99     = 1350ms                                       ║
║   max     = 2100ms                                       ║
║                                                          ║
║ HTTP Status Distribution:                                ║
║   200 OK              : 2891                             ║
║   500 Internal Error  : 67                               ║
║   504 Gateway Timeout : 22                               ║
║                                                          ║
║ Error Distribution:                                      ║
║   timeout             : 14                               ║
║   server_error        : 89                               ║
║   connection_refused  : 6                                ║
║                                                          ║
║ Effective RPS:        98.7 req/s                         ║
║ Errors logged to:     errors.log (109 entries)           ║
╚══════════════════════════════════════════════════════════╝
```

**errors.log formatı (düz metin, insan okuyabilir):**
```
2026-05-20T14:55:32Z wave=3 type=timeout latency=10001ms status=0 err="net/http: timeout"
2026-05-20T14:55:33Z wave=3 type=server_error latency=340ms status=500 body="mongo: connection pool exhausted"
```

JSON/CSV formatı tercih edilmedi — kullanıcı CLI tabanlı sade çıktı istedi. grep/less ile yeterli.

## 8. Concurrency ve Hata Senaryoları

- **Channel kapasitesi:** `make(chan Result, batch)` — buffer eşit batch'e. Reporter geride kalsa bile worker'lar bloklanmaz.
- **WaitGroup:** Her worker `defer wg.Done()` ile garantili düşer; panic durumunda bile.
- **Worker panic koruması:** Worker fonksiyonunun en üstünde `defer recover()` — bir worker panikleyse test devam etsin, sadece o istek "unknown" hatası olarak kaydedilsin.
- **HTTP client timeout:** `client.Timeout = config.Timeout` — hung connection'lar kilitlenmez.
- **errors.log yazımı:** Test boyunca buffer'da tutulur (`[]ErrorEntry`), test bitince tek seferde diske yazılır. Disk I/O test ölçümünü etkilemez. (Eğer test çok uzun sürerse — örn. 10dk+ — periyodik flush eklenebilir; ilk versiyonda YAGNI.)

## 9. Kapsam Dışı (YAGNI)

İlk versiyona girmeyecekler:
- Warmup / rampup fazı (sabit batch ile yeterli)
- Canlı ilerleme (per-saniye terminal güncellemesi) — dalga özeti yeterli
- CSV/JSON output (final rapor terminal + errors.log JSONL yeterli)
- Distributed mode (tek makineden test)
- Authentication (endpoint açık)
- Custom header / cookie desteği
- Histogram / grafik çıktısı
- Retry mekanizması (yük testinde retry istenmez)

Bu öğeler ihtiyaç doğarsa sonraki iterasyonlarda eklenir.

## 10. Test Stratejisi

Yük testi aracının kendisi de test edilmeli:
- **Metrics birim testi:** Bilinen latency listesi → bilinen p50/p95/p99 sonuçları.
- **Worker birim testi:** Mock HTTP server (httptest.Server) → bilinen status code'lar → doğru ErrType sınıflandırması.
- **Runner entegrasyon testi:** Mock server'a karşı küçük batch + kısa duration → wave sayısı ve total request sayısı beklenen aralıkta.

Implementation planı bu testlerin TDD sırasını netleştirecek.

## 11. Dosya Yapısı

```
gotest/
├── docs/superpowers/specs/2026-05-20-rps-load-tester-design.md
├── go.mod
├── main.go              # tüm bileşenler tek dosyada (ilk versiyon)
├── main_test.go         # birim + entegrasyon testleri
└── errors.log           # runtime'da oluşur, .gitignore'da
```

Tek dosya tercih edildi çünkü toplam kod ~300-400 satır arası. Büyürse `internal/` paketlerine bölünür.
