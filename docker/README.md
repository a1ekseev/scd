# Тестовые конфиги для docker-compose

**Временные тестовые конфиги** для локальных smoke-проверок `scd`, `scd sfc` и `scd gtl`.
Будут заменены реальными конфигами владельца. Реальных секретов в них нет:
все VLESS-ссылки фейковые (UUID `00000000-0000-4000-8000-00000000000N`,
хосты `*.example.com`, `203.0.113.x`, `198.51.100.x`, случайный REALITY `pbk`).

## Один образ

Все три сервиса — один бинарь `scd` и один образ `ghcr.io/a1ekseev/scd`
(`Dockerfile` в корне репозитория). Образы `sfc`/`gtl` больше не публикуются.
В `docker-compose.yml` три сервиса собираются из одного `build:` в один
`image: ghcr.io/a1ekseev/scd:local` и различаются только `command:`:

| Сервис | `command:` |
| --- | --- |
| `scd` | по умолчанию (`CMD ["scd","daemon","--config","/app/config/config.yml"]`) |
| `sfc` | `["scd","sfc","serve","--config","/app/config/config.yml"]` |
| `gtl` | `["scd","gtl","serve","--config","/app/config/config.yml"]` |

Конфиг — по-прежнему `/app/config/config.yml`, кэш sfc — volume
`/app/config/.sfc-cache`. Образ работает от `65532:65532` (не root): каталог
кэша sfc на bind mount или в volume от старого root-образа нужно один раз
`chown -R 65532:65532`. Smoke проверяет пользователя всех трёх сервисов и что
sfc записал кэш. Тестовый Xray — `ghcr.io/xtls/xray-core:26.10.21`;
версия зафиксирована в `.xray-version` и обновляется только вместе с
xray-core в `go.mod` (проверка: `bash scripts/check-xray-version.sh`).

## Файлы

| Файл | Назначение |
| --- | --- |
| `scd/config.example.yml` | `scd` в режиме `daemon`, подписка с одним источником `vless` (`fixtures/subscription.txt`, порты `20000..20099`), цель `xray:8080`, outbounds+inbounds+routing, `balancerSelection`, статус-сервер `0.0.0.0:8080` |
| `sfc/config.example.yml` | `scd sfc`, источник `fixtures/subscription.b64`, выходы `/s/test-de`, `/s/test-nl`, `/s/test-all` |
| `gtl/config.example.yml` | `scd gtl`, `/load/<sizeKb>`, лимит 10240 KiB |
| `*/config.real-shape.yml`, `*/config.real-shape.go.yml` | форма боевых конфигов (TS → Go) |
| `compose.smoke.yml` | override для `scripts/compose-smoke.sh`: монтирует `config.example.yml` вместо `config.yml` (для scd — временную копию с sync раз в 5 секунд из `SCD_SMOKE_SCD_CONFIG`) |
| `fixtures/subscription.txt` | 8 фейковых VLESS-ссылок: `tcp+tls`, `ws+tls`, `tcp+reality+vision` (DE/NL/FI/US) |
| `fixtures/subscription.b64` | та же подписка в base64 |
| `xray/config/*.json` | конфиг тестового Xray (`-confdir`) |

В `scd/config.example.yml` `fixedOutbounds` / `fixedInbounds` / `fixedRouting`
перечисляют статические теги из `xray/config/*.json` (`direct`, `blocked`,
`socks-proxy`, `wg-proxy`, `api`, ruleTag `direct` и `x-balancer`), чтобы sync
их не удалял: каждый sync сначала удаляет «сирот» — не fixed-ресурсы вне
пространств имён включённых источников (`x-observe-vless-…`, `vless-…`,
`in-vless-…`, `route-vless-…`); правила без `ruleTag` не трогаются.
`observatorySubjectSelectorPrefix: x-observe-` совпадает с
селектором балансировщика и observatory. Мониторинг выключен — хосты фейковые.
Подписка задаётся только списком `sources`: старые ключи подписки (`input`,
`format`, `filters`, `fetch*`, `allowInsecure`) и `target.inboundSocks.portRange`
`validate-config` отклоняет как неизвестные; `config.real-shape.yml` —
старая форма, `config.real-shape.go.yml` — текущая.

`balancerSelection` (карточка «Xray selection» на дашборде) опрашивает Xray
read-only RPC `RoutingService.GetBalancerInfo` и
`ObservatoryService.GetOutboundStatus`, поэтому в `xray/config/01_api.json`
включён `ObservatoryService`, а в `05_observatory.json` есть секция
`observatory` — без них Xray с таким конфигом не стартует.

## Использование

1. Проверка конфигов без Docker (из корня репозитория):

   ```bash
   go build -o bin/scd ./cmd/scd
   ./bin/scd validate-config --config docker/scd/config.example.yml
   ./bin/scd sfc validate-config --config docker/sfc/config.example.yml
   ./bin/scd gtl validate-config --config docker/gtl/config.example.yml
   ```

2. Smoke всего стека (собирает образ, поднимает Xray + три сервиса на
   примерах, проверяет ответы и всегда убирает за собой; игнорируемые
   `docker/*/config.yml` не читает и не трогает). В конце перезапускает Xray
   и ждёт, что scd заметит потерю ресурсов (`xray_state_drift`) и применит их
   снова; для этого scd в smoke синхронизируется раз в 5 секунд:

   ```bash
   bash scripts/compose-smoke.sh
   ```

   Проект compose — `scd-smoke`, но `container_name` и порты фиксированы:
   если уже запущен стек из `docker-compose.yml`, скрипт сообщит о конфликте
   и выйдет — сначала `docker compose down`.

3. Ручной запуск: compose монтирует игнорируемые git файлы
   `docker/<svc>/config.yml` (свои конфиги; для теста — копия примера,
   `cp docker/<svc>/config.example.yml docker/<svc>/config.yml`, если файла
   ещё нет). Пути к фикстурам в конфигах относительные (`../fixtures/...`) и
   разрешаются от каталога конфига; в контейнере конфиг лежит в `/app/config/`,
   поэтому compose монтирует `./docker/fixtures` в `/app/fixtures` для `scd` и
   `sfc`. Запуск: `docker compose up -d --build`. Порты на хосте:
   - `scd` статус-страница: http://127.0.0.1:9090/
   - `sfc`: http://127.0.0.1:9091/s/test-all (и `/healthz`)
   - `gtl`: http://127.0.0.1:9092/load/32 (и `/healthz`)
