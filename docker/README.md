# Тестовые конфиги для docker-compose

**Временные тестовые конфиги** для локальных smoke-проверок `scd`, `scd sfc`, `scd gtl` и `scd wgp`.
Будут заменены реальными конфигами владельца. Реальных секретов в них нет:
все VLESS-ссылки фейковые (UUID `00000000-0000-4000-8000-00000000000N`,
хосты `*.example.com`, `203.0.113.x`, `198.51.100.x`, случайный REALITY `pbk`).

## Один образ

Все четыре сервиса — один бинарь `scd` и один образ `ghcr.io/a1ekseev/scd`
(`Dockerfile` в корне репозитория). Образы `sfc`/`gtl` больше не публикуются.
В `docker-compose.yml` четыре сервиса собираются из одного `build:` в один
`image: ghcr.io/a1ekseev/scd:local` и различаются только `command:`:

| Сервис | `command:` |
| --- | --- |
| `scd` | по умолчанию (`CMD ["scd","daemon","--config","/app/config/config.yml"]`) |
| `sfc` | `["scd","sfc","serve","--config","/app/config/config.yml"]` |
| `gtl` | `["scd","gtl","serve","--config","/app/config/config.yml"]` |
| `wgp` | `["scd","wgp","serve","--config","/app/config/config.yml"]` |

Конфиг — по-прежнему `/app/config/config.yml`, кэш sfc — volume
`/app/config/.sfc-cache`. Образ работает от `65532:65532` (не root): каталог
кэша sfc на bind mount или в volume от старого root-образа нужно один раз
`chown -R 65532:65532`. Для wgp образ так же создаёт `/app/state` (0700) и
`/app/out`, для scd — `/app/wgp` (все владельца 65532). Smoke проверяет
пользователя всех четырёх сервисов и что sfc записал кэш.

## wgp в compose

- Сервис `wgp` — в профиле compose `wgp`: обычный `docker compose up -d`
  поднимает Xray, scd, sfc и gtl и не требует конфига, state и ключа wgp
  (том `wgp-out` у scd тогда пустой). С wgp —
  `docker compose --profile wgp up -d` или `COMPOSE_PROFILES=wgp`;
  `scripts/compose-smoke.sh` включает профиль сам.
- Сеть `wgp-egress` — только у wgp (выход в интернет к API провайдеров);
  общей сети со scd и Xray нет. `read_only: true`, `cap_drop: [ALL]`,
  `no-new-privileges`. Страница статуса — `127.0.0.1:9093`.
- Том `wgp-out`: у wgp `/app/out` (rw), у scd `/app/wgp:ro`. wgp пишет туда
  файлы кандидатов и `status.json` с правами 0600 от 65532; scd работает от
  того же 65532 и читает их, больше никто. Новый именованный том получает
  владельца 65532 из образа (`/app/out` и `/app/wgp` в образе принадлежат 65532),
  в каком бы порядке контейнеры ни стартовали.
- State — bind mount `./docker/wgp/state:/app/state` (только у wgp), каталог
  0700 владельца 65532.
- Ключ state — secret `wgp_state_key` из `./secrets/wgp_state_key`
  (base64 32 случайных байт). Вне swarm compose монтирует файл secret'а как
  bind mount с владельцем и правами хоста (`uid`/`gid`/`mode` не действуют),
  поэтому владелец должен быть 65532, права 0400 или 0600.
- Провайдер static читает `./docker/wgp/static/*.conf` (`:ro`): файлы без
  прав для «прочих» (0600 или 0640) владельца 65532.
- Провайдер proton (в примере `enabled: false`) требует входа — один раз на
  хосте, при остановленном `serve` (lock каталога state); `run` берёт те же
  тома, сеть и secret:

  ```bash
  docker compose --profile wgp stop wgp
  docker compose --profile wgp run --rm -it wgp scd wgp login --provider proton --config /app/config/config.yml
  docker compose --profile wgp start wgp
  ```

  Выход и отзыв своих сертификатов — так же, командой
  `scd wgp logout --provider proton --config /app/config/config.yml`.
- Подготовка на хосте (Linux):

  ```bash
  mkdir -p secrets docker/wgp/state docker/wgp/static
  head -c 32 /dev/urandom | base64 > secrets/wgp_state_key
  chmod 0400 secrets/wgp_state_key && chmod 0700 docker/wgp/state
  chmod 0600 docker/wgp/static/*.conf
  sudo chown 65532:65532 secrets/wgp_state_key docker/wgp/state docker/wgp/static/*.conf
  ```

  Docker Desktop на macOS показывает файлы bind mount как принадлежащие
  читающему пользователю, там `chown` не нужен (права 0600/0700 всё равно
  нужны). `secrets/`, `docker/wgp/state/` и `docker/wgp/static/` — в
  `.gitignore`. Тестовый Xray — `ghcr.io/xtls/xray-core:26.10.21`;
