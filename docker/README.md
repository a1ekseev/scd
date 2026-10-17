# Тестовые конфиги для docker-compose

**Временные тестовые конфиги** для локальных smoke-проверок `scd`, `sfc` и `gtl`.
Будут заменены реальными конфигами владельца. Реальных секретов в них нет:
все VLESS-ссылки фейковые (UUID `00000000-0000-4000-8000-00000000000N`,
хосты `*.example.com`, `203.0.113.x`, `198.51.100.x`, случайный REALITY `pbk`).

## Файлы

| Файл | Назначение |
| --- | --- |
| `scd/config.example.yml` | `scd` в режиме `daemon`, цель `xray:8080`, outbounds+inbounds+routing, статус-сервер `0.0.0.0:8080` |
| `sfc/config.example.yml` | `sfc`, источник `fixtures/subscription.b64`, выходы `/s/test-de`, `/s/test-nl`, `/s/test-all` |
| `gtl/config.example.yml` | `gtl`, `/load/<sizeKb>`, лимит 10240 KiB |
| `fixtures/subscription.txt` | 8 фейковых VLESS-ссылок: `tcp+tls`, `ws+tls`, `tcp+reality+vision` (DE/NL/FI/US) |
| `fixtures/subscription.b64` | та же подписка в base64 |

В `scd/config.example.yml` `fixedOutbounds` / `fixedInbounds` / `fixedRouting`
перечисляют статические теги из `xray/config/*.json` (`direct`, `blocked`,
`socks-proxy`, `wg-proxy`, `api`, ruleTag `direct` и `x-balancer`), чтобы sync
их не удалял. `observatorySubjectSelectorPrefix: x-observe-` совпадает с
селектором балансировщика и observatory. Мониторинг выключен — хосты фейковые.

## Использование

1. Скопировать примеры в игнорируемые git файлы, которые монтирует compose:

   ```bash
   for s in scd sfc gtl; do cp docker/$s/config.example.yml docker/$s/config.yml; done
   ```

2. Пути к фикстурам в конфигах относительные (`../fixtures/...`) и разрешаются
   от каталога конфига. В контейнере конфиг лежит в `/app/config/`, поэтому
   фикстуры нужно смонтировать в `/app/fixtures` для `scd` и `sfc`:

   ```yaml
   - ./docker/fixtures:/app/fixtures:ro
   ```

3. Проверка конфигов без Docker (из каталога сервиса, после `npm ci && npm run build`):

   ```bash
   node ./dist/cli.js validate-config --config ../docker/<svc>/config.yml
   ```

4. Запуск: `docker compose up -d --build`. Порты на хосте:
   - `scd` статус-страница: http://127.0.0.1:9090/
   - `sfc`: http://127.0.0.1:9091/s/test-all (и `/healthz`)
   - `gtl`: http://127.0.0.1:9092/load/32 (и `/healthz`)
