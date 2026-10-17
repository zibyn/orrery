package agent

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/httpjson"
)

// maxFileSize mirrors api.md §2: a single file is at most 5 GiB.
const maxFileSize = int64(5) << 30

// Workspace paths are resolved by os.Root, which applies openat(2) semantics:
// lexical traversal is rejected here, and escaping symlinks are rejected by the
// kernel (file-transfer-mechanics-and-path-safety.md §9).
var errBadPath = errors.New("path must be relative to the Workspace and stay inside it")

type fileEntry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModTime    time.Time `json:"mod_time"`
	LinkTarget *string   `json:"link_target,omitempty"`
}

type fileList struct {
	Items []fileEntry `json:"items"`
}

type pathRequest struct {
	Path string `json:"path"`
}

type moveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// localPath accepts only a path that is lexically inside the Workspace; it
// never rewrites a path that escapes. Empty and "." both name the root.
func localPath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errBadPath
	}
	if raw == "" || raw == "." {
		return ".", nil
	}
	if !filepath.IsLocal(raw) {
		return "", errBadPath
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", errBadPath
		}
	}
	return filepath.Clean(raw), nil
}

// isEscapeErr recognizes os.Root's unexported escape sentinel. It only
// classifies the error; containment has already been enforced by os.Root.
func isEscapeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "escapes from parent")
}

// writeFileProblem and its siblings keep the Agent's file handlers readable;
// the wire format itself is shared with the Control Plane API.
func writeFileProblem(w http.ResponseWriter, code, detail string) {
	httpjson.Problem(w, code, detail)
}

func writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadPath), isEscapeErr(err):
		writeFileProblem(w, "invalid_request", "the path must stay inside the Workspace")
	case errors.Is(err, fs.ErrNotExist):
		writeFileProblem(w, "not_found", "no such file or directory")
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, fs.ErrExist):
		writeFileProblem(w, "conflict", "the directory is not empty or the target exists")
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		writeFileProblem(w, "invalid_request", "the path has the wrong file type")
	case errors.Is(err, fs.ErrPermission):
		writeFileProblem(w, "conflict", "the file operation is not permitted")
	default:
		writeFileProblem(w, "internal", "")
	}
}

func writeFileJSON(w http.ResponseWriter, status int, v any) {
	httpjson.JSON(w, status, v)
}

func decodeFileJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpjson.Decode(w, r, dst)
}

// withWorkspace opens a Sandbox's Workspace and applies the read-only rule for
// mutations (api.md §5). A missing workspace reference means the Sandbox has no
// Workspace on this Node.
func (r *Runtime) withWorkspace(w http.ResponseWriter, req *http.Request, write bool, fn func(root *os.Root)) {
	sid, err := uuid.Parse(req.PathValue("id"))
	if err != nil {
		writeFileProblem(w, "invalid_request", "sandbox id must be a UUID")
		return
	}
	ref, ok := readWorkspaceRef(sandboxPath(r.dataRoot, sid))
	if !ok {
		writeFileProblem(w, "not_found", "the Sandbox has no Workspace on this Node")
		return
	}
	if write && ref.Mode != "rw" {
		writeFileProblem(w, "conflict", "the Workspace is attached read-only")
		return
	}
	wid, err := uuid.Parse(ref.ID)
	if err != nil {
		writeFileProblem(w, "internal", "the Workspace reference is malformed")
		return
	}
	root, err := os.OpenRoot(workspacePath(r.dataRoot, wid))
	if err != nil {
		writeFileProblem(w, "internal", "open the Workspace")
		return
	}
	defer root.Close() //nolint:errcheck // read-only handle
	fn(root)
}

func (r *Runtime) handleFileList(w http.ResponseWriter, req *http.Request) {
	r.withWorkspace(w, req, false, func(root *os.Root) {
		path, err := localPath(req.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, err)
			return
		}
		info, err := root.Lstat(path)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if !info.IsDir() {
			writeFileProblem(w, "invalid_request", "path is not a directory")
			return
		}
		entries, err := fs.ReadDir(root.FS(), path)
		if err != nil {
			writeFileError(w, err)
			return
		}
		list := fileList{Items: make([]fileEntry, 0, len(entries))}
		for _, d := range entries {
			info, err := d.Info()
			if err != nil {
				continue
			}
			list.Items = append(list.Items, fileEntryFor(root, filepath.Join(path, d.Name()), info))
		}
		writeFileJSON(w, http.StatusOK, list)
	})
}

func (r *Runtime) handleFileStat(w http.ResponseWriter, req *http.Request) {
	r.withWorkspace(w, req, false, func(root *os.Root) {
		path, err := localPath(req.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, err)
			return
		}
		info, err := root.Lstat(path)
		if err != nil {
			writeFileError(w, err)
			return
		}
		writeFileJSON(w, http.StatusOK, fileEntryFor(root, path, info))
	})
}

