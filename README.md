# wireprompt

**See every prompt, token and dollar your AI tools spend — live, locally, zero integration.**

wireprompt is a single-binary local proxy that records the LLM API traffic of any tool on your machine — Claude Code, Gemini CLI, coding agents, scripts, SDK apps — and shows you what's actually on the wire: full conversations, token usage, cache behavior, cost, latency and time-to-first-token. A Wireshark for your AI tools.

- **Zero integration** — `wireprompt run -- <any command>` wraps the tool and injects the provider base-URL env vars. No SDK, no code changes.
- **Local-first** — single Go binary, SQLite storage, embedded web UI. No account, no cloud, no telemetry. Your prompts never leave your machine.
- **Agent-native** — turn-delta collapsing shows exactly what your agent added to its context each turn; per-session tool analytics show which tool is eating your tokens; cache-bust detection flags the request that invalidated your prompt cache and what it cost you.
- **Wireshark depth** — persistent full-text-searchable history, request diffing, replay, raw SSE event inspection.

## Install

```sh
brew install zkrebbekx/tap/wireprompt        # macOS
go install github.com/zkrebbekx/wireprompt/cmd/wireprompt@latest
# or grab a binary from Releases
```

## Quick start

```sh
# capture one tool's traffic (starts the proxy if needed)
wireprompt run -- claude

# see it live
open http://127.0.0.1:9091/

# no traffic yet? load a sample agent session
wireprompt demo
```

Terminal rollups:

```sh
wireprompt stats -by session -since 24h
wireprompt search "that one prompt about rate limiting"
```

Point your own code at it without the wrapper:

```sh
eval "$(wireprompt env -session myproject)"
```

## What you get

**The list** — requests grouped by session with per-row stacked token bars (cache-read / cache-write / fresh-input / output). A healthy agent run is a wall of cache-blue; a cache bust shows up as a sudden pink/amber bar — and the session rollup names the exact request that busted it.

**The inspector** — every request rendered as a conversation: system prompt, collapsible message cards, dimmed thinking blocks, tool calls with inputs, paired tool results. Agent requests collapse the unchanged context prefix ("42 messages unchanged — expand") so you see exactly what this turn added. Tabs drop to raw JSON or the individual SSE events.

**The economics** — cost per request from per-model pricing tables (including Anthropic's 5m/1h cache-write tiers), a running "cache saved $X" ticker, context-window gauge, input-composition breakdown (system vs tools vs history vs new turn), tokens/sec and TTFT.

**The workbench** — full-text search across every prompt you've ever sent, two-request diff (spot prompt drift between runs), one-click replay (auth from env, never stored), copy-as-curl.

## Providers

Built in: Anthropic (`/anthropic`), OpenAI incl. responses API (`/openai`), Gemini (`/gemini`). Any OpenAI-compatible upstream works too:

```sh
wireprompt serve -upstream openrouter=https://openrouter.ai/api -upstream ollama=http://localhost:11434
```

OpenRouter's vendor-prefixed model ids and provider-reported costs are handled natively. Streaming OpenAI requests get `stream_options.include_usage` injected automatically so streamed token counts are never zero (disable with `"inject_usage": false` in the config).

## Configuration

`~/.config/wireprompt/config.json` (flags override):

```json
{
  "addr": "127.0.0.1:9091",
  "upstreams": { "openrouter": "https://openrouter.ai/api" },
  "redact": ["sk-[A-Za-z0-9-_]{10,}", "ghp_[A-Za-z0-9]{36}"],
  "no_bodies": false,
  "retention_days": 60
}
```

Pricing overrides live in `~/.config/wireprompt/pricing.json` — same schema as the embedded table, longest-prefix match on model id.

## Privacy & security

- Bodies are stored locally in `~/.wireprompt/wireprompt.db` (0600, dir 0700) so you can inspect conversations later. **Authorization headers and API keys are never stored.**
- The server binds `127.0.0.1` by default and validates the `Host` header, blocking DNS-rebinding attacks. Binding a non-loopback address requires `-token`.
- `redact` patterns scrub stored bodies; `-no-bodies` disables body storage entirely; `wireprompt prune -older-than 720h` (or `retention_days`) expires history.

## Development

```sh
go test -race ./...
```

PRs welcome. CI runs vet + race-enabled tests on Linux and macOS plus a goreleaser dry-run.

## License

MIT
