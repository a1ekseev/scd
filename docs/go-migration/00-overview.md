# Миграция blanc (scd + sfc + gtl) на Go: обзор и план

Ветка: `move-to-go`. Цель: переписать три TS/Node-сервиса на Go в **один бинарник**, полностью сохранив функциональность. Ключевое — внутренняя работа scd с Xray gRPC API.

## Документы

| Файл | Содержание |
|---|---|
| `07-decisions.md` | **Принятые решения — приоритет над остальными файлами** |
| `01-scd-domain.md` | scd: конфиг (zod-схема), загрузка input, разбор VLESS (WHATWG URL), теги, фильтры, топология, CLI, тексты ошибок, тест-кейсы |
| `02-scd-xray-api.md` | scd ↔ Xray: 9 RPC, protobuf-сообщения и номера полей, алгоритмы apply/rollback, найденные баги энкодера, Go-пакеты xray-core, стратегия тестов |
| `08-xray-26.10.10.md` | Дельта v26.3.27 → v26.10.10 (целевая версия Xray) |
| `03-scd-runtime.md` | scd: sync, daemon/cron-циклы, мониторинг/repair/rejoin, SOCKS5-клиент, status-сервер и JSON API, 34 лог-события, 66 тест-кейсов |
| `04-sfc.md` | sfc: CLI, конфиг, refresh/cron, дисковый кэш, HTTP, дубли с scd |
| `05-gtl.md` | gtl: генератор нагрузки (xorshift32, Go-порт с golden SHA-256), HTTP, конфиг |
| `06-go-stack.md` | Выбор библиотек, CLI/multicall, cron, YAML, валидация, логи, сборка, Docker, CI |

## Целевая архитектура

```
go.mod  module github.com/a1ekseev/blanc  (go 1.27.0, toolchain go1.27.2)
cmd/blanc/            один бинарь; multicall по argv[0]: `blanc scd daemon` ≡ `scd daemon`
internal/common/      cron, cronloop, logging (pino-совместимый slog), configload (${ENV}+YAML),
                      schema (zod-подобные ошибки), jsregex (regexp2/ECMAScript), listenaddr, subscription/scanlines
internal/scd/         config, input, normalize, topology, builders, xrayapi (xray-core типы + grpc),
                      apply, runtime (sync, daemon, monitoring, socks), status (html/template + embed)
internal/sfc/         config, input, refresh, cache, server
internal/gtl/         config, generator, server
```
`internal/{scd,sfc,gtl}` не импортируют друг друга. Разошедшиеся копии (`parse-subscription-line`, `load-input`) остаются раздельными до паритетных тестов.

Docker: один Dockerfile, target'ы `scd`/`sfc`/`gtl` (distroless/static, кросс-компиляция amd64/arm64 без QEMU). Образы `ghcr.io/a1ekseev/{scd,sfc,gtl}` сохраняются.

## Стек (кратко, детали — 06 + 07)
Go 1.27.2 · cobra · свой cron-парсер (семантика cron-parser v5) · `go.yaml.in/yaml/v3` · свой schema-пакет · `log/slog` с pino-форматом · `dlclark/regexp2` · `github.com/xtls/xray-core` (v26.10.10, псевдоверсия по коммиту) + `google.golang.org/grpc` · ручной SOCKS5 · `html/template`+`embed` · `x/text/collate` · тесты: testing, httptest, bufconn, synctest · CI: golangci-lint v2.15.0, govulncheck, `go test -race`.

## Главные риски паритета
1. **Xray API** (02, 08): точная последовательность RPC, fixed-ресурсы, rollback только outbound, per-target lock; два бага кодирования TS (ws `protocol_name`, TLS fingerprint) — в Go исправляются (D2).
2. **Разбор VLESS** (01): WHATWG URL ≠ `net/url`; хэш тега по необрезанной строке; JS `trim` ≠ `strings.TrimSpace` (BOM/NEL); `decodeURIComponent` падает на `%`.
3. **Cron** (06, 03): ни одна Go-библиотека не совпадает с cron-parser (сек. поле, dow 7, `L`/`#`, OR dom/dow) → свой парсер + golden из Node.
4. **Валидация конфига** (01, 04, 05): тексты zod v4 и три разных формата ошибок; порядок cross-field проверок.
5. **Мелкие контракты**: JSON (2 пробела, порядок ключей, без экранирования `<>&`), ICU-сортировка, lenient base64 Node, `User-Agent: node`, тексты сетевых ошибок, логи pino.
6. **UI** (D3): визуальное совпадение, проверка скриншотами; JSON API — строгий контракт.

## План работ

**Этап 0. Эталоны (до удаления TS).**
Скрипты в `tools/golden/` (Node 24.14): golden-выходы `generate`/`print-config`/`sync`-отчётов, разбор VLESS на корпусе граничных строк, cron `next()` на наборе выражений, тексты ошибок валидации, байты gtl, JSON API status-сервера, скриншоты дашборда (Playwright). Перепроверить на Node 24.14 названия стран и `accept-encoding` (01 отмечает).

**Этап 1. Каркас.** `go.mod`, `cmd/blanc` (multicall), `internal/common/{logging,configload,schema,cron,cronloop}`, `.golangci.yml`, CI-джоба Go параллельно TS.

**Этап 2. gtl** (самый простой, проверяет каркас, Docker target, CI): генератор с golden SHA-256.

**Этап 3. sfc**: input, парсинг, кэш, refresh, сервер.

**Этап 4. scd домен**: config, input, normalize, topology, builders, generate.

**Этап 5. scd Xray API**: `xrayapi.Client` на типах xray-core v26.10.10, apply (outbound/inbound/routing), фейковый Xray на bufconn, smoke против `ghcr.io/xtls/xray-core:26.10.10`.

**Этап 6. scd runtime**: sync, daemon, monitoring/repair/rejoin, SOCKS5-проба, remote push, status-сервер + дашборд (visual regression).

**Этап 7. Переключение.** Единый Dockerfile, compose, CI/publish, удаление TS-пакетов, обновление `AGENTS.md` (корневой + сервисные: один Go-модуль, разрешённые `internal/common`, новые команды проверки), README.

Критерий готовности каждого этапа: перенесены все тест-кейсы из инвентаря соответствующего документа + golden из этапа 0, `golangci-lint` и `go test -race` зелёные.

## Открытые вопросы к владельцу
1. Подтвердить исправление багов кодирования ws path/Host и TLS fingerprint (D2) — рекомендуется.
2. Повторять ли «баги» Node: SIGTERM ждёт следующего cron-слота (scd, sfc); переполнение `setTimeout` >24.8 дней; зависание gtl-обработчика при обрыве клиента. Рекомендация — не повторять (быстрый graceful shutdown).
3. `labelIncludeRegex`: `regexp2` (ECMAScript, совместимо с README) — рекомендуется — или RE2 с ограничениями.
4. Тексты сетевых ошибок (`fetch failed`, ошибки сокетов) — дословно как в Node или go-нативные. Рекомендация: дословно там, где они видны пользователю (дашборд, Remote Push, отчёты).
5. Docker: публиковать дополнительно `ghcr.io/a1ekseev/blanc`? Пользователи, переопределявшие CMD `node dist/cli.js …`, должны будут его поменять.
6. `allowInsecure=1` в подписках: Xray v26.10.10 его больше не поддерживает. Рекомендация — warning в логах, параметр не передаётся (D4).
7. Нет `docker/{scd,sfc,gtl}/config.yml` — для smoke-проверок нужны образцы конфигов.
