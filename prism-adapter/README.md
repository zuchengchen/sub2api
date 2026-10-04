# Prism OAuth adapter (gpt-6.1-sol)

Loopback Chromium sidecar for HTTP `/v1/responses` whose mapped upstream model
is `gpt-6.1-sol` (including `gpt-6.1-sol-max` → Extra high / `xhigh`). This
fork enables Prism for every schedulable OpenAI OAuth account when
`gateway.prism_browser.enabled` is true. Extra `openai_prism_browser: false`
force-off a single account. No admin checkbox is required.

`gpt-5.6-sol`, terra, luna, API Key, native WebSocket, compact, images, and
`previous_response_id` stay on Codex / BPS / Cookie WS.

Busy adapter, adapter down, timeout, and missing Prism 6.1-sol entitlement
fall back to the same account's Tibo / BPS / Cookie WS / HTTP path. Compact,
images, and `previous_response_id` return 422 with no fallback.

Successful turns estimate input+output tokens and bill the existing
gpt-6.1-sol 2× official card (`X-Prism-Usage: estimated`).

## Protocol

- Adapter listens only on loopback (`http://127.0.0.1:8319/v1`).
- OAuth access token is sent in `X-Prism-OAuth-Token`. No proxy, redirect, or
  plugin transport.
- Exact model ID `gpt-6.1-sol` and efforts `low` / `medium` / `high` /
  `xhigh`. Gateway folds `max` → `xhigh` before the adapter.
- 6.1-sol client tool bridge is on by default
  (`PRISM_ADAPTER_CLIENT_TOOLS_ENABLED=true`). Missing `jsonschema` / `lark`
  fails adapter startup.
- Projects are not auto-deleted.

## Runtime

Python 3.12, `requirements.txt` wheels only:

```sh
pip install --only-binary=:all: -r requirements.txt
```

This host cannot download official Playwright Chromium (CDN 403). Use npmmirror:

```sh
export PLAYWRIGHT_DOWNLOAD_HOST=https://npmmirror.com/mirrors/playwright
python -m playwright install chromium
```

Run as a non-root user with Chromium sandbox. Do not use `--no-sandbox`.
Install path `/opt/sub2api/prism-adapter`, state `/var/lib/sub2api-prism`,
restricted env `/etc/sub2api-prism.env` (root, `0600`). See
`deploy/sub2api-prism-adapter.service` and `deploy/sub2api-prism.env.example`.

systemd stays independent of sub2api. Suggested first multiplex settings:

```dotenv
PRISM_ADAPTER_MODE=multiplex
PRISM_ADAPTER_MAX_INFLIGHT=4
PRISM_ADAPTER_ACCOUNT_MAX_INFLIGHT=4
PRISM_ADAPTER_MAX_QUEUED=20
PRISM_ADAPTER_CLIENT_TOOLS_ENABLED=true
```

Queue wait of 15s returns 429; the gateway then falls back. Raise inflight
toward 20 only after RSS stays under `MemoryMax=900M` and 429 is not the
common path.

## Offline checks

```sh
python -m unittest discover -s prism-adapter -p 'test_*.py'
cd backend
go test ./internal/service -run 'TestPrismBrowser|TestAccountUsesPrism|TestPrismClient' -count=1
```

Browser smoke (local Chromium, synthetic pages, no OAuth):

```sh
python prism-adapter/smoke_browser.py --chrome /absolute/path/to/chromium
python prism-adapter/smoke_client_tools.py --chrome /absolute/path/to/chrome-headless-shell
```

Live owner-machine checks (account 23136) are not a merge gate: one text Tibo
True on 6.1-sol xhigh through the sidecar, and one forced adapter-down fallback
to Codex.

Rollback: `gateway.prism_browser.enabled: false` or stop the adapter, then
restart sub2api.