версия зафиксирована в `.xray-version` и обновляется только вместе с
xray-core в `go.mod` (проверка: `bash scripts/check-xray-version.sh`).

## Файлы

| Файл | Назначение |
| --- | --- |
| `scd/config.example.yml` | `scd` в режиме `daemon`, подписка с источником `vless` (`fixtures/subscription.txt`, порты `20000..20099`), выключенными источниками `wireguard` `wg` (порты `20100..20109`) и `masque` `mq` (порты `20110..20119`; файлов кандидатов нет), цель `xray:8080`, outbounds+inbounds+routing, `balancerSelection`, статус-сервер `0.0.0.0:8080` |
| `sfc/config.example.yml` | `scd sfc`, источник `fixtures/subscription.b64`, выходы `/s/test-de`, `/s/test-nl`, `/s/test-all` |
| `gtl/config.example.yml` | `scd gtl`, `/load/<sizeKb>`, лимит 10240 KiB |
| `wgp/config.example.yml` | `scd wgp`: общие ключи (`server`, `state`, `deviceNamePrefix`, `api`), провайдер `static` (`/app/static`, все ключи с умолчаниями) и выход `wg` → `/app/out/wg`, который читает источник `wg` из `scd/config.example.yml`; выключенные `proton` (`identification.proton`, `keys.revokeOldAfterMinutes`, выход `proton`), `warp` и `warpmq`. Каталоги `secrets/`, `docker/wgp/state/` и `docker/wgp/static/` в `.gitignore` |
| `*/config.real-shape.yml`, `*/config.real-shape.go.yml` | форма боевых конфигов (TS → Go) |
| `compose.smoke.yml` | override для `scripts/compose-smoke.sh`: монтирует `config.example.yml` вместо `config.yml` (для scd — временную копию с sync раз в 5 секунд и включённым источником `wg` из `SCD_SMOKE_SCD_CONFIG`); у wgp — state в именованном томе, копия `fixtures/static/*.conf` с правами 0600 (`SCD_SMOKE_WGP_STATIC`) и новый ключ state (`SCD_SMOKE_WGP_KEY`), оба во временном каталоге скрипта |
| `fixtures/subscription.txt` | 8 фейковых VLESS-ссылок: `tcp+tls`, `ws+tls`, `tcp+reality+vision` (DE/NL/FI/US) |
| `fixtures/subscription.b64` | та же подписка в base64 |
| `fixtures/wireguard/candidates.txt` | фейковые WireGuard-кандидаты (ключи из повторяющихся байтов, адреса 192.0.2.0/24) и строки, которые пропускаются: `wireguard_invalid`, `unsupported_scheme`, `duplicate_tag`. Тесты копируют файл с правами 0600 |
| `fixtures/masque/candidates.txt` | фейковые MASQUE-кандидаты (ключи P-256 сгенерированы один раз, адреса 192.0.2.0/24): строки h3 и h2 двух ключей устройства, строка h3 третьего (`sni` = IP endpoint'а) и строки, которые пропускаются: `masque_invalid`, `unsupported_scheme`. Тесты копируют файл с правами 0600 |
| `fixtures/static/*.conf` | фейковые `.conf` для провайдера static (ключи из повторяющихся байтов, адреса 192.0.2.0/24, метки 🇳🇱/🇨🇭): все поля, умолчания `defaults`, `# Label`, `# Reserved`, игнорируемые `ListenPort` и `PostUp`. В git права 0644, поэтому тесты и smoke копируют их с правами 0600 |
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
   ./bin/scd wgp validate-config --config docker/wgp/config.example.yml
   ```

2. Smoke всего стека (собирает образ, поднимает Xray + четыре сервиса на
   примерах, проверяет ответы и всегда убирает за собой; игнорируемые
   `docker/*/config.yml` не читает и не трогает). wgp публикует
   `fixtures/static/*.conf`; smoke проверяет `/api/status` wgp, что
   `wg` и `status.json` на томе — файлы 0600 владельца 65532, что scd
   через `:ro` прочитал их (блок `wgp` и туннели `protocol: wireguard` в
   `/api/instance-status`) и что WireGuard-outbound'ы есть в Xray (туннели не
   поднимаются: ключи фейковые). На Linux скрипт отдаёт ключ и `.conf`
   владельцу 65532 через `sudo -n chown` (нужен root или sudo без пароля, как
   на раннерах CI; без них скрипт останавливается с ошибкой). В конце перезапускает Xray
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
   - `wgp` страница статуса: http://127.0.0.1:9093/ (и `/api/status`, `/healthz`);
     перед запуском — подготовка из «wgp в compose»
