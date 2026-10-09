package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxFileUpload = 50 << 20

func uploadContentType(filename string, data []byte) string {
	detected := http.DetectContentType(data)
	switch detected {
	case "application/pdf", "image/gif", "image/jpeg", "image/png", "image/webp":
		return detected
	case "application/zip":
		return officeContentType(filename, data)
	default:
		return ""
	}
}

func officeContentType(filename string, data []byte) string {
	var contentType, documentPart string
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".docx":
		contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		documentPart = "word/document.xml"
	case ".xlsx":
		contentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		documentPart = "xl/workbook.xml"
	default:
		return ""
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	var hasContentTypes, hasDocumentPart bool
	for _, part := range archive.File {
		hasContentTypes = hasContentTypes || part.Name == "[Content_Types].xml"
		hasDocumentPart = hasDocumentPart || part.Name == documentPart
	}
	if hasContentTypes && hasDocumentPart {
		return contentType
	}
	return ""
}

type directUpload struct {
	SignedID       string `json:"signed_id"`
	AttachableSGID string `json:"attachable_sgid"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	ByteSize       int64  `json:"byte_size"`
	Upload         struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"direct_upload"`
}

func readUpload(filename string) ([]byte, string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, "", failure("storage", "Could not open the upload file.")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxFileUpload {
		return nil, "", failure("storage", "Upload must be a regular file between 1 byte and 50 MiB.")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileUpload+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, "", failure("storage", "Could not read the upload file or its size changed.")
	}
	contentType := uploadContentType(filename, data)
	if contentType == "" {
		return nil, "", failure("invalid_input", "Only PDF, GIF, JPEG, PNG, WebP, DOCX, and XLSX files can be uploaded.")
	}
	return data, contentType, nil
}

func (c *client) uploadFile(ctx context.Context, filename string) (directUpload, error) {
	data, contentType, err := readUpload(filename)
	if err != nil {
		return directUpload{}, err
	}
	checksum := md5.Sum(data)

	var upload directUpload
	form := url.Values{
		"filename":     {filepath.Base(filename)},
		"byte_size":    {fmt.Sprint(len(data))},
		"checksum":     {base64.StdEncoding.EncodeToString(checksum[:])},
		"content_type": {contentType},
	}
	if err := c.json(ctx, http.MethodPost, apiPaths["api/uploads#create"], form, true, false, &upload); err != nil {
		return directUpload{}, uncertainWrite(err, "Upload initialization may have reserved an unused file. Start a new upload only if you still need attachment identifiers.")
	}
	if upload.SignedID == "" || upload.AttachableSGID == "" || upload.Filename == "" || upload.ContentType != contentType || upload.ByteSize != int64(len(data)) {
		return directUpload{}, failure("protocol", "Server returned invalid upload metadata.")
	}

	uploadURL, sameOrigin, err := c.validateUploadURL(upload.Upload.URL)
	if err != nil {
		return directUpload{}, err
	}
	if err := c.putUpload(ctx, uploadURL, upload.Upload.Headers, data, sameOrigin); err != nil {
		return directUpload{}, err
	}
	return upload, nil
}

func (c *client) putUpload(ctx context.Context, uploadURL *url.URL, headers map[string]string, data []byte, sameOrigin bool) error {
	for name := range headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Host") {
			return failure("protocol", "Server returned an unsafe upload header.")
		}
	}
	httpClient := *c.http
	httpClient.Timeout = transferRequestTimeout
	response, err := c.withRetry(ctx, retryPolicy{transient: true, rateLimited: true}, httpClient.Timeout, func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL.String(), bytes.NewReader(data))
		if err != nil {
			return nil, failure("protocol", "Could not build the storage upload request.")
		}
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		request.Header.Set("User-Agent", "valiido-cli/"+version)
		if sameOrigin {
			request.Header.Set("Authorization", "Bearer "+c.token)
		}
		return c.do(&httpClient, request)
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if transientStatus(response.StatusCode) || response.StatusCode == 429 {
		return responseError(response.StatusCode, nil)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return failure("storage", fmt.Sprintf("File upload failed (HTTP %d).", response.StatusCode))
	}

	return nil
}

