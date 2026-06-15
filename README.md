# Audiobook TTS Reader

Консольний інструмент на Go для озвучення текстових книг у Windows Desktop із автоматичним збереженням прогресу.

## Платформа

Застосунок використовує Windows SAPI через PowerShell і призначений для Windows 10/11. Контейнерний запуск у Docker або Kubernetes/minikube не є коректною runtime-моделлю для цього проєкту, бо Linux-контейнер не має доступу до Windows SAPI та desktop audio session.

## Можливості

- Озвучення тексту встановленими голосами Windows SAPI.
- Старт із збереженої byte-позиції або з указаної фрази.
- Розбиття тексту на фрагменти з урахуванням UTF-8 і меж речень.
- Збереження прогресу після кожного фрагмента, при TTS-помилці та при `Ctrl+C`.
- Валідація файлу прогресу, `-chunk`, `-tts-timeout` і UTF-8 меж перед відновленням читання.
- Timeout для кожного TTS-фрагмента, щоб несправний Windows SAPI/audio stack не блокував CLI назавжди.
- Регресійні тести для failure scenarios без запуску реального TTS.

## Вимоги

- Windows 10/11.
- Go 1.25.11 або сумісний patched toolchain.
- PowerShell із доступом до `System.Speech`.

## Запуск із вихідного коду

```powershell
go run . -book book.txt -save book_save.json
```

## Приклади

```powershell
go run . -book "C:\Books\novel.txt" -save "C:\Books\novel.progress.json"
```

```powershell
go run . -book book.txt -start "Розділ 3" -voice "Microsoft Irina Desktop"
```

```powershell
go run . -book book.txt -chunk 400
```

```powershell
go run . -book book.txt -tts-timeout 45s
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
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...
go build ./...
```

`golangci-lint` не є обов'язковою залежністю проєкту, але може запускатися додатково, якщо встановлений локально.

## CI

GitHub Actions workflow у `.github/workflows/ci.yml` запускається на `windows-latest` і перевіряє форматування, модулі, тести, race detector, `go vet`, `staticcheck`, `govulncheck` та збірку.

## Файли користувача

`book.txt`, `book_save.json` і зібрані бінарні файли не мають потрапляти в репозиторій. Вони додані до `.gitignore`.
