package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
)

const (
	maxCommandInput  = 64 << 10
	maxImportFile    = 16 << 20
	maxImportItems   = 5000
	maxImportBatches = 50
	importBatchSize  = 100
)

type importBatchResult struct {
	Index                 int    `json:"index"`
	Count                 int    `json:"count"`
	Summary               string `json:"summary"`
	DryRun                bool   `json:"dry_run,omitempty"`
	ConfirmationToken     string `json:"confirmation_token,omitempty"`
	ConfirmationExpiresIn int64  `json:"confirmation_expires_in,omitempty"`
	Command               string `json:"command,omitempty"`
	Risk                  string `json:"risk,omitempty"`
}

func readImportBatches(path string) ([][]byte, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, failure("usage", "Could not open the import file.")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, failure("usage", "Import file must be a regular file.")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImportFile+1))
	if err != nil || len(data) > maxImportFile {
		return nil, 0, failure("usage", "Import file could not be read or exceeds 16 MiB.")
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(data, &rawItems) != nil || len(rawItems) == 0 {
		return nil, 0, failure("usage", "Import file must contain a non-empty JSON array of objects.")
	}
	if len(rawItems) > maxImportItems {
		return nil, 0, failure("usage", "Import file cannot contain more than 5000 resources.")
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		var item map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&item) != nil || item == nil {
			return nil, 0, failure("usage", "Every imported resource must be a JSON object.")
		}
		items = append(items, item)
	}

	batches := make([][]byte, 0, (len(items)+importBatchSize-1)/importBatchSize)
	current := make([]map[string]any, 0, importBatchSize)
	for _, item := range items {
		candidate := append(current, item)
		encoded, err := json.Marshal(map[string]any{"items": candidate})
		if err != nil {
			return nil, 0, failure("usage", "Could not encode the import file.")
		}
		if len(candidate) <= importBatchSize && len(encoded) <= maxCommandInput {
			current = candidate
			continue
		}
		if len(current) == 0 {
			return nil, 0, failure("usage", "One imported resource exceeds the 64 KiB command limit.")
		}
		batch, _ := json.Marshal(map[string]any{"items": current})
		batches = append(batches, batch)
		current = []map[string]any{item}
		encoded, _ = json.Marshal(map[string]any{"items": current})
		if len(encoded) > maxCommandInput {
			return nil, 0, failure("usage", "One imported resource exceeds the 64 KiB command limit.")
		}
	}
	if len(current) > 0 {
		batch, _ := json.Marshal(map[string]any{"items": current})
		batches = append(batches, batch)
	}
	if len(batches) > maxImportBatches {
		return nil, 0, failure("usage", "Import requires more than 50 batches; split the file into smaller imports.")
	}
	return batches, len(items), nil
}

func (c *client) importResources(ctx context.Context, resourceType string, batches [][]byte, dryRun bool) ([]importBatchResult, error) {
	name := resourceType + ".bulk_create"
	validated := make([]commandResult, 0, len(batches))
	for _, batch := range batches {
		result, err := c.runCommand(ctx, name, string(batch), true)
		if err != nil {
			return nil, err
		}
		validated = append(validated, result)
	}
	if dryRun {
		return compactImportResults(validated, batches, false)
	}

	prepared := make([]commandResult, 0, len(batches))
	for _, batch := range batches {
		result, err := c.runCommand(ctx, name, string(batch), false)
		if err != nil {
			rejectPrepared(ctx, c, prepared)
			return nil, err
		}
		if token, ok := result.Data["confirmation_token"].(string); !ok || token == "" {
			rejectPrepared(ctx, c, prepared)
			return nil, failure("protocol", "Bulk import did not return a confirmation token.")
		}
		prepared = append(prepared, result)
	}
	return compactImportResults(prepared, batches, true)
}

func compactImportResults(results []commandResult, batches [][]byte, prepared bool) ([]importBatchResult, error) {
	compact := make([]importBatchResult, 0, len(results))
	for index, result := range results {
		var payload struct {
			Items []json.RawMessage `json:"items"`
		}
		if index >= len(batches) || json.Unmarshal(batches[index], &payload) != nil || len(payload.Items) == 0 {
			return nil, failure("protocol", "Could not summarize an import batch.")
		}
		batch := importBatchResult{
			Index: index + 1, Count: len(payload.Items), Summary: result.Summary, DryRun: result.DryRun,
		}
		if prepared {
			batch.ConfirmationToken, _ = result.Data["confirmation_token"].(string)
			batch.Command, _ = result.Data["command_name"].(string)
			batch.Risk, _ = result.Data["risk"].(string)
			if seconds, ok := result.Data["confirmation_expires_in"].(float64); ok {
				batch.ConfirmationExpiresIn = int64(seconds)
			}
		}
		compact = append(compact, batch)
	}
	return compact, nil
}

func rejectPrepared(ctx context.Context, c *client, prepared []commandResult) {
	for _, prior := range prepared {
		if token, ok := prior.Data["confirmation_token"].(string); ok && token != "" {
			_, _ = c.resolveCommand(ctx, "reject", token)
		}
	}
}
