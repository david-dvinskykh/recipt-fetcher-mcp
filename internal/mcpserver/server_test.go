package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/david-dvinskykh/recipt-fetcher-mcp/internal/config"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(config.Config{StateDir: t.TempDir(), UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// connect wires the MCP server to an in-memory client, the way a real client
// would speak to it over stdio.
func connect(t *testing.T, server *Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.MCPServer().Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestToolsAreAdvertised(t *testing.T) {
	session := connect(t, testServer(t))

	found := map[string]bool{}
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		found[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
	}
	for _, want := range []string{
		"receipts_providers", "receipts_login", "receipts_logout",
		"receipts_list", "receipts_get", "receipts_export", "receipts_mail_probe",
	} {
		if !found[want] {
			t.Errorf("tool %q is not advertised", want)
		}
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func decodeResult(t *testing.T, result *mcp.CallToolResult, out any) {
	t.Helper()
	if result.StructuredContent == nil {
		t.Fatalf("no structured content: %+v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatal(err)
	}
}

func TestProvidersReportsEveryStore(t *testing.T) {
	session := connect(t, testServer(t))

	var out providersOutput
	decodeResult(t, callTool(t, session, "receipts_providers", map[string]any{}), &out)

	ids := map[string]bool{}
	for _, status := range out.Providers {
		ids[status.Provider] = true
		if status.LoggedIn {
			t.Errorf("provider %q claims to be logged in on a fresh state dir", status.Provider)
		}
	}
	for _, want := range []string{"lidl", "action", "allegro"} {
		if !ids[want] {
			t.Errorf("provider %q missing from the status", want)
		}
	}
	if out.Mailbox.Configured {
		t.Error("the mailbox should start unconfigured")
	}
	if len(out.Mailbox.Stores) == 0 {
		t.Error("the stores covered by e-mail rules should be listed")
	}
}

func TestListWithoutCredentialsIsAnError(t *testing.T) {
	session := connect(t, testServer(t))

	result := callTool(t, session, "receipts_list", map[string]any{"from": "2026-01-01"})
	if !result.IsError {
		t.Fatal("listing with no credentials at all should be an error, not a silent empty list")
	}
	text := resultText(result)
	if !strings.Contains(text, "lidl") {
		t.Errorf("the error should name the stores that could not be queried: %s", text)
	}
}

func resultText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func TestLoginRejectsUnknownProvider(t *testing.T) {
	session := connect(t, testServer(t))

	result := callTool(t, session, "receipts_login", map[string]any{
		"provider": "tesco",
		"fields":   map[string]string{"token": "x"},
	})
	if !result.IsError {
		t.Fatal("an unknown provider must be rejected")
	}
}

func TestMailLoginAndLogoutDoNotLeakSecrets(t *testing.T) {
	server := testServer(t)
	session := connect(t, server)

	// No IMAP server here, so the login fails; what matters is that the failure
	// is reported and no password is echoed back.
	result := callTool(t, session, "receipts_login", map[string]any{
		"provider": "mail",
		"fields": map[string]string{
			"host":     "127.0.0.1",
			"port":     "1",
			"username": "someone@example.org",
			"password": "hunter2-do-not-leak",
		},
	})
	if !result.IsError {
		t.Fatal("connecting to a dead IMAP port should fail")
	}
	if strings.Contains(resultText(result), "hunter2") {
		t.Fatal("the password was echoed back to the client")
	}
}

func TestBuildQuery(t *testing.T) {
	q, err := buildQuery("2026-01-01", "2026-01-31", 10)
	if err != nil {
		t.Fatal(err)
	}
	if q.From.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("from = %s", q.From)
	}
	// A plain end date must cover the whole day, or the last day is lost.
	if q.To.Format("2006-01-02 15:04") != "2026-01-31 23:59" {
		t.Errorf("to = %s, want the end of the day", q.To)
	}
	if q.Limit != 10 {
		t.Errorf("limit = %d", q.Limit)
	}

	if _, err := buildQuery("2026-02-01", "2026-01-01", 0); err == nil {
		t.Error("an inverted range must be rejected")
	}
	if _, err := buildQuery("yesterday", "", 0); err == nil {
		t.Error("an unparsable date must be rejected")
	}

	full, err := buildQuery("2026-01-01T10:00:00Z", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !full.From.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 from = %s", full.From)
	}
}

func TestNormalizeIDs(t *testing.T) {
	got := normalizeIDs([]string{" Lidl ", "", "ALLEGRO"})
	if len(got) != 2 || got[0] != "lidl" || got[1] != "allegro" {
		t.Fatalf("normalizeIDs = %v", got)
	}
}
