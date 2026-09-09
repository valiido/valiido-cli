package cli

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxFileUpload = 50 << 20

var uploadContentTypes = map[string]bool{
	"application/pdf": true,
	"image/gif":       true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/webp":      true,
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

func (c *client) uploadFile(ctx context.Context, filename string) (directUpload, error) {
	file, err := os.Open(filename)
	if err != nil {
		return directUpload{}, failure("storage", "Could not open the file.")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxFileUpload {
		return directUpload{}, failure("storage", "Upload must be a regular file between 1 byte and 50 MiB.")
	}
	header := make([]byte, min(512, int(info.Size())))
	if _, err := io.ReadFull(file, header); err != nil {
		return directUpload{}, failure("storage", "Could not read the file.")
	}
	contentType := http.DetectContentType(header)
	if !uploadContentTypes[contentType] {
		return directUpload{}, failure("invalid_input", "Only PDF, GIF, JPEG, PNG, and WebP files can be uploaded.")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return directUpload{}, failure("storage", "Could not read the file.")
	}
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return directUpload{}, failure("storage", "Could not read the file.")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return directUpload{}, failure("storage", "Could not read the file.")
	}

	var upload directUpload
	form := url.Values{
		"filename":     {filepath.Base(filename)},
		"byte_size":    {fmt.Sprint(info.Size())},
		"checksum":     {base64.StdEncoding.EncodeToString(hash.Sum(nil))},
		"content_type": {contentType},
	}
	if err := c.json(ctx, http.MethodPost, apiPaths["api/uploads#create"], form, true, false, &upload); err != nil {
		return directUpload{}, err
	}
	if upload.SignedID == "" || upload.AttachableSGID == "" || upload.Filename == "" || upload.ContentType != contentType || upload.ByteSize != info.Size() {
		return directUpload{}, failure("protocol", "Server returned invalid upload metadata.")
	}

	uploadURL, sameOrigin, err := c.validateUploadURL(upload.Upload.URL)
	if err != nil {
		return directUpload{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL.String(), file)
	if err != nil {
		return directUpload{}, failure("protocol", "Could not build the storage upload request.")
	}
	request.ContentLength = info.Size()
	for name, value := range upload.Upload.Headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Host") {
			return directUpload{}, failure("protocol", "Server returned an unsafe upload header.")
		}
		request.Header.Set(name, value)
	}
	request.Header.Set("User-Agent", "valiido-cli/"+version)
	if sameOrigin {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	httpClient := *c.http
	httpClient.Timeout = 5 * time.Minute
	response, err := httpClient.Do(request)
	if err != nil {
		return directUpload{}, failure("network", "Could not upload the file.")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return directUpload{}, failure("storage", fmt.Sprintf("File upload failed (HTTP %d).", response.StatusCode))
	}

	return upload, nil
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
