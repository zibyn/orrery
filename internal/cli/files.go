package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
)

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
	list, err := c.ListFiles(ctx, id.String(), path)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tSIZE\tMODE\tPATH")
	for _, item := range list.Items {
		name := item.Path
		if item.Type == "symlink" && item.LinkTarget != nil && *item.LinkTarget != "" {
			name += " -> " + *item.LinkTarget
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
	return c.ReadFile(ctx, id.String(), path, dst, "")
}

func uploadFile(c *client, id uuid.UUID, path string, src io.Reader, size int64) error {
	return c.WriteFile(ctx, id.String(), path, src, size)
}
