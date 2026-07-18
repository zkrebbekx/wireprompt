# wireprompt

**See every prompt, token and dollar your AI tools spend — live, locally, zero integration.**

wireprompt is a single-binary local proxy that records the LLM API traffic of any tool on your machine — Claude Code, coding agents, scripts, SDK apps — and shows you what's actually on the wire: full conversations, token usage, cache hits, cost, latency and time-to-first-token. A Wireshark for your AI tools.

- **Zero integration** — `wireprompt run -- <any command>` wraps the tool and injects `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL`. No SDK, no code changes.
- **Local-first** — single Go binary, SQLite storage, embedded web UI. No account, no cloud, no telemetry. Your prompts never leave your machine.
- **LLM-native** — reassembles SSE streams, renders conversations and tool calls, computes cost from per-model pricing tables, tracks TTFT.
- **Persistent** — every request is history you can search, inspect and aggregate. Not an ephemeral dashboard.

## Quick start

```sh
# capture one tool's traffic (starts the proxy if needed)
wireprompt run -- claude

# or run the proxy + web UI standalone
wireprompt serve            # ui at http://localhost:9091/

# cost rollups in the terminal
wireprompt stats -by model -since 24h
```

Point your own code at it without the wrapper:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:9091/anthropic
export OPENAI_BASE_URL=http://127.0.0.1:9091/openai/v1
```

Sessions group traffic per tool run: `wireprompt run -session refactor -- aider`, or bake the session into the base URL path: `/s/<name>/anthropic`.

## Providers

Built in: Anthropic (`/anthropic`) and OpenAI (`/openai`). Any OpenAI-compatible upstream (OpenRouter, Ollama, vLLM, …) can be added:

```sh
wireprompt serve -upstream openrouter=https://openrouter.ai/api -upstream ollama=http://localhost:11434
export OPENAI_BASE_URL=http://127.0.0.1:9091/openrouter/v1
```

## Pricing

Costs are computed from an embedded per-model price table (USD per million tokens, including cache read/write rates). Override or extend it at `~/.config/wireprompt/pricing.json` — same schema, longest-prefix match on model id wins.

## Privacy

Request and response bodies are stored locally in `~/.wireprompt/wireprompt.db` so you can inspect conversations later. Authorization headers and API keys are **never** stored. Delete the database at any time.

## Status

v0.1 — capture, live feed, conversation inspection, cost rollups. Planned: search and filter DSL, request diffing, replay, redaction rules, Gemini, opt-in MITM mode for tools that ignore base-URL overrides.

## License

MIT