func (r *Runtime) handleFileContent(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.withWorkspace(w, req, false, func(root *os.Root) {
			path, err := localPath(req.URL.Query().Get("path"))
			if err != nil {
				writeFileError(w, err)
				return
			}
			if path == "." {
				writeFileProblem(w, "invalid_request", "path must name a file")
				return
			}
			// Lstat first: opening a FIFO or device would block or read
			// outside the file abstraction (research §9 rule ③).
			info, err := root.Lstat(path)
			if err != nil {
				writeFileError(w, err)
				return
			}
			if !info.Mode().IsRegular() {
				writeFileProblem(w, "invalid_request", "path is not a regular file")
				return
			}
			if info.Size() > maxFileSize {
				writeFileProblem(w, "payload_too_large", "files are limited to 5 GiB")
				return
			}
			f, err := root.Open(path)
			if err != nil {
				writeFileError(w, err)
				return
			}
			defer f.Close() //nolint:errcheck // read-only
			opened, err := f.Stat()
			if err != nil {
				writeFileError(w, err)
				return
			}
			// fstat after open: the name may have been swapped between the
			// checks and the open (TOCTOU), so the inode must be the same.
			if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				writeFileProblem(w, "invalid_request", "the path changed while it was opened")
				return
			}
			// ServeContent implements Range, Content-Length and 206 responses.
			http.ServeContent(w, req, opened.Name(), opened.ModTime(), f)
		})
	case http.MethodPut:
		r.withWorkspace(w, req, true, func(root *os.Root) {
			path, err := localPath(req.URL.Query().Get("path"))
			if err != nil {
				writeFileError(w, err)
				return
			}
			if path == "." {
				writeFileProblem(w, "invalid_request", "path must name a file")
				return
			}
			n, err := writeAtomic(root, path, req.Body, 0o666)
			if errors.Is(err, errTooLarge) || n > maxFileSize {
				writeFileProblem(w, "payload_too_large", "files are limited to 5 GiB")
				return
			}
			if err != nil {
				writeFileError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

func (r *Runtime) handleFileMkdir(w http.ResponseWriter, req *http.Request) {
	var body pathRequest
	if !decodeFileJSON(w, req, &body) {
		return
	}
	r.withWorkspace(w, req, true, func(root *os.Root) {
		path, err := localPath(body.Path)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if path == "." {
			writeFileProblem(w, "invalid_request", "path must name a directory")
			return
		}
		if err := ensureDir(root, path); err != nil {
			writeFileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (r *Runtime) handleFileMove(w http.ResponseWriter, req *http.Request) {
	var body moveRequest
	if !decodeFileJSON(w, req, &body) {
		return
	}
	r.withWorkspace(w, req, true, func(root *os.Root) {
		from, err := localPath(body.From)
		if err != nil {
			writeFileError(w, err)
			return
		}
		to, err := localPath(body.To)
		if err != nil {
			writeFileError(w, err)
			return
		}
		if from == "." || to == "." {
			writeFileProblem(w, "invalid_request", "the Workspace root cannot be moved")
			return
		}
		if err := root.Rename(from, to); err != nil {
			writeFileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (r *Runtime) handleFileDelete(w http.ResponseWriter, req *http.Request) {
	r.withWorkspace(w, req, true, func(root *os.Root) {
		path, err := localPath(req.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, err)
			return
		}
		if path == "." {
			writeFileProblem(w, "invalid_request", "the Workspace root cannot be deleted")
			return
		}
		// Remove deletes a file, a symlink, or an empty directory; a non-empty
		// directory is reported as a conflict rather than wiped implicitly.
		if err := root.Remove(path); err != nil {
			writeFileError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (r *Runtime) handleArchiveGet(w http.ResponseWriter, req *http.Request) {
	r.withWorkspace(w, req, false, func(root *os.Root) {
		path, err := localPath(req.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, err)
			return
		}
		if _, err := root.Lstat(path); err != nil {
			writeFileError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		tw := tar.NewWriter(w)
		fsys := root.FS()
		err = fs.WalkDir(fsys, path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			link := ""
			switch {
			case info.IsDir():
			case info.Mode()&fs.ModeSymlink != 0:
				// Links are archived as links and never followed here.
				link, err = root.Readlink(p)
				if err != nil {
					return err
				}
			case info.Mode().IsRegular():
			default:
				return nil // devices and FIFOs are not part of the API
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = p
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			defer f.Close() //nolint:errcheck // read-only
			n, err := io.Copy(tw, io.LimitReader(f, maxFileSize+1))
			if err == nil && n > maxFileSize {
				return errTooLarge
			}
			return err
		})
		_ = tw.Close()
		if err != nil {
			// Headers are already sent, so the stream just ends short.
			return
		}
	})
}

func (r *Runtime) handleArchivePut(w http.ResponseWriter, req *http.Request) {
	r.withWorkspace(w, req, true, func(root *os.Root) {
		path, err := localPath(req.URL.Query().Get("path"))
		if err != nil {
			writeFileError(w, err)
			return
		}
		if err := ensureDir(root, path); err != nil {
			writeFileError(w, err)
			return
		}
		dst, err := root.OpenRoot(path)
		if err != nil {
			writeFileError(w, err)
			return
		}
		defer dst.Close() //nolint:errcheck // read-only handle
		tr := tar.NewReader(req.Body)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				writeFileProblem(w, "invalid_request", "invalid tar archive: "+err.Error())
				return
			}
			name, err := tarLocalPath(hdr.Name)
			if err != nil {
				writeFileProblem(w, "invalid_request", "the archive contains a path that escapes")
				return
			}
			if name == "." {
				continue
			}
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := ensureDir(dst, name); err != nil {
					writeFileError(w, err)
					return
				}
			case tar.TypeReg, tar.TypeRegA:
				if err := ensureDir(dst, filepath.Dir(name)); err != nil {
					writeFileError(w, err)
					return
				}
				mode := hdr.FileInfo().Mode().Perm()
				if mode == 0 {
					mode = 0o666
				}
				if _, err := writeAtomic(dst, name, tr, mode); err != nil {
					if errors.Is(err, errTooLarge) {
						writeFileProblem(w, "payload_too_large", "files are limited to 5 GiB")
						return
					}
					writeFileError(w, err)
					return
				}
			case tar.TypeSymlink:
				// The write direction never creates links: a tree the Sandbox
				// can write can plant src -> /etc (research §9 rule ②).
				writeFileProblem(w, "invalid_request", "archives may not create symbolic links")
				return
			default:
				writeFileProblem(w, "invalid_request", "the archive contains a special file")
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

var errTooLarge = errors.New("file exceeds 5 GiB")

// writeAtomic writes to a temporary file in the target's directory and renames
// it into place, so readers never observe a half-written file (ADR-0018,
// file-transfer-mechanics-and-path-safety.md §9).
//
// ponytail: a crash between create and rename can leave a .stars-tmp-* file;
// sweep those when Workspace maintenance exists.
func writeAtomic(root *os.Root, path string, body io.Reader, perm fs.FileMode) (int64, error) {
	parent := filepath.Dir(path)
	if err := ensureDir(root, parent); err != nil {
		return 0, err
	}
	tmp := filepath.Join(parent, ".stars-tmp-"+uuid.NewString())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(body, maxFileSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = root.Remove(tmp)
		if copyErr != nil {
			return n, copyErr
		}
		return n, closeErr
	}
	if n > maxFileSize {
		_ = root.Remove(tmp)
		return n, errTooLarge
	}
	if err := root.Chmod(tmp, perm); err != nil {
		_ = root.Remove(tmp)
		return n, err
	}
	if err := root.Rename(tmp, path); err != nil {
		_ = root.Remove(tmp)
		return n, err
	}
	return n, nil
}

// ensureDir creates missing path components. New directories are made
// world-writable because the Sandbox image's USER may differ from the Agent's
// uid; existing directories keep their mode.
func ensureDir(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	current := ""
	for _, part := range strings.Split(dir, "/") {
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		if info, err := root.Lstat(current); err == nil {
			if !info.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: current, Err: syscall.ENOTDIR}
			}
			continue
		}
		if err := root.Mkdir(current, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		_ = root.Chmod(current, 0o777)
	}
	return nil
}

func fileEntryFor(root *os.Root, path string, info fs.FileInfo) fileEntry {
	entry := fileEntry{
		Path: path, Name: filepath.Base(path), Type: fileType(info.Mode()),
		Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime().UTC(),
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if target, err := root.Readlink(path); err == nil {
			entry.LinkTarget = &target
		}
	}
	return entry
}

func fileType(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

func tarLocalPath(name string) (string, error) {
	return localPath(strings.TrimSuffix(name, "/"))
}

func (r *Runtime) writeWorkspaceRefFile(id uuid.UUID, ref workspaceRef) error {
	dir := sandboxPath(r.dataRoot, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "workspace.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "workspace")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("record Workspace reference: %w", err)
	}
	return nil
}

func readWorkspaceRef(dir string) (workspaceRef, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "workspace"))
	if err != nil {
		return workspaceRef{}, false
	}
	var ref workspaceRef
	if json.Unmarshal(raw, &ref) != nil || ref.ID == "" {
		return workspaceRef{}, false
	}
	return ref, true
}

// workspaceRef records which Workspace a Sandbox mounts, so the file API still
// finds it after an Agent restart.
type workspaceRef struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}
