package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadImportBatchesByCountAndCommandSize(t *testing.T) {
	items := make([]map[string]any, 201)
	for i := range items {
		items[i] = map[string]any{"asset_name": fmt.Sprintf("Asset %d", i+1)}
	}
	path := writeImportFile(t, items)
	batches, total, err := readImportBatches(path)
	if err != nil || total != 201 || len(batches) != 3 {
		t.Fatalf("unexpected batches: total=%d batches=%d err=%v", total, len(batches), err)
	}
	for _, batch := range batches {
		if len(batch) > maxCommandInput {
			t.Fatalf("batch exceeds API input limit: %d", len(batch))
		}
		var payload struct {
			Items []map[string]any `json:"items"`
		}
		if json.Unmarshal(batch, &payload) != nil || len(payload.Items) > importBatchSize {
			t.Fatalf("invalid batch: %s", batch)
		}
	}
}

func TestReadImportPreservesLargeIntegerIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ids.json")
	if err := os.WriteFile(path, []byte(`[{"employee_role_id":9007199254740993}]`), 0600); err != nil {
		t.Fatal(err)
	}
	batches, _, err := readImportBatches(path)
	if err != nil || !strings.Contains(string(batches[0]), `9007199254740993`) {
		t.Fatalf("large integer changed during import: %s err=%v", batches[0], err)
	}
}

func TestReadImportBatchesRejectsUnsafeOrInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	invalid := map[string]string{
		"empty.json":     `[]`,
		"object.json":    `{}`,
		"null.json":      `[null]`,
		"primitive.json": `["asset"]`,
		"oversized.json": `[{"asset_name":"` + strings.Repeat("x", maxCommandInput) + `"}]`,
	}
	for name, body := range invalid {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readImportBatches(path); err == nil {
			t.Errorf("accepted invalid import %s", name)
		}
	}
	if _, _, err := readImportBatches(dir); err == nil {
		t.Fatal("accepted a directory as an import file")
	}
}

func TestImportValidatesEveryBatchBeforePreparing(t *testing.T) {
	var dryRuns, preparations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("name") != "assets.bulk_create" {
			t.Fatal("wrong bulk command request")
		}
		if r.Form.Get("dry_run") == "true" {
			dryRuns++
			json.NewEncoder(w).Encode(commandResult{OK: true, Data: map[string]any{"resources": []any{"large preview"}}, Summary: "Validated", DryRun: true})
		} else {
			preparations++
			writeCommandResult(w, map[string]any{
				"confirmation_token":      fmt.Sprintf("token-%d", preparations),
				"confirmation_expires_in": 600, "command_name": "assets.bulk_create", "risk": "write",
				"input": map[string]any{"items": []any{"large input"}},
			})
		}
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	batches := [][]byte{[]byte(`{"items":[{"asset_name":"One"}]}`), []byte(`{"items":[{"asset_name":"Two"}]}`)}
	results, err := c.importResources(context.Background(), "assets", batches, false)
	if err != nil || dryRuns != 2 || preparations != 2 || len(results) != 2 {
		t.Fatalf("unexpected import: dry=%d prepare=%d results=%d err=%v", dryRuns, preparations, len(results), err)
	}
	encoded, _ := json.Marshal(results)
	if strings.Contains(string(encoded), "large preview") || strings.Contains(string(encoded), "large input") ||
		results[0].Count != 1 || results[0].Index != 1 || results[0].ConfirmationToken != "token-1" ||
		results[0].ConfirmationExpiresIn != 600 || results[0].Command != "assets.bulk_create" || results[0].Risk != "write" {
		t.Fatalf("import output was not compact or lost confirmation data: %s", encoded)
	}
}

func TestDryRunImportOutputIsCompactAndMarked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(commandResult{
			OK: true, Data: map[string]any{"resources": []any{"large preview"}},
			Summary: "Would create one asset.", DryRun: true,
		})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	results, err := c.importResources(context.Background(), "assets", [][]byte{[]byte(`{"items":[{"asset_name":"One"}]}`)}, true)
	if err != nil || len(results) != 1 || !results[0].DryRun || results[0].Count != 1 {
		t.Fatalf("dry-run marker or batch count missing: %#v err=%v", results, err)
	}
	encoded, _ := json.Marshal(results)
	if strings.Contains(string(encoded), "large preview") || strings.Contains(string(encoded), "confirmation_token") {
		t.Fatalf("dry-run output was not compact: %s", encoded)
	}
}

func TestImportRejectsPreparedBatchesAfterLaterFailure(t *testing.T) {
	preparations, rejects := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/api/v1/commands/reject" {
			rejects++
			json.NewEncoder(w).Encode(commandResult{OK: true, Data: map[string]any{"rejected": true}})
			return
		}
		if r.Form.Get("dry_run") == "true" {
			writeCommandResult(w, map[string]any{"validated": true})
			return
		}
		preparations++
		if preparations == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeCommandResult(w, map[string]any{"confirmation_token": "prepared-token"})
	}))
	defer server.Close()
	c, _ := newClient(server.URL, "secret")
	batches := [][]byte{[]byte(`{"items":[{"asset_name":"One"}]}`), []byte(`{"items":[{"asset_name":"Two"}]}`)}
	if _, err := c.importResources(context.Background(), "assets", batches, false); err == nil || rejects != 1 {
		t.Fatalf("partial preparation was not rejected: rejects=%d err=%v", rejects, err)
	}
}

func writeImportFile(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
