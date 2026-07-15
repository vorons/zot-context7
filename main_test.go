package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// parseArgs tests
// ---------------------------------------------------------------------------

func TestParseArgs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"docs", []string{"docs"}},
		{"docs lodash merge", []string{"docs", "lodash", "merge"}},
		{"docs \"lodash merge\"", []string{"docs", "lodash merge"}},
		{"  docs  lodash  merge  ", []string{"docs", "lodash", "merge"}},
		{`search "react hooks" --limit 5`, []string{"search", "react hooks", "--limit", "5"}},
	}

	for _, tc := range tests {
		got := parseArgs(tc.input)
		if !stringSliceEqual(got, tc.want) {
			t.Errorf("parseArgs(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// DB schema tests
// ---------------------------------------------------------------------------

func openTestDB(t *testing.T) *dbWrap {
	t.Helper()
	dir := t.TempDir()
	w, err := openDB(dir)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	return w
}

func TestDBCreation(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	// Verify tables exist
	tables := []string{"metadata", "libraries", "snippets", "query_cache", "token_stats"}
	for _, name := range tables {
		var count int
		err := w.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count)
		if err != nil {
			t.Fatalf("check table %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("table %s not found", name)
		}
	}
}

func TestDBSchemaFile(t *testing.T) {
	// Verify the DB file is created at the correct path
	dir := t.TempDir()
	w, err := openDB(dir)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(filepath.Join(dir, "docs.db")); os.IsNotExist(err) {
		t.Error("docs.db was not created")
	}
}

func TestLibraryCRUD(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	// Insert
	now := time.Now().UTC().Format(time.RFC3339)
	dep := "lodash"
	w.upsertLibrary(libRow{
		ID:          "/lodash/lodash",
		Name:        "lodash",
		DepName:     &dep,
		ImportCount: 42,
		LastFetched: &now,
	})

	// Read by dep
	l := w.getLibraryByDep("lodash")
	if l == nil {
		t.Fatal("getLibraryByDep returned nil")
	}
	if l.ID != "/lodash/lodash" {
		t.Errorf("ID = %q, want /lodash/lodash", l.ID)
	}
	if l.ImportCount != 42 {
		t.Errorf("ImportCount = %d, want 42", l.ImportCount)
	}

	// Read by ID
	l2 := w.getLibrary("/lodash/lodash")
	if l2 == nil {
		t.Fatal("getLibrary returned nil")
	}
	if l2.Name != "lodash" {
		t.Errorf("Name = %q, want lodash", l2.Name)
	}

	// List
	libs, err := w.listLibraries()
	if err != nil {
		t.Fatalf("listLibraries: %v", err)
	}
	if len(libs) != 1 {
		t.Errorf("listLibraries count = %d, want 1", len(libs))
	}
}

func TestQueryCache(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	libID := "/test/lib"
	query := "how to use"

	// No cache initially
	cached := w.getCachedQuery(libID, query)
	if cached != nil {
		t.Error("expected nil cache entry")
	}

	// Store
	result := `{"codeSnippets":[],"infoSnippets":[{"breadcrumb":"test","content":"hello","pageId":"","contentTokens":10}]}`
	ttl := 168
	w.storeQueryCache(libID, query, result, ttl)

	// Read back
	cached = w.getCachedQuery(libID, query)
	if cached == nil {
		t.Fatal("expected cache entry after store")
	}
	if cached.ResultJSON != result {
		t.Errorf("ResultJSON = %q, want %q", cached.ResultJSON, result)
	}

	// Freshness
	if !isCacheFresh(cached) {
		t.Error("cache entry should be fresh")
	}

	// Expired
	cached.FetchedAt = time.Now().UTC().Add(-200 * time.Hour).Format(time.RFC3339)
	if isCacheFresh(cached) {
		t.Error("cache entry with past date should not be fresh")
	}
}

func TestSnippets(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	libName := "testlib"
	libID := "/test/lib"
	query := "testing"

	ss := []snippet{
		{Title: "Snippet 1", Content: "Content 1", SourceURL: "url1", Tokens: 10},
		{Title: "Snippet 2", Content: "Content 2", SourceURL: "url2", Tokens: 20},
	}

	w.storeSnippets(libName, libID, query, ss)

	var count int
	w.QueryRow("SELECT COUNT(*) FROM snippets WHERE library_id=?", libID).Scan(&count)
	if count != 2 {
		t.Errorf("snippet count = %d, want 2", count)
	}

	// FTS search (if available)
	if w.hasFTS {
		results, err := w.searchFTS("Content", 10)
		if err != nil {
			t.Fatalf("searchFTS: %v", err)
		}
		if len(results) == 0 {
			t.Error("expected FTS results")
		}
	}
}

func TestTokenStats(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	w.recordTokenStat("/lib/a", "q1", 100, true)  // cache hit
	w.recordTokenStat("/lib/a", "q2", 200, false) // API call
	w.recordTokenStat("/lib/b", "q1", 150, true)  // cache hit
	w.recordTokenStat("/lib/a", "q3", 50, false)  // API call

	stats := w.getTokenStats()
	if stats.TotalTokens != 500 {
		t.Errorf("TotalTokens = %d, want 500", stats.TotalTokens)
	}
	if stats.CacheHits != 2 {
		t.Errorf("CacheHits = %d, want 2", stats.CacheHits)
	}
	if stats.APICalls != 2 {
		t.Errorf("APICalls = %d, want 2", stats.APICalls)
	}
	if stats.HitRate != 0.5 {
		t.Errorf("HitRate = %.2f, want 0.50", stats.HitRate)
	}
}

func TestClearCache(t *testing.T) {
	w := openTestDB(t)
	defer w.Close()

	dep := "test"
	w.upsertLibrary(libRow{ID: "/t/t", Name: "test", DepName: &dep})
	w.storeQueryCache("/t/t", "q", "{}", 168)
	w.recordTokenStat("/t/t", "q", 100, true)

	w.clearCache()

	var count int
	w.QueryRow("SELECT COUNT(*) FROM libraries").Scan(&count)
	if count != 0 {
		t.Errorf("libraries count after clear = %d, want 0", count)
	}
	w.QueryRow("SELECT COUNT(*) FROM query_cache").Scan(&count)
	if count != 0 {
		t.Errorf("query_cache count after clear = %d, want 0", count)
	}
	w.QueryRow("SELECT COUNT(*) FROM token_stats").Scan(&count)
	if count != 0 {
		t.Errorf("token_stats count after clear = %d, want 0", count)
	}
}

// ---------------------------------------------------------------------------
// Config tests
// ---------------------------------------------------------------------------

func TestConfigLoadSave(t *testing.T) {
	dir := t.TempDir()

	// No config file → defaults
	cfg := loadConfig(dir)
	if cfg.CacheTtlHours != 168 {
		t.Errorf("default TTL = %d, want 168", cfg.CacheTtlHours)
	}
	if cfg.DefaultTokenLimit != 5000 {
		t.Errorf("default token limit = %d, want 5000", cfg.DefaultTokenLimit)
	}

	// Save
	cfg.APIKey = "test-key-123"
	cfg.CacheTtlHours = 72
	if err := saveConfig(dir, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	// Load again
	cfg2 := loadConfig(dir)
	if cfg2.APIKey != "test-key-123" {
		t.Errorf("APIKey = %q, want test-key-123", cfg2.APIKey)
	}
	if cfg2.CacheTtlHours != 72 {
		t.Errorf("CacheTtlHours = %d, want 72", cfg2.CacheTtlHours)
	}
}

func TestResolveAPIKey(t *testing.T) {
	// From config
	cfg := &Config{APIKey: "from-config"}
	if key := resolveAPIKey(cfg); key != "from-config" {
		t.Errorf("resolveAPIKey = %q, want from-config", key)
	}

	// From env
	os.Setenv("CONTEXT7_API_KEY", "from-env")
	defer os.Unsetenv("CONTEXT7_API_KEY")
	if key := resolveAPIKey(nil); key != "from-env" {
		t.Errorf("resolveAPIKey(nil) = %q, want from-env", key)
	}

	// Config takes precedence over env
	if key := resolveAPIKey(cfg); key != "from-config" {
		t.Errorf("resolveAPIKey should prefer config, got %q", key)
	}
}

// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestTimeSince(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		input time.Time
		want  string
	}{
		{now, "just now"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-2 * time.Hour), "2h ago"},
		{now.Add(-3 * 24 * time.Hour), "3d ago"},
		{now.Add(-14 * 24 * time.Hour), "2w ago"},
	}

	for _, tc := range tests {
		got := timeSince(tc.input.Format(time.RFC3339))
		if got != tc.want {
			t.Errorf("timeSince(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFmtSize(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
	}
	for _, tc := range tests {
		got := fmtSize(tc.input)
		if got != tc.want {
			t.Errorf("fmtSize(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// JSON format tests (no network)
// ---------------------------------------------------------------------------

func TestFormatDocs(t *testing.T) {
	docs := &docsResponse{
		InfoSnippets: []infoSnippet{
			{Breadcrumb: "Getting Started", Content: "Install via npm."},
		},
		CodeSnippets: []codeSnippet{
			{
				CodeTitle:       "Basic usage",
				CodeDescription: "Import and call.",
				CodeList:        []codeBlock{{Language: "js", Code: "import _ from 'lodash';\n_.map(...)"}},
			},
		},
	}

	result := formatDocs("lodash", docs)
	if !strings.Contains(result, "Getting Started") {
		t.Error("missing breadcrumb")
	}
	if !strings.Contains(result, "Basic usage") {
		t.Error("missing code title")
	}
	if !strings.Contains(result, "_.map") {
		t.Error("missing code content")
	}
	if !strings.Contains(result, "```js") {
		t.Error("missing language tag")
	}
}

func TestFormatDocsEmpty(t *testing.T) {
	docs := &docsResponse{}
	result := formatDocs("empty", docs)
	if !strings.Contains(result, "No documentation") {
		t.Error("expected 'No documentation' for empty docs")
	}
}

func TestFormatSearchResults(t *testing.T) {
	results := []resultRow{
		{LibraryName: "lodash", LibraryID: "/lodash/lodash", Title: "merge", Content: "Deeply merges..."},
	}
	result := formatSearchResults(results, "merge")
	if !strings.Contains(result, "lodash") {
		t.Error("missing library name")
	}
	if !strings.Contains(result, "merge") {
		t.Error("missing title")
	}
	if !strings.Contains(result, "Found **1**") {
		t.Error("wrong count")
	}

	// Empty
	empty := formatSearchResults(nil, "nothing")
	if !strings.Contains(empty, "No results") {
		t.Error("expected 'No results' for empty")
	}
}

// ---------------------------------------------------------------------------
// Wire protocol frame tests
// ---------------------------------------------------------------------------

func TestJSONRoundTrip(t *testing.T) {
	// Verify our Frame types marshal/unmarshal as expected
	f := Frame{
		"type":        "register_tool",
		"name":        "ctx7",
		"description": "test tool",
		"schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"search", "docs"},
				},
			},
			"required": []string{"action"},
		},
	}

	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Frame
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["type"] != "register_tool" {
		t.Errorf("type = %v, want register_tool", decoded["type"])
	}
	if decoded["name"] != "ctx7" {
		t.Errorf("name = %v, want ctx7", decoded["name"])
	}
}

// ---------------------------------------------------------------------------
// Snippets from docs conversion
// ---------------------------------------------------------------------------

func TestSnippetsFromDocs(t *testing.T) {
	docs := &docsResponse{
		CodeSnippets: []codeSnippet{
			{
				CodeTitle:       "Example",
				CodeDescription: "An example",
				CodeID:          "ex1",
				CodeTokens:      50,
				CodeList: []codeBlock{
					{Language: "go", Code: "fmt.Println(\"hi\")"},
				},
			},
		},
		InfoSnippets: []infoSnippet{
			{Breadcrumb: "Intro", Content: "Introduction text", PageID: "p1", ContentTokens: 20},
		},
	}

	ss := snippetsFromDocs("/lib/x", "query", docs)
	if len(ss) != 2 {
		t.Fatalf("got %d snippets, want 2", len(ss))
	}

	// Code snippet
	if ss[0].Title != "Example" {
		t.Errorf("code snippet title = %q, want Example", ss[0].Title)
	}
	if ss[0].Tokens != 50 {
		t.Errorf("code snippet tokens = %d, want 50", ss[0].Tokens)
	}

	// Info snippet
	if ss[1].Title != "Intro" {
		t.Errorf("info snippet title = %q, want Intro", ss[1].Title)
	}
	if ss[1].Tokens != 20 {
		t.Errorf("info snippet tokens = %d, want 20", ss[1].Tokens)
	}
}

// ---------------------------------------------------------------------------
// DB with existing file (simulates npm compatibility)
// ---------------------------------------------------------------------------

func TestExistingDB(t *testing.T) {
	// Create a DB using the same schema (as npm would)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "docs.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Create the libraries table (subset of schema)
	db.Exec("CREATE TABLE IF NOT EXISTS libraries (id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT, total_snippets INTEGER, trust_score INTEGER, benchmark_score REAL, versions TEXT, pinned_version TEXT, source_file TEXT, dep_name TEXT, import_count INTEGER DEFAULT 0, last_fetched TEXT)")
	db.Exec("INSERT INTO libraries (id, name, description, import_count, last_fetched) VALUES ('/existing/lib', 'existing', 'pre-populated', 99, '2024-01-01T00:00:00Z')")
	db.Close()

	// Now open with our wrapper
	w, err := openDB(dir)
	if err != nil {
		t.Fatalf("openDB existing: %v", err)
	}
	defer w.Close()

	// Should find the pre-existing data
	lib := w.getLibrary("/existing/lib")
	if lib == nil {
		t.Fatal("existing library not found")
	}
	if lib.Name != "existing" {
		t.Errorf("Name = %q, want existing", lib.Name)
	}
	if lib.ImportCount != 99 {
		t.Errorf("ImportCount = %d, want 99", lib.ImportCount)
	}
}
