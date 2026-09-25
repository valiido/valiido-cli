package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompletePaginationAndBearer(t *testing.T) {
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/api/v1/commands" {
			t.Error("missing authentication or wrong command path")
		}
		r.ParseForm()
		if r.Form.Get("name") != "documents.list" {
			t.Error("wrong command")
		}
		if requests == 1 {
			writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(1, "rich_text")}, NextCursor: "opaque&?+"})
		} else {
			var input map[string]any
			json.Unmarshal([]byte(r.Form.Get("input")), &input)
			if input["cursor"] != "opaque&?+" {
				t.Error("cursor was not encoded safely")
			}
			writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(2, "folder")}, Complete: true})
		}
	})

	c, _ := newClient(server.URL, "secret")
	docs, err := c.documents(context.Background())
	if err != nil || len(docs) != 2 || requests != 2 {
		t.Fatalf("did not collect every page: %v %v", docs, err)
	}
}

func TestResourceQueryTravelsAcrossPages(t *testing.T) {
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		var input map[string]any
		if json.Unmarshal([]byte(r.Form.Get("input")), &input) != nil || input["query"] != "Customer & cloud" {
			t.Fatalf("query missing from page %d: %#v", requests, input)
		}
		page := resourcePage{Type: "assets", Resources: []resource{}, Complete: requests == 2}
		if requests == 1 {
			page.NextCursor = "next"
		}
		writeCommandResult(w, page)
	})

	c, _ := newClient(server.URL, "secret")
	if _, err := c.resources(context.Background(), "assets", listOptions{query: "Customer & cloud", pageSize: pageSize}); err != nil || requests != 2 {
		t.Fatalf("filtered pagination failed: requests=%d err=%v", requests, err)
	}
}

func TestExamplesCollectEveryPageWithStableQuery(t *testing.T) {
	requests := 0
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil || r.Form.Get("name") != "assets.list_examples" {
			t.Fatal("wrong example command")
		}
		var input map[string]any
		if json.Unmarshal([]byte(r.Form.Get("input")), &input) != nil || input["query"] != "cloud" || input["limit"] != float64(3-requests) {
			t.Fatalf("query or page size changed: %#v", input)
		}
		if requests == 1 {
			if input["cursor"] != "" {
				t.Fatal("first page unexpectedly had a cursor")
			}
			writeCommandResult(w, examplePage{Type: "assets", Examples: []example{{ExampleID: 1, Label: "One", Attributes: map[string]any{"asset_name": "One"}}}, NextCursor: "signed&opaque"})
			return
		}
		if input["cursor"] != "signed&opaque" {
			t.Fatalf("cursor was not preserved: %#v", input)
		}
		writeCommandResult(w, examplePage{Type: "assets", Examples: []example{{ExampleID: 2, Label: "Two", Attributes: map[string]any{"asset_name": "Two"}}}, Complete: true})
	})

	c, _ := newClient(server.URL, "secret")
	page, err := c.examples(context.Background(), "assets", listOptions{query: "cloud", limit: 2, pageSize: pageSize})
	if err != nil || requests != 2 || len(page.Examples) != 2 || !page.Complete || page.NextCursor != "" {
		t.Fatalf("examples were not fully collected: requests=%d page=%#v err=%v", requests, page, err)
	}
}

func TestExamplesRejectBrokenPagination(t *testing.T) {
	for _, mode := range []string{"duplicate", "loop", "inconsistent"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				page := examplePage{Type: "assets", Examples: []example{{ExampleID: 1, Label: "One", Attributes: map[string]any{}}}, NextCursor: "same"}
				if mode == "inconsistent" {
					page.Complete = true
				}
				writeCommandResult(w, page)
			})

			c, _ := newClient(server.URL, "secret")
			if _, err := c.examples(context.Background(), "assets", listOptions{pageSize: pageSize}); err == nil {
				t.Fatal("accepted unsafe example pagination")
			}
		})
	}
}

func TestPaginationFailureNeverReturnsPartialSuccess(t *testing.T) {
	for _, mode := range []string{"failure", "loop", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests > 1 && mode == "failure" {
					http.Error(w, "secret server exception", 500)
					return
				}
				id := int64(requests)
				if mode == "duplicate" {
					id = 1
				}
				writeCommandResult(w, resourcePage{Type: "documents", Resources: []resource{documentResource(id, "rich_text")}, NextCursor: "same"})
			})

			c, _ := newClient(server.URL, "secret")
			docs, err := c.documents(context.Background())
			if err == nil || docs != nil || strings.Contains(err.Error(), "secret server") {
				t.Fatalf("partial success or information leak: %v %v", docs, err)
			}
		})
	}
}

func documentResource(id int64, kind string) resource {
	return resource{Type: "documents", ID: id, Version: resourceVersion(3), Label: "Document", Attributes: map[string]any{"title": "Document", "kind": kind}}
}

func resourceVersion(value int64) *int64 { return &value }

func TestResourceVersionIsOptionalAndNonNegative(t *testing.T) {
	if err := validateResource(resource{Type: "tasks", ID: 1, Version: resourceVersion(-1), Attributes: map[string]any{}}, "tasks"); err == nil {
		t.Fatal("accepted a negative resource version")
	}
	// Read-only resources such as full audits have neither version nor title.
	audit := resource{Type: "auditmagic_fullaudits", ID: 12, Label: "2026-11-18", Attributes: map[string]any{"rating": "good"}}
	if err := validateResource(audit, "auditmagic_fullaudits"); err != nil {
		t.Fatalf("rejected a resource without version: %v", err)
	}
	if _, err := documentFromResource(resource{Type: "documents", ID: 1, Attributes: map[string]any{"title": "Synthetic", "kind": "pdf"}}); err == nil {
		t.Fatal("accepted a document without version")
	}

	if err := validateResource(resource{Type: "tasks", ID: 1, Version: resourceVersion(0), Attributes: map[string]any{}}, "tasks"); err != nil {
		t.Fatalf("rejected initial resource version: %v", err)
	}
}

