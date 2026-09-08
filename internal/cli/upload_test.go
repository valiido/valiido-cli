package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPDFUploadUsesAuthenticatedAPIAndStorage(t *testing.T) {
	pdf := []byte("%PDF-1.4\nsynthetic test\n%%EOF\n")
	filename := filepath.Join(t.TempDir(), "policy.pdf")
	if err := os.WriteFile(filename, pdf, 0600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("request did not use the connection bearer token")
		}
		switch r.URL.Path {
		case "/api/v1/uploads":
			if err := r.ParseForm(); err != nil || r.Form.Get("filename") != "policy.pdf" {
				t.Fatal("invalid upload metadata")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"signed_id": "signed", "filename": "policy.pdf",
				"content_type": "application/pdf", "byte_size": len(pdf),
				"direct_upload": map[string]any{
					"url": server.URL + "/storage", "headers": map[string]string{"Content-Type": "application/pdf"},
				},
			})
		case "/storage":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPut || string(body) != string(pdf) {
				t.Fatal("storage did not receive the PDF")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatal("unexpected request path")
		}
	}))
	defer server.Close()

	client, _ := newClient(server.URL, "secret")
	upload, err := client.uploadPDF(context.Background(), filename)
	if err != nil || upload.SignedID != "signed" {
		t.Fatalf("upload failed: %#v %v", upload, err)
	}
}

func TestPDFUploadDoesNotSendBearerToStorageHost(t *testing.T) {
	pdf := []byte("%PDF-1.4\nsynthetic test\n%%EOF\n")
	filename := filepath.Join(t.TempDir(), "policy.pdf")
	if err := os.WriteFile(filename, pdf, 0600); err != nil {
		t.Fatal(err)
	}

	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("connection bearer token leaked to storage host")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"signed_id": "signed", "filename": "policy.pdf",
			"content_type": "application/pdf", "byte_size": len(pdf),
			"direct_upload": map[string]any{"url": storage.URL, "headers": map[string]string{"Content-Type": "application/pdf"}},
		})
	}))
	defer api.Close()

	client, _ := newClient(api.URL, "secret")
	// Trust only this synthetic TLS server for the storage request.
	client.http.Transport = storage.Client().Transport
	if _, err := client.uploadPDF(context.Background(), filename); err != nil {
		t.Fatal(err)
	}
}

func TestPDFUploadRejectsNonPDFBeforeNetwork(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "notes.pdf")
	if err := os.WriteFile(filename, []byte("plain text"), 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := newClient("http://127.0.0.1:1", "secret")
	if _, err := client.uploadPDF(context.Background(), filename); err == nil {
		t.Fatal("accepted a file without a PDF signature")
	}
}
