// zot-context7 — zot extension for Context7 documentation cache.
//
// Slash command: /context7 <subcommand> [args...]
// LLM tool:     ctx7 — search or fetch docs
//
// Cache is shared with npm context7-skill: ~/.context7-cache/docs.db
// Config (apiKey, TTL): data_dir/config.json
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Wire protocol
// ---------------------------------------------------------------------------

type Frame map[string]any

func emit(f Frame) { json.NewEncoder(os.Stdout).Encode(f) }

func notify(level, msg string) { emit(Frame{"type": "notify", "level": level, "message": msg}) }

// readFrames returns a channel that delivers decoded frames from stdin.
func readFrames() <-chan Frame {
	ch := make(chan Frame, 4)
	go func() {
		defer close(ch)
		dec := json.NewDecoder(os.Stdin)
		for {
			var f Frame
			if err := dec.Decode(&f); err != nil {
				return
			}
			ch <- f
		}
	}()
	return ch
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type Config struct {
	APIKey            string `json:"apiKey,omitempty"`
	CacheTtlHours     int    `json:"cacheTtlHours,omitempty"`
	DefaultTokenLimit int    `json:"defaultTokenLimit,omitempty"`
}

func defaultConfig() *Config {
	return &Config{
		CacheTtlHours:     168, // 7 days
		DefaultTokenLimit: 5000,
	}
}

func loadConfig(dataDir string) *Config {
	cfg := defaultConfig()
	path := filepath.Join(dataDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, cfg)
	return cfg
}

func saveConfig(dataDir string, cfg *Config) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func resolveAPIKey(cfg *Config) string {
	if cfg != nil && cfg.APIKey != "" {
		return cfg.APIKey
	}
	return os.Getenv("CONTEXT7_API_KEY")
}

// homeCacheDir returns ~/.context7-cache.
func homeCacheDir() string { return filepath.Join(os.Getenv("HOME"), ".context7-cache") }

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

const schemaSQL = `
CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT
);

CREATE TABLE IF NOT EXISTS libraries (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    total_snippets INTEGER,
    trust_score INTEGER,
    benchmark_score REAL,
    versions TEXT,
    pinned_version TEXT,
    source_file TEXT,
    dep_name TEXT,
    import_count INTEGER DEFAULT 0,
    last_fetched TEXT
);

CREATE TABLE IF NOT EXISTS snippets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id TEXT NOT NULL REFERENCES libraries(id),
    title TEXT,
    content TEXT,
    source_url TEXT,
    query TEXT,
    tokens INTEGER,
    fetched_at TEXT
);

CREATE TABLE IF NOT EXISTS query_cache (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id TEXT NOT NULL,
    query TEXT NOT NULL,
    result_json TEXT,
    fetched_at TEXT,
    ttl_hours INTEGER DEFAULT 168,
    UNIQUE(library_id, query)
);

CREATE TABLE IF NOT EXISTS token_stats (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    library_id TEXT,
    query TEXT,
    tokens_served INTEGER,
    was_cache_hit INTEGER,
    timestamp TEXT
);

CREATE INDEX IF NOT EXISTS idx_snippets_library ON snippets(library_id);
CREATE INDEX IF NOT EXISTS idx_snippets_query ON snippets(query);
CREATE INDEX IF NOT EXISTS idx_query_cache_lookup ON query_cache(library_id, query);
CREATE INDEX IF NOT EXISTS idx_token_stats_lib ON token_stats(library_id);
`

const ftsSQL = `CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
	library_name, library_id, title, content, query,
	tokenize='porter unicode61'
)`

type resultRow struct {
	LibraryName string `json:"library_name"`
	LibraryID   string `json:"library_id"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Query       string `json:"query"`
	Rank        float64 `json:"rank,omitempty"`
}

type libRow struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    *string  `json:"description"`
	TotalSnippets  *int     `json:"total_snippets"`
	TrustScore     *int     `json:"trust_score"`
	BenchmarkScore *float64 `json:"benchmark_score"`
	Versions       *string  `json:"versions"`
	PinnedVersion  *string  `json:"pinned_version"`
	SourceFile     *string  `json:"source_file"`
	DepName        *string  `json:"dep_name"`
	ImportCount    int      `json:"import_count"`
	LastFetched    *string  `json:"last_fetched"`
}

type cacheRow struct {
	LibraryID  string `json:"library_id"`
	Query      string `json:"query"`
	ResultJSON string `json:"result_json"`
	FetchedAt  string `json:"fetched_at"`
	TtlHours   int    `json:"ttl_hours"`
}

// dbWrap wraps *sql.DB with helper methods.
type dbWrap struct {
	*sql.DB
	hasFTS bool
}

func openDB(cacheDir string) (*dbWrap, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(cacheDir, "docs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// WAL for safe concurrent reads when npm tool also accesses the same DB.
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA foreign_keys=OFF")

	w := &dbWrap{DB: db}
	w.initSchema()
	return w, nil
}

func (w *dbWrap) initSchema() {
	w.Exec(schemaSQL)

	// FTS5 may not be available in all SQLite builds.
	if _, err := w.Exec(ftsSQL); err == nil {
		w.hasFTS = true
	}
}

// ---------------------------------------------------------------------------
// DB queries
// ---------------------------------------------------------------------------

func (w *dbWrap) listLibraries() ([]libRow, error) {
	rows, err := w.Query("SELECT * FROM libraries ORDER BY import_count DESC, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []libRow
	for rows.Next() {
		var r libRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.TotalSnippets,
			&r.TrustScore, &r.BenchmarkScore, &r.Versions, &r.PinnedVersion,
			&r.SourceFile, &r.DepName, &r.ImportCount, &r.LastFetched); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (w *dbWrap) searchFTS(query string, limit int) ([]resultRow, error) {
	safe := strings.NewReplacer("'", "", "\"", "").Replace(query)
	if safe == "" {
		return nil, nil
	}

	if w.hasFTS {
		rows, err := w.Query(`SELECT library_name, library_id, title, content, query, rank
			FROM docs_fts WHERE docs_fts MATCH ? ORDER BY rank LIMIT ?`, "\""+safe+"\"*", limit)
		if err == nil {
			defer rows.Close()
			var out []resultRow
			for rows.Next() {
				var r resultRow
				if err := rows.Scan(&r.LibraryName, &r.LibraryID, &r.Title, &r.Content, &r.Query, &r.Rank); err != nil {
					return nil, err
				}
				out = append(out, r)
			}
			return out, rows.Err()
		}
		// FTS query failed, fall through to LIKE
	}

	// Fallback: LIKE search
	pat := "%" + safe + "%"
	rows, err := w.Query(`SELECT library_name, library_id, title, content, query, 0.0 as rank
		FROM docs_fts WHERE content LIKE ? OR title LIKE ? OR library_name LIKE ?
		LIMIT ?`, pat, pat, pat, limit)
	if err != nil {
		// FTS table may not exist; search snippets directly
		rows, err = w.Query(`SELECT '' as library_name, s.library_id, s.title, s.content, s.query, 0.0 as rank
			FROM snippets s WHERE s.content LIKE ? OR s.title LIKE ? ORDER BY s.fetched_at DESC LIMIT ?`,
			pat, pat, limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	var out []resultRow
	for rows.Next() {
		var r resultRow
		if err := rows.Scan(&r.LibraryName, &r.LibraryID, &r.Title, &r.Content, &r.Query, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (w *dbWrap) getCachedQuery(libraryID, query string) *cacheRow {
	r := w.QueryRow("SELECT library_id, query, result_json, fetched_at, ttl_hours FROM query_cache WHERE library_id=? AND query=?",
		libraryID, query)
	var c cacheRow
	if err := r.Scan(&c.LibraryID, &c.Query, &c.ResultJSON, &c.FetchedAt, &c.TtlHours); err != nil {
		return nil
	}
	return &c
}

func isCacheFresh(c *cacheRow) bool {
	t, err := time.Parse(time.RFC3339, c.FetchedAt)
	if err != nil {
		return false
	}
	ttl := time.Duration(c.TtlHours) * time.Hour
	return time.Since(t) < ttl
}

func (w *dbWrap) storeQueryCache(libraryID, query, resultJSON string, ttlHours int) {
	now := time.Now().UTC().Format(time.RFC3339)
	w.Exec(`INSERT OR REPLACE INTO query_cache (library_id, query, result_json, fetched_at, ttl_hours)
		VALUES (?, ?, ?, ?, ?)`, libraryID, query, resultJSON, now, ttlHours)
}

func (w *dbWrap) storeSnippets(libraryName, libraryID, query string, snippets []snippet) {
	now := time.Now().UTC().Format(time.RFC3339)

	ins, err := w.Prepare(`INSERT INTO snippets (library_id, title, content, source_url, query, tokens, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return
	}
	defer ins.Close()

	insFTS := func(title, content string) {
		if !w.hasFTS {
			return
		}
		w.Exec("INSERT INTO docs_fts (library_name, library_id, title, content, query) VALUES (?, ?, ?, ?, ?)",
			libraryName, libraryID, title, content, query)
	}

	for _, s := range snippets {
		ins.Exec(libraryID, s.Title, s.Content, s.SourceURL, query, s.Tokens, now)
		insFTS(s.Title, s.Content)
	}
}

func (w *dbWrap) upsertLibrary(lib libRow) {
	w.Exec(`INSERT OR REPLACE INTO libraries (id, name, description, total_snippets, trust_score, benchmark_score, versions, pinned_version, source_file, dep_name, import_count, last_fetched)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lib.ID, lib.Name, lib.Description, lib.TotalSnippets, lib.TrustScore,
		lib.BenchmarkScore, lib.Versions, lib.PinnedVersion, lib.SourceFile,
		lib.DepName, lib.ImportCount, lib.LastFetched)
}

func (w *dbWrap) getLibraryByDep(name string) *libRow {
	r := w.QueryRow("SELECT * FROM libraries WHERE dep_name=? OR name LIKE ?", name, "%"+name+"%")
	var l libRow
	if err := r.Scan(&l.ID, &l.Name, &l.Description, &l.TotalSnippets,
		&l.TrustScore, &l.BenchmarkScore, &l.Versions, &l.PinnedVersion,
		&l.SourceFile, &l.DepName, &l.ImportCount, &l.LastFetched); err != nil {
		return nil
	}
	return &l
}

func (w *dbWrap) getLibrary(id string) *libRow {
	r := w.QueryRow("SELECT * FROM libraries WHERE id=?", id)
	var l libRow
	if err := r.Scan(&l.ID, &l.Name, &l.Description, &l.TotalSnippets,
		&l.TrustScore, &l.BenchmarkScore, &l.Versions, &l.PinnedVersion,
		&l.SourceFile, &l.DepName, &l.ImportCount, &l.LastFetched); err != nil {
		return nil
	}
	return &l
}

func (w *dbWrap) recordTokenStat(libraryID, query string, tokens int, cacheHit bool) {
	hit := 0
	if cacheHit {
		hit = 1
	}
	w.Exec("INSERT INTO token_stats (library_id, query, tokens_served, was_cache_hit, timestamp) VALUES (?, ?, ?, ?, ?)",
		libraryID, query, tokens, hit, time.Now().UTC().Format(time.RFC3339))
}

type tokenStats struct {
	TotalTokens     int     `json:"totalTokens"`
	CachedTokens    int     `json:"cachedTokens"`
	CacheHits       int     `json:"cacheHits"`
	APICalls        int     `json:"apiCalls"`
	HitRate         float64 `json:"hitRate"`
	EstimatedDollars string `json:"estimatedSavings"`
}

func (w *dbWrap) getTokenStats() *tokenStats {
	r := w.QueryRow(`SELECT
		COALESCE(SUM(tokens_served), 0),
		COALESCE(SUM(CASE WHEN was_cache_hit = 1 THEN tokens_served ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN was_cache_hit = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN was_cache_hit = 0 THEN 1 ELSE 0 END), 0)
		FROM token_stats`)

	var total, cachedTokens, hits, misses int
	r.Scan(&total, &cachedTokens, &hits, &misses)

	totalCalls := hits + misses
	var hitRate float64
	if totalCalls > 0 {
		hitRate = float64(hits) / float64(totalCalls)
	}

	// Rough estimate: $3/M input tokens for Claude.
	savedDollars := (float64(cachedTokens) / 1_000_000) * 3

	return &tokenStats{
		TotalTokens:      total,
		CachedTokens:     cachedTokens,
		CacheHits:        hits,
		APICalls:         misses,
		HitRate:          hitRate,
		EstimatedDollars: fmt.Sprintf("$%.4f", savedDollars),
	}
}

func (w *dbWrap) clearCache() {
	w.Exec("DELETE FROM docs_fts")
	w.Exec("DELETE FROM snippets")
	w.Exec("DELETE FROM query_cache")
	w.Exec("DELETE FROM libraries")
	w.Exec("DELETE FROM token_stats")
}

func (w *dbWrap) dbSize() string {
	var size int64
	w.QueryRow("SELECT IFNULL(SUM(pgsize), 0) FROM dbstat").Scan(&size)
	if size == 0 {
		// fallback: use file size
		var p string
		w.QueryRow("PRAGMA database_list").Scan(nil, nil, &p)
		if fi, err := os.Stat(p); err == nil {
			size = fi.Size()
		}
	}
	return fmtSize(size)
}

func fmtSize(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	}
}

// ---------------------------------------------------------------------------
// Context7 API types
// ---------------------------------------------------------------------------

type apiLibResult struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	TotalSnippets  int      `json:"totalSnippets"`
	TrustScore     int      `json:"trustScore"`
	BenchmarkScore float64  `json:"benchmarkScore"`
	Versions       []string `json:"versions"`
}

type searchResponse struct {
	Results []apiLibResult `json:"results"`
}

type codeBlock struct {
	Code     string `json:"code"`
	Language string `json:"language"`
}

type codeSnippet struct {
	CodeTitle       string      `json:"codeTitle"`
	CodeDescription string      `json:"codeDescription"`
	CodeID          string      `json:"codeId"`
	CodeTokens      int         `json:"codeTokens"`
	CodeList        []codeBlock `json:"codeList"`
}

type infoSnippet struct {
	Breadcrumb   string `json:"breadcrumb"`
	Content      string `json:"content"`
	PageID       string `json:"pageId"`
	ContentTokens int    `json:"contentTokens"`
}

type docsResponse struct {
	CodeSnippets []codeSnippet `json:"codeSnippets"`
	InfoSnippets []infoSnippet `json:"infoSnippets"`
}

type snippet struct {
	Title     string
	Content   string
	SourceURL string
	Tokens    int
}

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

const apiBase = "https://context7.com/api"

type apiClient struct {
	apiKey string
	http   *http.Client
}

func newAPIClient(apiKey string) *apiClient {
	return &apiClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *apiClient) get(path string) ([]byte, error) {
	u := apiBase + path
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (c *apiClient) searchLibrary(name, query string) ([]apiLibResult, error) {
	q := url.Values{}
	q.Set("libraryName", name)
	q.Set("query", query)

	body, err := c.get("/v2/libs/search?" + q.Encode())
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Results == nil {
		return nil, nil
	}
	return resp.Results, nil
}

func (c *apiClient) fetchDocs(libraryID, query string, tokens int) (*docsResponse, error) {
	q := url.Values{}
	q.Set("libraryId", libraryID)
	q.Set("query", query)
	q.Set("type", "json")
	if tokens > 0 {
		q.Set("tokens", fmt.Sprintf("%d", tokens))
	}

	body, err := c.get("/v2/context?" + q.Encode())
	if err != nil {
		return nil, err
	}

	var resp docsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *apiClient) ping() error {
	_, err := c.get("/v2/libs/search?libraryName=react&query=test")
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveLibraryID resolves a library name to (id, name).
// Order: local DB → API search.
func resolveLibraryID(db *dbWrap, api *apiClient, name string) (string, string, error) {
	if strings.HasPrefix(name, "/") {
		return name, name[strings.LastIndex(name, "/")+1:], nil
	}

	if db != nil {
		if l := db.getLibraryByDep(name); l != nil {
			return l.ID, l.Name, nil
		}
	}

	if api != nil {
		results, err := api.searchLibrary(name, "")
		if err == nil && len(results) > 0 {
			return results[0].ID, results[0].Title, nil
		}
	}

	return "", "", fmt.Errorf("library %q not found", name)
}

// snippetsFromDocs converts API docs into snippet records for caching.
func snippetsFromDocs(libraryID, query string, docs *docsResponse) []snippet {
	var out []snippet
	for _, cs := range docs.CodeSnippets {
		var codeParts []string
		for _, cb := range cs.CodeList {
			codeParts = append(codeParts, fmt.Sprintf("```%s\n%s\n```", cb.Language, cb.Code))
		}
		content := cs.CodeDescription
		if len(codeParts) > 0 {
			content += "\n\n" + strings.Join(codeParts, "\n")
		}
		out = append(out, snippet{
			Title:     cs.CodeTitle,
			Content:   content,
			SourceURL: cs.CodeID,
			Tokens:    cs.CodeTokens,
		})
	}
	for _, is := range docs.InfoSnippets {
		out = append(out, snippet{
			Title:     is.Breadcrumb,
			Content:   is.Content,
			SourceURL: is.PageID,
			Tokens:    is.ContentTokens,
		})
	}
	return out
}

// formatDocs formats docsResponse as a markdown string.
func formatDocs(libraryName string, docs *docsResponse) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Documentation for %s\n\n", libraryName))

	for _, is := range docs.InfoSnippets {
		if is.Breadcrumb != "" {
			b.WriteString(fmt.Sprintf("### %s\n", is.Breadcrumb))
		}
		b.WriteString(is.Content + "\n\n")
	}
	if len(docs.CodeSnippets) > 0 {
		b.WriteString("### Code Examples\n\n")
		for _, cs := range docs.CodeSnippets {
			b.WriteString(fmt.Sprintf("**%s**\n", cs.CodeTitle))
			if cs.CodeDescription != "" {
				b.WriteString(cs.CodeDescription + "\n")
			}
			for _, cb := range cs.CodeList {
				b.WriteString(fmt.Sprintf("```%s\n%s\n```\n", cb.Language, cb.Code))
			}
			b.WriteString("\n")
		}
	}
	if len(docs.CodeSnippets) == 0 && len(docs.InfoSnippets) == 0 {
		b.WriteString("No documentation found for this query.\n")
	}
	return b.String()
}

// formatSearchResults formats FTS results as markdown.
func formatSearchResults(results []resultRow, query string) string {
	if len(results) == 0 {
		return "No results found."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Found **%d** results for \"%s\":\n\n", len(results), query))
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		b.WriteString(fmt.Sprintf("- **[%s](%s)** %s\n", r.LibraryName, r.LibraryID, title))
		preview := strings.ReplaceAll(r.Content, "\n", " ")
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		b.WriteString(fmt.Sprintf("  %s\n", preview))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Command handler
// ---------------------------------------------------------------------------

type commandCtx struct {
	id      string
	db      *dbWrap
	api     *apiClient
	cfg     *Config
	cacheDir string
	jsonOut bool
}

func handleCommand(ctx *commandCtx, argv []string) {
	if len(argv) == 0 {
		ctx.respond(usage())
		return
	}

	cmd := argv[0]
	args := argv[1:]

	switch cmd {
	case "libs":
		handleLibs(ctx, args)
	case "search":
		handleSearch(ctx, args)
	case "docs":
		handleDocs(ctx, args)
	case "cache":
		handleCache(ctx, args)
	case "doctor":
		handleDoctor(ctx)
	default:
		ctx.respond("Unknown subcommand: " + cmd + "\n\n" + usage())
	}
}

func usage() string {
	return `Usage: /context7 <subcommand> [args...]

Subcommands:
  docs <library> <query>   Get documentation (cache-first, API fallback)
    --no-cache             Force fresh API fetch
    --json                 JSON output
    --tokens N             Max tokens

  search <query>           Full-text search over cached docs
    --json                 JSON output
    --limit N              Max results (default 20)

  libs                     List cached libraries
    --json                 JSON output

  cache stats              Cache statistics
  cache clear              Wipe local cache

  doctor                   Health check

  (no args)                This help`
}

// ---------------------------------------------------------------------------
// Subcommand: libs
// ---------------------------------------------------------------------------

func handleLibs(ctx *commandCtx, args []string) {
	// Parse --json
	jsonOut := ctx.jsonOut
	remain := args[:0]
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		default:
			remain = append(remain, a)
		}
	}
	_ = remain

	if ctx.db == nil {
		ctx.respond("No cache found. Use `docs` to populate the cache.")
		return
	}

	libs, err := ctx.db.listLibraries()
	if err != nil {
		ctx.respond("Error listing libraries: " + err.Error())
		return
	}
	if len(libs) == 0 {
		ctx.respond("No libraries cached. Use `docs` to fetch documentation.")
		return
	}

	if jsonOut {
		data, _ := json.MarshalIndent(libs, "", "  ")
		ctx.respond(string(data))
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**Cached Libraries (%d)**\n\n", len(libs)))
	b.WriteString("| Name | ID | Imports | Score | Last Fetched |\n")
	b.WriteString("|------|----|---------|-------|-------------|\n")
	for _, l := range libs {
		fetched := "never"
		if l.LastFetched != nil {
			fetched = timeSince(*l.LastFetched)
		}
		name := l.Name
		if l.PinnedVersion != nil && *l.PinnedVersion != "" {
			name += "@" + *l.PinnedVersion
		}
		id := l.ID
		if len(id) > 30 {
			id = id[:30]
		}
		score := 0.0
		if l.BenchmarkScore != nil {
			score = *l.BenchmarkScore
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %d | %.1f | %s |\n",
			name, id, l.ImportCount, score, fetched))
	}
	ctx.respond(b.String())
}

// ---------------------------------------------------------------------------
// Subcommand: search
// ---------------------------------------------------------------------------

func handleSearch(ctx *commandCtx, args []string) {
	limit := 20
	jsonOut := ctx.jsonOut
	var queryParts []string

	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "--limit="):
			fmt.Sscanf(a, "--limit=%d", &limit)
		case a == "--limit" && len(queryParts) == 0:
			// limit as next arg — handled by iterating twice,
			// but we use single pass; we'll handle this differently
		default:
			queryParts = append(queryParts, a)
		}
	}

	// Handle --limit N as next arg
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--limit" {
			fmt.Sscanf(args[i+1], "%d", &limit)
			break
		}
	}

	query := strings.Join(queryParts, " ")
	if query == "" {
		ctx.respond("Usage: `/context7 search <query>`")
		return
	}

	if ctx.db == nil {
		ctx.respond("No cache found. Use `docs` to populate the cache first.")
		return
	}

	results, err := ctx.db.searchFTS(query, limit)
	if err != nil {
		ctx.respond("Search error: " + err.Error())
		return
	}

	if jsonOut {
		data, _ := json.MarshalIndent(results, "", "  ")
		ctx.respond(string(data))
		return
	}

	ctx.respond(formatSearchResults(results, query))
}

// ---------------------------------------------------------------------------
// Subcommand: docs
// ---------------------------------------------------------------------------

func handleDocs(ctx *commandCtx, args []string) {
	noCache := false
	jsonOut := ctx.jsonOut
	tokens := 0
	var posArgs []string

	for _, a := range args {
		switch {
		case a == "--no-cache":
			noCache = true
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "--tokens="):
			fmt.Sscanf(a, "--tokens=%d", &tokens)
		default:
			posArgs = append(posArgs, a)
		}
	}

	// Handle --tokens N as separate args
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--tokens" {
			fmt.Sscanf(args[i+1], "%d", &tokens)
			break
		}
	}

	if len(posArgs) < 2 {
		ctx.respond("Usage: `/context7 docs <library> <query>`")
		return
	}

	library := posArgs[0]
	query := strings.Join(posArgs[1:], " ")

	// Resolve library ID
	libID, libName, err := resolveLibraryID(ctx.db, ctx.api, library)
	if err != nil {
		ctx.respond(fmt.Sprintf("Error: %v", err))
		return
	}

	// ponytail: TTL from config if available
	ttlHours := 168
	if ctx.cfg != nil && ctx.cfg.CacheTtlHours > 0 {
		ttlHours = ctx.cfg.CacheTtlHours
	}

	// Check cache (unless --no-cache)
	if !noCache && ctx.db != nil {
		if cached := ctx.db.getCachedQuery(libID, query); cached != nil && isCacheFresh(cached) {
			var docs docsResponse
			if err := json.Unmarshal([]byte(cached.ResultJSON), &docs); err == nil {
				tokenCount := len(cached.ResultJSON)
				ctx.db.recordTokenStat(libID, query, tokenCount, true)
				if jsonOut {
					ctx.respondJSON(docs)
				} else {
					ctx.respond(formatDocs(libName, &docs))
				}
				return
			}
		}
	}

	// Fetch from API
	if ctx.api == nil {
		// Offline: try to serve any cached content
		if ctx.db != nil {
			rows, err := ctx.db.Query("SELECT title, content FROM snippets WHERE library_id=? LIMIT 20", libID)
			if err == nil {
				var snippets []snippet
				for rows.Next() {
					var s snippet
					if rows.Scan(&s.Title, &s.Content) == nil {
						snippets = append(snippets, s)
					}
				}
				rows.Close()
				if len(snippets) > 0 {
					var b strings.Builder
					b.WriteString(fmt.Sprintf("**[OFFLINE] Cached docs for %s**\n\n", libName))
					for _, s := range snippets {
						b.WriteString(fmt.Sprintf("### %s\n%s\n\n", s.Title, s.Content))
					}
					ctx.respond(b.String())
					return
				}
			}
		}
		ctx.respond("API not available and no cached docs found.")
		return
	}

	docs, err := ctx.api.fetchDocs(libID, query, tokens)
	if err != nil {
		ctx.respond("API error: " + err.Error())
		return
	}

	// Cache the result
	if ctx.db != nil {
		resultJSON, _ := json.Marshal(docs)
		ctx.db.storeQueryCache(libID, query, string(resultJSON), ttlHours)

		ss := snippetsFromDocs(libID, query, docs)
		ctx.db.storeSnippets(libName, libID, query, ss)

		if ctx.db.getLibrary(libID) == nil {
			ctx.db.upsertLibrary(libRow{
				ID:            libID,
				Name:          libName,
				TotalSnippets: intPtr(len(ss)),
				DepName:       &library,
				ImportCount:   0,
				LastFetched:   strPtr(time.Now().UTC().Format(time.RFC3339)),
			})
		}

		tokenCount := len(resultJSON)
		ctx.db.recordTokenStat(libID, query, tokenCount, false)
	}

	if jsonOut {
		ctx.respondJSON(docs)
	} else {
		ctx.respond(formatDocs(libName, docs))
	}
}

// ---------------------------------------------------------------------------
// Subcommand: cache
// ---------------------------------------------------------------------------

func handleCache(ctx *commandCtx, args []string) {
	if len(args) == 0 {
		ctx.respond("Usage: `/context7 cache stats` or `/context7 cache clear`")
		return
	}

	switch args[0] {
	case "stats":
		handleCacheStats(ctx)
	case "clear":
		handleCacheClear(ctx)
	default:
		ctx.respond("Unknown cache subcommand: " + args[0])
	}
}

func handleCacheStats(ctx *commandCtx) {
	if ctx.db == nil {
		ctx.respond("No cache found.")
		return
	}

	stats := ctx.db.getTokenStats()
	size := ctx.db.dbSize()

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**Cache Statistics**\n\n"))
	b.WriteString(fmt.Sprintf("- **DB size:** %s\n", size))
	b.WriteString(fmt.Sprintf("- **Total tokens served:** %d\n", stats.TotalTokens))
	b.WriteString(fmt.Sprintf("- **Cache hits:** %d\n", stats.CacheHits))
	b.WriteString(fmt.Sprintf("- **API calls:** %d\n", stats.APICalls))
	b.WriteString(fmt.Sprintf("- **Cached tokens saved:** %d\n", stats.CachedTokens))
	b.WriteString(fmt.Sprintf("- **Hit rate:** %.1f%%\n", stats.HitRate*100))
	b.WriteString(fmt.Sprintf("- **Estimated savings:** %s\n", stats.EstimatedDollars))
	ctx.respond(b.String())
}

func handleCacheClear(ctx *commandCtx) {
	if ctx.db == nil {
		ctx.respond("No cache to clear.")
		return
	}
	ctx.db.clearCache()
	ctx.respond("Cache cleared.")
}

// ---------------------------------------------------------------------------
// Subcommand: doctor
// ---------------------------------------------------------------------------

func handleDoctor(ctx *commandCtx) {
	var b strings.Builder

	b.WriteString("**zot-context7 health check**\n\n")

	// 1. DB
	if ctx.db != nil {
		b.WriteString("✅ **Database:** `" + filepath.Join(ctx.cacheDir, "docs.db") + "`\n")
		libs, _ := ctx.db.listLibraries()
		b.WriteString(fmt.Sprintf("   Libraries cached: %d\n", len(libs)))
	} else {
		b.WriteString("⚠️ **Database:** not initialized\n")
		b.WriteString("   Run `/context7 docs <library> <query>` to create the cache.\n")
	}

	// 2. API auth
	if ctx.api != nil {
		if ctx.api.apiKey != "" {
			b.WriteString("✅ **API key:** configured\n")
		} else {
			b.WriteString("⚠️ **API key:** not set (unauthenticated, rate-limited)\n")
		}
	} else {
		b.WriteString("❌ **API:** not available\n")
	}

	// 3. API reachability
	if ctx.api != nil {
		if err := ctx.api.ping(); err != nil {
			b.WriteString(fmt.Sprintf("❌ **API reachability:** %v\n", err))
		} else {
			b.WriteString("✅ **API reachability:** OK\n")
		}
	}

	// 4. Config
	if ctx.cfg != nil {
		b.WriteString(fmt.Sprintf("✅ **Config:** TTL=%dh, tokenLimit=%d\n",
			ctx.cfg.CacheTtlHours, ctx.cfg.DefaultTokenLimit))
	}

	// 5. FTS
	if ctx.db != nil {
		if ctx.db.hasFTS {
			b.WriteString("✅ **FTS5:** available\n")
		} else {
			b.WriteString("⚠️ **FTS5:** unavailable (using LIKE fallback)\n")
		}
	}

	ctx.respond(b.String())
}

// ---------------------------------------------------------------------------
// Tool handler
// ---------------------------------------------------------------------------

func handleTool(ctx *commandCtx, toolID string, args map[string]any) {
	action, _ := args["action"].(string)
	query, _ := args["query"].(string)
	library, _ := args["library"].(string)

	if query == "" {
		ctx.toolResult(toolID, "Query is required.", true)
		return
	}

	switch action {
	case "search":
		handleToolSearch(ctx, toolID, query)
	case "docs":
		if library == "" {
			ctx.toolResult(toolID, "Library is required for docs action.", true)
			return
		}
		handleToolDocs(ctx, toolID, library, query)
	default:
		ctx.toolResult(toolID, "Unknown action: "+action+". Use 'search' or 'docs'.", true)
	}
}

func handleToolSearch(ctx *commandCtx, toolID, query string) {
	if ctx.db == nil {
		ctx.toolResult(toolID, "No cache available. Use `docs` action first to populate the cache.", true)
		return
	}
	results, err := ctx.db.searchFTS(query, 20)
	if err != nil {
		ctx.toolResult(toolID, "Search error: "+err.Error(), true)
		return
	}
	ctx.toolResult(toolID, formatSearchResults(results, query), false)
}

func handleToolDocs(ctx *commandCtx, toolID, library, query string) {
	noCache := false
	tokens := 0
	if ctx.cfg != nil {
		tokens = ctx.cfg.DefaultTokenLimit
	}

	// Resolve library ID
	libID, libName, err := resolveLibraryID(ctx.db, ctx.api, library)
	if err != nil {
		ctx.toolResult(toolID, fmt.Sprintf("Error: %v", err), true)
		return
	}

	ttlHours := 168
	if ctx.cfg != nil && ctx.cfg.CacheTtlHours > 0 {
		ttlHours = ctx.cfg.CacheTtlHours
	}

	// Check cache
	if !noCache && ctx.db != nil {
		if cached := ctx.db.getCachedQuery(libID, query); cached != nil && isCacheFresh(cached) {
			var docs docsResponse
			if err := json.Unmarshal([]byte(cached.ResultJSON), &docs); err == nil {
				ctx.db.recordTokenStat(libID, query, len(cached.ResultJSON), true)
				ctx.toolResult(toolID, formatDocs(libName, &docs), false)
				return
			}
		}
	}

	// Fetch from API
	if ctx.api == nil {
		if ctx.db != nil {
			rows, err := ctx.db.Query("SELECT title, content FROM snippets WHERE library_id=? LIMIT 20", libID)
			if err == nil {
				var snippets []snippet
				for rows.Next() {
					var s snippet
					if rows.Scan(&s.Title, &s.Content) == nil {
						snippets = append(snippets, s)
					}
				}
				rows.Close()
				if len(snippets) > 0 {
					var b strings.Builder
					b.WriteString(fmt.Sprintf("**[OFFLINE] Cached docs for %s**\n\n", libName))
					for _, s := range snippets {
						b.WriteString(fmt.Sprintf("### %s\n%s\n\n", s.Title, s.Content))
					}
					ctx.toolResult(toolID, b.String(), false)
					return
				}
			}
		}
		ctx.toolResult(toolID, "API not available and no cached docs found.", true)
		return
	}

	docs, err := ctx.api.fetchDocs(libID, query, tokens)
	if err != nil {
		ctx.toolResult(toolID, "API error: "+err.Error(), true)
		return
	}

	// Cache
	if ctx.db != nil {
		resultJSON, _ := json.Marshal(docs)
		ctx.db.storeQueryCache(libID, query, string(resultJSON), ttlHours)
		ss := snippetsFromDocs(libID, query, docs)
		ctx.db.storeSnippets(libName, libID, query, ss)
		if ctx.db.getLibrary(libID) == nil {
			ctx.db.upsertLibrary(libRow{
				ID:          libID,
				Name:        libName,
				DepName:     &library,
				LastFetched: strPtr(time.Now().UTC().Format(time.RFC3339)),
			})
		}
		ctx.db.recordTokenStat(libID, query, len(resultJSON), false)
	}

	ctx.toolResult(toolID, formatDocs(libName, docs), false)
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func (c *commandCtx) respond(text string) {
	emit(Frame{
		"type":    "command_response",
		"id":      c.id,
		"action":  "display",
		"display": strings.TrimSpace(text),
	})
}

func (c *commandCtx) respondJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	c.respond("```json\n" + string(data) + "\n```")
}

func (c *commandCtx) toolResult(id, text string, isErr bool) {
	emit(Frame{
		"type":     "tool_result",
		"id":       id,
		"is_error": isErr,
		"content":  []Frame{{"type": "text", "text": text}},
	})
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func timeSince(isoDate string) string {
	t, err := time.Parse(time.RFC3339, isoDate)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	}
}

func intPtr(n int) *int { return &n }
func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	emit(Frame{
		"type":         "hello",
		"name":         "zot-context7",
		"version":      "1.0.0",
		"capabilities": []string{"commands", "tools"},
	})

	var (
		db       *dbWrap
		api      *apiClient
		cfg      *Config
		cacheDir string
		dataDir  string
		ready    bool
	)

	frames := readFrames()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case <-sigCh:
			emit(Frame{"type": "shutdown_ack"})
			return
		case msg, ok := <-frames:
			if !ok {
				return
			}

			switch msg["type"] {

			// ---- handshake ----
			case "hello_ack":
				dataDir, _ = msg["data_dir"].(string)
				if dataDir == "" {
					dataDir = filepath.Join(os.Getenv("HOME"), ".local", "state", "zot-context7")
				}

				// Load config from extension's data dir
				cfg = loadConfig(dataDir)

				// API client (key is optional — unauthenticated requests work with rate limits)
				api = newAPIClient(resolveAPIKey(cfg))

				// Cache directory: ~/.context7-cache (shared with npm context7-skill)
				cacheDir = homeCacheDir()

				// Open or create DB
				d, err := openDB(cacheDir)
				if err != nil {
					notify("error", "DB init: "+err.Error())
				} else {
					db = d
				}

				// Register slash command
				emit(Frame{
					"type":        "register_command",
					"name":        "context7",
					"description": "Search Context7 documentation cache — libs, search, docs, cache, doctor",
				})

				// Register LLM tool
				emit(Frame{
					"type":        "register_tool",
					"name":        "ctx7",
					"description": "Fetch up-to-date docs and code examples for any library or framework. Call before implementing anything involving a third-party dependency.",
					"schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action": map[string]any{
								"type":        "string",
								"enum":        []string{"search", "docs"},
								"description": "Find libraries matching a topic or task. Returns names and IDs — use the ID with 'docs' action next.",
							},
							"library": map[string]any{
								"type":        "string",
								"description": "Library name or Context7 ID (e.g. 'react', '/facebook/react'). Required for 'docs'.",
							},
							"query": map[string]any{
								"type":        "string",
								"description": "Specific topic or question — narrow queries return better results.",
							},
						},
						"required": []string{"action", "query"},
					},
				})

				emit(Frame{"type": "ready"})
				ready = true

			// ---- /context7 slash command ----
			case "command_invoked":
				id, _ := msg["id"].(string)
				args, _ := msg["args"].(string)

				// Save config if apiKey was passed
				_ = saveConfig // keep saveConfig accessible

				ctx := &commandCtx{
					id:       id,
					db:       db,
					api:      api,
					cfg:      cfg,
					cacheDir: cacheDir,
				}

				argv := parseArgs(args)
				handleCommand(ctx, argv)

			// ---- ctx7 LLM tool ----
			case "tool_call":
				name, _ := msg["name"].(string)
				if name != "ctx7" {
					continue
				}
				toolID, _ := msg["id"].(string)
				argsMap, _ := msg["args"].(map[string]any)

				ctx := &commandCtx{
					id:       toolID,
					db:       db,
					api:      api,
					cfg:      cfg,
					cacheDir: cacheDir,
				}
				handleTool(ctx, toolID, argsMap)

			// ---- shutdown ----
			case "shutdown":
				if db != nil {
					db.Close()
				}
				emit(Frame{"type": "shutdown_ack"})
				return
			}

			// Delay DB close if we need it for further commands
			_ = ready
		}
	}
}

// parseArgs splits a command string into tokens, respecting double-quoted strings.
func parseArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	var args []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