func TestListResponsesStayWithinClientSafetyBounds(t *testing.T) {
	resources := make([]resource, pageSize+1)
	for index := range resources {
		resources[index] = resource{Type: "assets", ID: int64(index + 1), Version: resourceVersion(0), Attributes: map[string]any{}}
	}
	server := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeCommandResult(w, resourcePage{Type: "assets", Resources: resources, Complete: true})
	})

	c, _ := newClient(server.URL, "secret")
	if _, err := c.resources(context.Background(), "assets", listOptions{pageSize: pageSize}); err == nil {
		t.Fatal("accepted a resource page larger than requested")
	}

	if _, err := c.examples(context.Background(), "assets", listOptions{pageSize: pageSize}); err == nil {
		t.Fatal("accepted an invalid example response")
	}
}

func TestResourceListHasAnAggregateSafetyLimit(t *testing.T) {
	request := 0
	server := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		items := make([]resource, pageSize)
		for index := range items {
			items[index] = resource{
				Type:       "assets",
				ID:         int64(request*pageSize + index + 1),
				Version:    resourceVersion(0),
				Attributes: map[string]any{},
			}
		}
		request++
		writeCommandResult(w, resourcePage{Type: "assets", Resources: items, NextCursor: strconv.Itoa(request)})
	})

	c, _ := newClient(server.URL, "secret")
	if page, err := c.resources(context.Background(), "assets", listOptions{pageSize: pageSize}); err != nil || page.Count != maxListItems || page.Complete || page.NextCursor == "" || request != maxListItems/pageSize {
		t.Fatalf("aggregate safety limit was not enforced: requests=%d err=%v", request, err)
	}
}

func TestListLimitsThroughCLI(t *testing.T) {
	for _, path := range []string{"documents examples", "resource examples documents", "documents list", "resource list documents"} {
		for _, tc := range []struct {
			limit, count, requests int
		}{{1, 1, 1}, {250, 250, 3}, {0, 340, 4}} {
			t.Run(fmt.Sprintf("%s/%d", path, tc.limit), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				var calls []int
				server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/me" {
						json.NewEncoder(w).Encode(identity{TenantNumber: "12345678", Scope: agentScope})
						return
					}
					r.ParseForm()
					var input struct {
						Limit         int
						Cursor, Query string
					}
					if err := json.Unmarshal([]byte(r.Form.Get("input")), &input); err != nil || input.Query != "test's records" {
						t.Errorf("invalid list input: %#v, %v", input, err)
					}
					start, _ := strconv.Atoi(input.Cursor)
					calls = append(calls, input.Limit)
					if input.Limit > 100 {
						t.Errorf("request exceeded server page maximum: %d", input.Limit)
					}
					end := min(start+input.Limit, 340)
					cursor := ""
					if end < 340 {
						cursor = strconv.Itoa(end)
					}
					if strings.Contains(path, "examples") {
						page := examplePage{Type: "documents", NextCursor: cursor, Complete: cursor == ""}
						for i := start; i < end; i++ {
							page.Examples = append(page.Examples, example{ExampleID: int64(i + 1), Label: "Example", Attributes: map[string]any{}})
						}
						writeCommandResult(w, page)
					} else {
						page := resourcePage{Type: "documents", NextCursor: cursor, Complete: cursor == ""}
						for i := start; i < end; i++ {
							page.Resources = append(page.Resources, documentResource(int64(i+1), "rich_text"))
						}
						writeCommandResult(w, page)
					}
				})

				saveTestSession(t, "test", session{Server: server.URL, TenantNumber: "12345678", Scope: agentScope, AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour)})

				args := append(strings.Fields(path), "--query", "test's records", "--profile", "test", "--json", "--page-size", "100")
				if tc.limit > 0 {
					args = append(args, "--limit", strconv.Itoa(tc.limit))
				}
				crumbs := runList(t, args, tc.count, tc.limit == 0)
				if len(calls) != tc.requests || (tc.limit > 0 && calls[0] != min(tc.limit, 100)) || (tc.limit == 250 && calls[2] != 50) {
					t.Fatalf("unexpected page requests: %v", calls)
				}
				var next string
				for _, crumb := range crumbs {
					if crumb.Action == "load_more" {
						next = crumb.Command
					}
				}
				if tc.limit == 0 {
					if next != "" {
						t.Fatalf("complete list advertised more: %q", next)
					}
				} else if !strings.Contains(next, " --cursor '"+strconv.Itoa(tc.count)+"'") || !strings.Contains(next, " --query "+shellArg("test's records")) || !strings.Contains(next, " --profile 'test'") {
					t.Fatalf("missing continuation: %q", next)
				}
				if tc.limit == 250 {
					runList(t, append(args, "--cursor", "250"), 90, true)
				}
			})
		}
	}
}

func runList(t *testing.T, args []string, count int, complete bool) []breadcrumb {
	t.Helper()
	var out bytes.Buffer
	if code := Run(t.Context(), args, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var result struct {
		Data struct {
			Count      int
			Complete   bool
			NextCursor string `json:"next_cursor"`
		}
		Breadcrumbs []breadcrumb
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Count != count || result.Data.Complete != complete || (result.Data.NextCursor == "") != complete {
		t.Fatalf("unexpected list metadata: %+v", result.Data)
	}
	return result.Breadcrumbs
}
