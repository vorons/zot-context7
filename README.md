# zot-context7

**zot-context7** is a [zot](https://github.com/patriceckhart/zot) extension that wraps the
[Context7](https://context7.com) documentation API with a local SQLite cache.
It provides a `/context7` slash command and a `ctx7` LLM-callable tool.

The cache is compatible with the npm `context7-skill` package — both tools
share `~/.context7-cache/docs.db` transparently. No import or conversion needed.

## Installation

```bash
# Build and install from source
cd zot-context7
go build -ldflags="-s -w" -o zot-context7 .
zot ext install ./zot-context7

# Or install directly from the repo (if published)
zot ext install /path/to/zot-context7
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

The agent can call `ctx7` directly with two actions:

**`search`** — find libraries matching a topic:
```json
{"action": "search", "query": "react hooks"}
```

**`docs`** — get documentation for a specific library:
```json
{"action": "docs", "library": "lodash", "query": "deep merge objects"}
```

The tool returns markdown-formatted documentation and code examples.

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
        ├── Fetch from Context7 API (if key configured)
        ├── Store in query_cache + snippets + FTS index
        └── Return formatted markdown
```

- **Cache location:** `~/.context7-cache/docs.db` (shared with npm `context7-skill`)
- **TTL:** Configurable via `config.json` (default 7 days)
- **Full-text search:** FTS5 when available, `LIKE` fallback otherwise
- **Token tracking:** Every request is logged in `token_stats` for hit rate analysis

## Cache compatibility

The database schema is identical to npm `context7-skill` v0.1.2.
Existing `.context7-cache/docs.db` files are read directly — no migration or
import step required. The WAL journal mode allows safe concurrent access when
the npm tool and zot are not running simultaneously.

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
