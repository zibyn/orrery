package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
)

// transfers may be large, so they are not bound by the 30-second JSON timeout.
var transferClient = &http.Client{}

func runFiles(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox files ls|cp")
	}
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("files ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 || fs.NArg() > 2 {
			return fmt.Errorf("usage: stars-sandbox files ls <sandbox-id> [path]")
		}
		id, err := uuid.Parse(fs.Arg(0))
		if err != nil {
			return fmt.Errorf("sandbox id must be a UUID")
		}
		path := ""
		if fs.NArg() == 2 {
			path = fs.Arg(1)
		}
		return listFiles(c, id, path)
	case "cp":
		fs := flag.NewFlagSet("files cp", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: stars-sandbox files cp <src> <dst> (use <sandbox-id>:<path> for the remote side)")
		}
		return copyFile(c, fs.Arg(0), fs.Arg(1))
	default:
		return fmt.Errorf("unknown files command %q", args[0])
	}
}

func listFiles(c *client, id uuid.UUID, path string) error {
	data, err := c.do(http.MethodGet, "/api/v1/sandboxes/"+id.String()+"/files", url.Values{"path": {path}}, nil, "")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Path       string `json:"path"`
			Name       string `json:"name"`
			Type       string `json:"type"`
			Size       int64  `json:"size"`
			Mode       uint32 `json:"mode"`
			LinkTarget string `json:"link_target"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("decode file list: %w", err)
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tSIZE\tMODE\tPATH")
	for _, item := range list.Items {
		name := item.Path
		if item.Type == "symlink" && item.LinkTarget != "" {
			name += " -> " + item.LinkTarget
		}
		fmt.Fprintf(tw, "%s\t%d\t%04o\t%s\n", item.Type, item.Size, item.Mode, name)
	}
	return tw.Flush()
}

// parseRemote recognizes "<sandbox-uuid>:<path>". The UUID prefix keeps local
// paths that contain a colon from being mistaken for a remote side.
func parseRemote(arg string) (uuid.UUID, string, bool) {
	prefix, path, ok := strings.Cut(arg, ":")
	if !ok {
		return uuid.Nil, "", false
	}
	id, err := uuid.Parse(prefix)
	if err != nil {
		return uuid.Nil, "", false
	}
	return id, path, true
}

func copyFile(c *client, src, dst string) error {
	srcID, srcPath, srcRemote := parseRemote(src)
	dstID, dstPath, dstRemote := parseRemote(dst)
	switch {
	case srcRemote && dstRemote:
		return fmt.Errorf("only one side of cp can be remote")
	case srcRemote:
		if dst == "-" {
			return downloadFile(c, srcID, srcPath, c.out)
		}
		if info, err := os.Stat(dst); err == nil && info.IsDir() {
			dst = filepath.Join(dst, filepath.Base(srcPath))
		}
		return downloadTo(c, srcID, srcPath, dst)
	case dstRemote:
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", src)
		}
		return uploadFile(c, dstID, dstPath, f, info.Size())
	default:
		return fmt.Errorf("one side of cp must be <sandbox-id>:<path>")
	}
}

// downloadTo writes beside the destination and renames, so a failed transfer
// neither truncates nor half-overwrites the existing file.
func downloadTo(c *client, id uuid.UUID, path, dst string) error {
	f, err := os.CreateTemp(filepath.Dir(dst), ".stars-download-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := downloadFile(c, id, path, f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o666); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func downloadFile(c *client, id uuid.UUID, path string, dst io.Writer) error {
	req, err := c.fileRequest(http.MethodGet, id, "/content", path)
	if err != nil {
		return err
	}
	resp, err := transferClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := fileStatus(resp); err != nil {
		return err
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

func uploadFile(c *client, id uuid.UUID, path string, src io.Reader, size int64) error {
	req, err := c.fileRequest(http.MethodPut, id, "/content", path)
	if err != nil {
		return err
	}
	req.Body = io.NopCloser(src)
	req.ContentLength = size
	resp, err := transferClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return fileStatus(resp)
}

func (c *client) fileRequest(method string, id uuid.UUID, suffix, path string) (*http.Request, error) {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	endpoint := strings.TrimRight(c.base, "/") + "/api/v1/sandboxes/" + id.String() + "/files" + suffix + "?" + q.Encode()
	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func fileStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var p struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(data, &p) == nil && p.Code != "" {
		if p.Detail == "" {
			return fmt.Errorf("%s", p.Code)
		}
		return fmt.Errorf("%s: %s", p.Code, p.Detail)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}
