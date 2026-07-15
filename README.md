# Audiobook TTS Reader

Консольний інструмент на Go для озвучення текстових книг у Windows Desktop із автоматичним збереженням прогресу.

## Платформа

Застосунок використовує Windows SAPI через PowerShell і призначений для Windows 10/11. Контейнерний запуск у Docker або Kubernetes/minikube не є коректною runtime-моделлю для цього проєкту, бо Linux-контейнер не має доступу до Windows SAPI та desktop audio session.

## Можливості

- Озвучення тексту встановленими голосами Windows SAPI.
- Старт із збереженої byte-позиції або з указаної фрази.
- Потокове читання книги через `StreamingChunkReader` без попереднього розбиття всього файлу в пам'яті.
- Розбиття тексту на фрагменти з урахуванням UTF-8, byte-offset прогресу і меж речень.
- Збереження прогресу після кожного фрагмента, при TTS-помилці та при `Ctrl+C`.
- Валідація файлу прогресу, `-chunk`, `-tts-timeout` і UTF-8 меж перед відновленням читання.
- Timeout для кожного TTS-фрагмента, щоб несправний Windows SAPI/audio stack не блокував CLI назавжди.
- Регресійні тести для failure scenarios без запуску реального TTS.
- Benchmarks і profiling workflow для порівняння повного pre-split підходу зі streaming chunk generation.
- Локальний REST API режим із JSON endpoints, SSE-подіями, browser dashboard і OpenAPI contract.

## Архітектура читання

```text
Book file
  ↓
StreamingChunkReader
  ↓
Chunk{Text, StartByte, EndByte}
  ↓
Windows SAPI TTS
  ↓
Save EndByte progress
  ↓
Next chunk
```

## Вимоги

- Windows 10/11.
- Go 1.25.12 або сумісний patched toolchain.
- PowerShell із доступом до `System.Speech`.

## Releases

GitHub Release створюється автоматично після push tag у форматі `v*`:

```powershell
git tag v0.1.0
git push origin v0.1.0
```

Workflow `.github/workflows/release.yml` збирає Windows amd64 binary і додає до Release два файли:

```text
tts-reader-windows-amd64.exe
tts-reader-windows-amd64.exe.sha256
```

Перевірка checksum після завантаження:

```powershell
Get-FileHash .\tts-reader-windows-amd64.exe -Algorithm SHA256
Get-Content .\tts-reader-windows-amd64.exe.sha256
```

## Запуск із вихідного коду

```powershell
go run . read -book book.txt -save book_save.json
```

Legacy запуск без subcommand також підтримується:

```powershell
go run . -book book.txt -save book_save.json
```

Локальний HTTP API:

```powershell
go run . serve
```

За замовчуванням API слухає тільки loopback-адресу і друкує одноразовий token для керування:

```text
Local TTS API listening on http://127.0.0.1:8080/?token=<token>
```

## Приклади

```powershell
go run . read -book "C:\Books\novel.txt" -save "C:\Books\novel.progress.json"
```

```powershell
go run . read -book book.txt -start "Розділ 3" -voice "Microsoft Irina Desktop"
```

```powershell
go run . read -book book.txt -chunk 400
```

```powershell
go run . read -book book.txt -tts-timeout 45s
```

## Local REST API

`serve` режим перетворює CLI на локальний керований TTS service:

```text
CLI / Browser
  ↓
Local Go API
  ↓
StreamingChunkReader
  ↓
Windows SAPI
  ↓
Windows Audio
```

Вбудована browser-панель доступна за URL, який друкує `serve`:

```text
http://127.0.0.1:8080/?token=<token>
```

OpenAPI contract:

```text
api/openapi.yaml
http://127.0.0.1:8080/api/openapi.yaml
```

API DTO у Go-коді відповідають схемам OpenAPI (`AddBookRequest`, `StartPlaybackRequest`, `SetPositionRequest`, `PublicBook`, `Voice`, `PlaybackState`). TTS backend ізольований через `TTSEngine`, тому HTTP handlers і тести не залежать від реального Windows audio.

Security model:

- HTTP server приймає тільки loopback bind, наприклад `127.0.0.1:8080` або `localhost:8080`.
- Middleware перевіряє `Host` і `Origin`, щоб стороння browser-сторінка не могла керувати локальним TTS service.
- `POST`, `PUT`, `PATCH`, `DELETE` і `GET /api/v1/events` потребують token через `X-TTS-Token` або `?token=`.
- HTTP API не приймає `save_file` і не повертає абсолютні filesystem paths; progress-файл є внутрішньою деталлю застосунку.
- Книга стартує тільки якщо файл не змінився після реєстрації: перевіряються size, modification time і sampled SHA-256 fingerprint.
- Помилки API повертаються у структурованому форматі `{ "code": "...", "error": "..." }`; `Stop` повертає playback snapshot навіть якщо progress не вдалося зберегти.
- JSON endpoints перевіряють `Content-Type: application/json`, unknown fields, required fields, UTF-8 byte positions і межі `chunk_size`.

Endpoints:

| Method | Path | Опис |
| --- | --- | --- |
| `GET` | `/api/openapi.yaml` | OpenAPI 3.1 contract. |
| `GET` | `/api/v1/voices` | Список доступних Windows SAPI голосів. |
| `POST` | `/api/v1/books` | Додати локальну книгу за file path. |
| `GET` | `/api/v1/books` | Список книг, зареєстрованих у поточному процесі. |
| `POST` | `/api/v1/playback` | Запустити читання книги. |
| `GET` | `/api/v1/playback` | Поточний стан playback. |
| `POST` | `/api/v1/playback/pause` | Пауза між TTS-фрагментами. |
| `POST` | `/api/v1/playback/resume` | Продовжити читання. |
| `POST` | `/api/v1/playback/stop` | Зупинити читання і зберегти позицію. |
| `PUT` | `/api/v1/playback/position` | Змінити byte-позицію читання. |
| `GET` | `/api/v1/events` | Server-Sent Events stream. |

