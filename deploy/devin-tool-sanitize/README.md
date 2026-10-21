# devin-tool-sanitize

Loopback HTTP sanitizer that sits in front of the local `CLIProxyAPI` Devin
bridge and scrubs `tool` / `tool` markers plus overlong descriptions out of
request bodies before they reach Cognition's swe-2 upstream.

## Why

Devin swe-2 rejects requests whose payload mentions tool servers (or whose
tool descriptions exceed ~480 bytes) with:

```
403 permission_denied: devin upstream error: Unable to process request due to
an tool configuration issue.
```

`CLIProxyAPI` does not rewrite descriptions, so this proxy shortens and
rewrites them first. Tool *names* are left untouched: the model echoes them
back in `tool_use` blocks and one-way renaming would break client dispatch.

## Topology

```
sub2api (account "Devin Claude loopback", group devin2claude)
  -> 127.0.0.1:8318   devin-tool-sanitize (this proxy)
  -> 127.0.0.1:8317   CLIProxyAPI Devin
  -> Cognition Devin upstream
```

The sub2api account's upstream URL must point at `:8318`, not `:8317`.

## Install

```bash
install -Dm755 proxy.py ~/.local/opt/devin-tool-sanitize/proxy.py
install -Dm644 devin-tool-sanitize.service \
  ~/.config/systemd/user/devin-tool-sanitize.service
systemctl --user daemon-reload
systemctl --user enable --now devin-tool-sanitize
```

The unit expects `cliproxyapi-devin.service` to exist alongside it.

## Debugging

`DEVIN_SANITIZE_DUMP=1` (set in the unit) writes raw/sanitized bodies to
`/tmp/devin-sanitize-<ts>-raw.json` / `-out.json` whenever the body changes.
Diff the pair to see which field still carries a residual `tool` token; add
its key to `SCRUB_STRING_KEYS` in `proxy.py` and restart.
