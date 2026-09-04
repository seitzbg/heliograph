# MCP client config example

A minimal stdio client config for `smoked mcp`. See the [README's MCP server
section](../../README.md#mcp-server) for the full tool list, env/flags, and the diff→apply safety
model.

```json
{
  "mcpServers": {
    "heliograph": {
      "command": "smoked",
      "args": ["mcp", "-url", "https://YOUR-HUB-HOSTNAME"],
      "env": {
        "HELIOGRAPH_BASIC_USER": "YOUR-PROXY-BASIC-AUTH-USER",
        "HELIOGRAPH_BASIC_PASS": "YOUR-PROXY-BASIC-AUTH-PASS",
        "HELIOGRAPH_ADMIN_PASS": "YOUR-ADMIN-PASSWORD"
      }
    }
  }
}
```

Replace the four placeholders and drop whichever `env` entries don't apply — `HELIOGRAPH_BASIC_USER`/
`HELIOGRAPH_BASIC_PASS` only matter if your hub sits behind a reverse proxy with Basic Auth (see
[Federation deployment](../../README.md#federation-deployment-reverse-proxy)), and
`HELIOGRAPH_ADMIN_PASS` is only needed to enable the config-write tools (`config_stage_*`,
`config_apply`) and unredacted admin reads; omit it to run read-only. `smoked` must be on `PATH`, or
replace `"command": "smoked"` with an absolute path to the binary.

Or add it with the Claude Code CLI instead of hand-editing JSON:

```sh
claude mcp add heliograph -- \
  smoked mcp -url https://YOUR-HUB-HOSTNAME \
  -basic-user "$HELIO_USER" -basic-pass "$HELIO_PASS" \
  -admin-pass "$HELIO_ADMIN"
```

Credentials here live only in this config file (or the shell environment that populates it) and in
the request Basic Auth / login call `smoked mcp` makes straight to your hub over the URL you gave
it — they are never sent anywhere else, logged, or held by the MCP client beyond passing them
through to the `smoked mcp` process it launches.