Приклад додавання книги:

```powershell
$token = "<token from serve output>"
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/api/v1/books `
  -Headers @{ "X-TTS-Token" = $token } `
  -ContentType 'application/json' `
  -Body '{"path":"C:\\Books\\novel.txt","title":"Novel"}'
```

Приклад запуску:

```json
{
  "book_id": "book-1",
  "voice": "Microsoft Irina Desktop",
  "chunk_size": 400
}
```

Playback state:

```json
{
  "state": "playing",
  "book_id": "book-1",
  "progress_percent": 47.32,
  "current_byte": 918234,
  "voice": "Microsoft Irina Desktop",
  "chunk_size": 400
}
```

Voices response:

```json
{
  "voices": [
    { "name": "Microsoft Irina Desktop" }
  ]
}
```

SSE event types:

```text
playback.started
chunk.started
progress.updated
playback.paused
playback.resumed
playback.stopped
playback.finished
playback.failed
position.updated
```

## Прапорці CLI

| Прапорець | За замовчуванням | Опис |
| --- | --- | --- |
| `-book` | `book.txt` | Шлях до текстового файлу книги. |
| `-save` | `book_save.json` | Шлях до JSON-файлу прогресу. |
| `-start` | empty | Фраза для старту, яка ігнорує збережений прогрес. |
| `-voice` | empty | Точна назва голосу Windows SAPI. |
| `-chunk` | `250` | Максимальний розмір фрагмента в символах. |
| `-tts-timeout` | `2m0s` | Максимальний час очікування одного TTS-фрагмента. |

## Перевірки

```powershell
go mod verify
npx --yes @redocly/cli@2.38.0 lint api/openapi.yaml
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...
go build ./...
```

`golangci-lint` не є обов'язковою залежністю проєкту, але може запускатися додатково, якщо встановлений локально.

## Fuzzing

Проєкт має coverage-guided fuzz targets для складних UTF-8 і byte-offset сценаріїв:

- `FuzzChunkReader` — streaming chunk generation, malformed bytes, emoji, кирилиця, sentence boundaries.
- `FuzzUTF8Boundary` — перевірка byte-позицій на межах UTF-8 символів.
- `FuzzProgressLoad` — corrupted JSON progress і відновлення позиції.
- `FuzzStartPosition` — streaming-пошук стартової фрази.

Швидкий smoke-run:

```powershell
go test -run '^$' -fuzz=FuzzChunkReader -fuzztime=10s
go test -run '^$' -fuzz=FuzzUTF8Boundary -fuzztime=10s
go test -run '^$' -fuzz=FuzzProgressLoad -fuzztime=10s
go test -run '^$' -fuzz=FuzzStartPosition -fuzztime=10s
```

Довший локальний прогін перед релізом:

```powershell
go test -run '^$' -fuzz=FuzzChunkReader -fuzztime=2m
```

Основні інваріанти: chunks відновлюють оригінальний валідний UTF-8 текст, кожен chunk валідний UTF-8, фінальна byte-позиція дорівнює розміру вхідного тексту, а відновлена позиція прогресу не потрапляє всередину UTF-8 символу.

## Benchmarks і profiling

Порівняння старого full-book chunking baseline зі streaming chunk generation:

```powershell
go test -bench=BenchmarkChunker -benchmem
```

Швидка перевірка benchmark suite без довгого прогону:

```powershell
go test -run '^$' -bench=BenchmarkChunker_ASCII_1MB -benchmem -benchtime=1x
```

CPU і memory profiles:

```powershell
go test -run '^$' -bench=BenchmarkChunker -benchmem -cpuprofile=cpu.out -memprofile=mem.out
```

Перегляд profile у браузері:

```powershell
go tool pprof -http=:8080 cpu.out
```

Benchmark matrix містить ASCII і UTF-8 книги розміром 1 MB, 10 MB і 100 MB. Основні метрики: `ns/op`, `B/op`, `allocs/op`, startup latency і peak memory через pprof.

Локальний reference run на Windows amd64, `go test -run '^$' -bench=BenchmarkChunker -benchmem -benchtime=1x`:

| Implementation | Book | Time/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Original | ASCII 100 MB | 3.263s | 559,658,016 | 291,306 |
| Streaming | ASCII 100 MB | 3.038s | 111,863,280 | 291,279 |
| Original | UTF-8 100 MB | 3.103s | 354,900,544 | 150,366 |
| Streaming | UTF-8 100 MB | 3.435s | 110,660,176 | 150,341 |

`B/op` показує виділення пам'яті за операцію benchmark, а не точний RSS процесу; для peak memory використовуй `-memprofile` і `go tool pprof`.

## CI

GitHub Actions workflow у `.github/workflows/ci.yml` запускається на `windows-latest` і перевіряє форматування, OpenAPI contract, модулі, тести, race detector, `go vet`, `staticcheck`, `govulncheck` та збірку.

Release workflow у `.github/workflows/release.yml` запускається на tags `v*`, збирає `tts-reader-windows-amd64.exe`, створює SHA256 checksum і публікує обидва артефакти в GitHub Release.

## Файли користувача

`book.txt`, `book_save.json` і зібрані бінарні файли не мають потрапляти в репозиторій. Вони додані до `.gitignore`.

## Ліцензія

Проєкт поширюється за умовами MIT License. Деталі дивись у файлі `LICENSE`.
