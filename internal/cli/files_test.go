package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	server = newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("request did not use the connection bearer token")
		}
		switch r.URL.Path {
		case "/api/v1/uploads":
			if err := r.ParseForm(); err != nil || r.Form.Get("filename") != "policy.pdf" {
				t.Fatal("invalid upload metadata")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"signed_id": "signed", "attachable_sgid": "attachable", "filename": "policy.pdf",
				"content_type": "application/pdf", "byte_size": len(pdf),
				"direct_upload": map[string]any{
					"url": server.URL + "/storage", "headers": map[string]string{"Content-Type": "application/pdf"},
				},
			})
		case "/storage":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPut || r.ContentLength != int64(len(pdf)) || string(body) != string(pdf) {
				t.Fatal("storage did not receive the PDF")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatal("unexpected request path")
		}
	})
	defer server.Close()

	client, _ := newClient(server.URL, "secret")
	upload, err := client.uploadFile(context.Background(), filename)
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

	storage := startPipeServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("connection bearer token leaked to storage host")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer storage.Close()

	api := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"signed_id": "signed", "attachable_sgid": "attachable", "filename": "policy.pdf",
			"content_type": "application/pdf", "byte_size": len(pdf),
			"direct_upload": map[string]any{"url": storage.URL, "headers": map[string]string{"Content-Type": "application/pdf"}},
		})
	})
	defer api.Close()

	client, _ := newClient(api.URL, "secret")
	// Trust only this synthetic TLS server for the storage request.
	client.http.Transport = storage.Client().Transport
	if _, err := client.uploadFile(context.Background(), filename); err != nil {
		t.Fatal(err)
	}
}

func TestFileUploadRejectsUnsupportedContentBeforeNetwork(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "notes.pdf")
	if err := os.WriteFile(filename, []byte("plain text"), 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := newClient("http://127.0.0.1:1", "secret")
	if _, err := client.uploadFile(context.Background(), filename); err == nil {
		t.Fatal("accepted an unsupported file")
	}
}

func TestImageUploadUsesDetectedContentType(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nsynthetic image")
	filename := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(filename, image, 0600); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	server = newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/uploads":
			if err := r.ParseForm(); err != nil || r.Form.Get("content_type") != "image/png" {
				t.Fatal("image content type was not detected")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"signed_id": "signed", "attachable_sgid": "attachable", "filename": "diagram.png",
				"content_type": "image/png", "byte_size": len(image),
				"direct_upload": map[string]any{
					"url": server.URL + "/storage", "headers": map[string]string{"Content-Type": "image/png"},
				},
			})
		case "/storage":
			body, _ := io.ReadAll(r.Body)
			if string(body) != string(image) {
				t.Fatal("storage did not receive the image")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatal("unexpected request path")
		}
	})
	defer server.Close()

	client, _ := newClient(server.URL, "secret")
	upload, err := client.uploadFile(context.Background(), filename)
	if err != nil || upload.AttachableSGID != "attachable" {
		t.Fatalf("upload failed: %#v %v", upload, err)
	}
}

func TestOfficeUploadContentType(t *testing.T) {
	for _, tc := range []struct{ filename, part, want string }{
		{"evidence.DOCX", "word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"register.xlsx", "xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"renamed.docx", "xl/workbook.xml", ""},
		{"archive.xlsx", "arbitrary.txt", ""},
		{"workbook.zip", "xl/workbook.xml", ""},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			var data bytes.Buffer
			archive := zip.NewWriter(&data)
			for _, name := range []string{"[Content_Types].xml", tc.part} {
				if _, err := archive.Create(name); err != nil {
					t.Fatal(err)
				}
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if got := uploadContentType(tc.filename, data.Bytes()); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPDFDownloadDoesNotOverwrite(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		io.WriteString(w, "%PDF-1.4\nTest\n%%EOF")
	})

	c, _ := newClient(server.URL, "secret")
	path := filepath.Join(t.TempDir(), "test.pdf")
	if err := c.downloadPDF(context.Background(), "risks", 1, path, false); err != nil {
		t.Fatal(err)
	}
	if err := c.downloadPDF(context.Background(), "risks", 1, path, false); err == nil {
		t.Fatal("overwrote existing PDF")
	}
}

func TestPDFDownloadRejectionsLeaveNoFile(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		headers    map[string]string
		body, code string
	}{
		{
			name: "API error", status: http.StatusNotFound,
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"error":{"code":"not_found","message":"The requested asset could not be found."}}`,
			code:    "not_found",
		},
		{
			name: "partial download", status: http.StatusOK,
			headers: map[string]string{"Content-Type": "application/pdf", "Content-Length": "100"},
			body:    "%PDF-short", code: "protocol",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t, func(writer http.ResponseWriter, _ *http.Request) {
				for name, value := range test.headers {
					writer.Header().Set(name, value)
				}
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.body)
			})
			client, _ := newClient(server.URL, "test-token")
			filename := filepath.Join(t.TempDir(), "synthetic.pdf")
			err := client.downloadPDF(t.Context(), "assets", 99, filename, false)
			var failure *apiError
			if !errors.As(err, &failure) || failure.Code != test.code {
				t.Fatalf("unexpected PDF failure: %v", err)
			}
			if test.code == "not_found" && exitCode(failure.Code) != 5 {
				t.Fatalf("unexpected PDF error exit code: %d", exitCode(failure.Code))
			}
			if _, err := os.Stat(filename); !os.IsNotExist(err) {
				t.Fatal("failed download left an output file")
			}
		})
	}
}
