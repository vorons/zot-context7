# zot-context7

**zot-context7** is a [zot](https://github.com/patriceckhart/zot) extension that wraps the
[Context7](https://context7.com) documentation API with a local SQLite cache.
It provides a `/context7` slash command and a `ctx7` LLM-callable tool.

## Installation

```bash
# Build and install from source
cd zot-context7
go build -ldflags="-s -w" -o zot-context7 .
zot ext install ./zot-context7

# Or install directly from the repo (if published)
zot ext install https://github.com/vorons/zot-context7
```

Restart zot (or run `/reload-ext`). The extension registers automatically.

### Prerequisites

- Go 1.22+
- zot 0.x with extension support

## Configuration

Create `config.json` in the extension's data directory
(`~/.local/state/zot/extensions/zot-context7/config.json`):

```json
{
  "apiKey": "ctx7sk-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "cacheTtlHours": 168,
  "defaultTokenLimit": 5000
}
```

The API key is **optional**. Without it, requests go to Context7 unauthenticated
with lower rate limits. With a key, you get higher limits.

The API key can also be set via the `CONTEXT7_API_KEY` environment variable.
Config file takes precedence.

## Usage

### Slash command: `/context7`

```
/context7 docs <library> <query>      Get documentation (cache-first, API fallback)
  --no-cache                           Force fresh API fetch
  --json                               JSON output
  --tokens N                           Max tokens

/context7 search <query>               Full-text search over cached docs
  --json                               JSON output
  --limit N                            Max results (default 20)

/context7 libs                         List cached libraries
  --json                               JSON output

/context7 cache stats                  Cache hit rate, tokens saved, size
/context7 cache clear                  Wipe local cache

/context7 doctor                       Health check (DB, API key, connectivity)
```

#### Examples

```text
# Fetch docs
/context7 docs lodash deepMerge

# Fetch with custom token limit
/context7 docs fastapi dependency-injection --tokens 3000

# Force refresh
/context7 docs react hooks --no-cache

# Search cached content
/context7 search "error handling"

# Check cache stats
/context7 cache stats

# Diagnostics
/context7 doctor
```

### LLM tool: `ctx7`

> *Fetch up-to-date docs and code examples for any library or framework.
> Call before implementing anything involving a third-party dependency.*

The agent can call `ctx7` directly with two actions:

**`search`** — find libraries matching a topic or task:
```json
{"action": "search", "query": "react hooks"}
```

**`docs`** — fetch docs and code examples for a specific library:
```json
{"action": "docs", "library": "lodash", "query": "deep merge objects"}
```

The `library` parameter accepts a package name or Context7 ID (e.g. `/facebook/react`).
The `query` parameter should be a specific topic or question — narrow queries return better results.

Returns markdown-formatted documentation and code examples.

## How it works

```
/context7 docs lodash merge
        │
        ▼
  Open ~/.context7-cache/docs.db
        │
        ├── Cache hit (fresh)? ──yes──► Return cached result
        │
        │   no
        │
        ├── Resolve library name (DB → API)
        ├── Fetch from Context7 API (key optional — unauthenticated with rate limits)
        ├── Store in query_cache + snippets + FTS index
        └── Return formatted markdown
```

- **Cache location:** `~/.context7-cache/docs.db`
- **TTL:** Configurable via `config.json` (default 7 days)
- **Full-text search:** FTS5 when available, `LIKE` fallback otherwise
- **Token tracking:** Every request is logged in `token_stats` for hit rate analysis

## Commands reference

| Slash command | Tool name | Description |
|---|---|---|
| `/context7` | — | Documentation cache CLI |
| — | `ctx7` | LLM-callable search/docs tool |

## Development

```bash
# Build
go build -ldflags="-s -w" -o zot-context7 .

# Test
go test -v -count=1 ./...

# Install locally
zot ext install ./zot-context7

# Reload in running zot
/reload-ext
```

## License

MIT