func (c *client) validateUploadURL(raw string) (*url.URL, bool, error) {
	uploadURL, err := url.Parse(raw)
	if err != nil || uploadURL.Host == "" || uploadURL.User != nil || uploadURL.Fragment != "" {
		return nil, false, failure("protocol", "Server returned an invalid upload URL.")
	}
	local := uploadURL.Hostname() == "127.0.0.1" || uploadURL.Hostname() == "::1"
	if uploadURL.Scheme != "https" && !(uploadURL.Scheme == "http" && local) {
		return nil, false, failure("protocol", "Server returned an insecure upload URL.")
	}
	baseURL, _ := url.Parse(c.base)
	sameOrigin := uploadURL.Scheme == baseURL.Scheme && uploadURL.Host == baseURL.Host
	return uploadURL, sameOrigin, nil
}

// The signed storage URL is only needed for the PUT. Keep it out of CLI output.
func (u directUpload) identifiers() any {
	return map[string]any{
		"signed_id": u.SignedID, "attachable_sgid": u.AttachableSGID,
		"filename": u.Filename, "content_type": u.ContentType, "byte_size": u.ByteSize,
	}
}

func (client *client) downloadPDF(ctx context.Context, resourceType string, id int64, output string, dryRun bool) error {
	if _, err := os.Lstat(output); err == nil {
		return failure("storage", "Output file already exists. Choose a new filename.")
	} else if !errors.Is(err, os.ErrNotExist) {
		return failure("storage", "Could not inspect the output file.")
	}
	if dir, err := os.Stat(filepath.Dir(output)); err != nil || !dir.IsDir() {
		return failure("storage", "Output directory is unavailable. Choose an existing directory.")
	}
	const maxPDF = 50 << 20
	previous := client.http
	transfer := *client.http
	transfer.Timeout = transferRequestTimeout
	client.http = &transfer
	defer func() { client.http = previous }()
	response, err := client.requestAPI(ctx, "GET", fmt.Sprintf(apiPaths["api/resources#pdf"], url.PathEscape(resourceType), id), nil, true, func(resp *http.Response) (*http.Response, error) {
		buffered, err := bufferResponse(resp, maxPDF)
		var apiErr *apiError
		if resp.StatusCode == http.StatusOK && err != nil && !errors.As(err, &apiErr) {
			return nil, failure("protocol", "PDF download was incomplete or exceeded 50 MiB.")
		}
		return buffered, err
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 {
		return responseError(response.StatusCode, nil)
	}
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxJSON+1))
		if readErr != nil || len(data) > maxJSON {
			return failure("protocol", "API response could not be read or exceeded the size limit.")
		}
		return responseError(response.StatusCode, data)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/pdf") {
		return failure("api", "PDF download failed or returned a non-PDF response.")
	}
	// Validate the header before creating a file. Never follow a signed blob URL.
	header := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, header); err != nil || string(header) != "%PDF-" {
		return failure("protocol", "Response does not have a PDF signature.")
	}
	if dryRun {
		return nil
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			return failure("storage", "Cannot create output file; existing files are never overwritten.")
		case errors.Is(err, os.ErrNotExist):
			return failure("storage", "Output directory does not exist: "+filepath.Dir(output))
		case errors.Is(err, os.ErrPermission):
			return failure("storage", "Permission denied while creating the output file.")
		default:
			return failure("storage", "Could not create the output file.")
		}
	}
	saved := false
	defer func() {
		if !saved {
			_ = file.Close()
			_ = os.Remove(output)
		}
	}()
	written, err := io.Copy(file, io.LimitReader(io.MultiReader(strings.NewReader(string(header)), response.Body), maxPDF+1))
	if err != nil || written > maxPDF {
		return failure("protocol", "PDF download was incomplete or exceeded 50 MiB.")
	}
	if err := file.Close(); err != nil {
		return failure("storage", "Could not finish saving PDF.")
	}
	saved = true
	return nil
}
