package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ReadFile streams a Workspace file. byteRange, when non-empty, is sent as
// the Range header (api.md §5).
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, dst io.Writer, byteRange string) error {
	_, err := c.stream(ctx, http.MethodGet, c.fileURL(sandboxID, "/content", path), nil, nil, byteRange, dst)
	return err
}

// ReadFileRange reads part of a Workspace file and returns the bytes with the
// response's Content-Type. byteRange, when non-empty, is sent as the Range
// header (api.md §5).
func (c *Client) ReadFileRange(ctx context.Context, sandboxID, path, byteRange string) ([]byte, string, error) {
	var buf bytes.Buffer
	contentType, err := c.stream(ctx, http.MethodGet, c.fileURL(sandboxID, "/content", path), nil, nil, byteRange, &buf)
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), contentType, nil
}

// WriteFile uploads a Workspace file in one request.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, src io.Reader, size int64) error {
	_, err := c.stream(ctx, http.MethodPut, c.fileURL(sandboxID, "/content", path), src, &size, "", nil)
	return err
}

// ReadArchive downloads a tar of path.
func (c *Client) ReadArchive(ctx context.Context, sandboxID, path string, dst io.Writer) error {
	_, err := c.stream(ctx, http.MethodGet, c.fileURL(sandboxID, "/archive", path), nil, nil, "", dst)
	return err
}

// WriteArchive uploads and unpacks a tar at path.
func (c *Client) WriteArchive(ctx context.Context, sandboxID, path string, src io.Reader, size int64) error {
	_, err := c.stream(ctx, http.MethodPut, c.fileURL(sandboxID, "/archive", path), src, &size, "", nil)
	return err
}

func (c *Client) fileURL(sandboxID, suffix, path string) string {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	return c.baseURL + "/api/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files" + suffix + "?" + q.Encode()
}

// stream performs a raw-body request and returns the response's Content-Type.
// Transfers are not bound by the JSON timeout.
func (c *Client) stream(ctx context.Context, method, endpoint string, body io.Reader, size *int64, byteRange string, dst io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	if size != nil {
		req.ContentLength = *size
	}
	resp, err := c.transfers.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", parseError(resp.StatusCode, data)
	}
	contentType := resp.Header.Get("Content-Type")
	if dst == nil {
		return contentType, nil
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return "", fmt.Errorf("transfer file: %w", err)
	}
	return contentType, nil
}
