package cli

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	starsclient "github.com/zibyn/stars-sandbox/client"
)

func runResource(c *client, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: stars-sandbox resource create|ls|rm|versions|current|push|pull")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("resource create", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: stars-sandbox resource create <name>")
		}
		resource, err := c.CreateResource(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return c.print(resource)
	case "ls":
		fs := flag.NewFlagSet("resource ls", flag.ContinueOnError)
		fs.SetOutput(stderr)
		limit := fs.Int("limit", 0, "page size (1-200)")
		cursor := fs.String("cursor", "", "page cursor")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: stars-sandbox resource ls")
		}
		return listResources(c, *limit, *cursor)
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource rm <name|id>")
		}
		resource, err := resolveResource(c, args[1])
		if err != nil {
			return err
		}
		deleted, err := c.DeleteResource(ctx, resource.ID)
		if err != nil {
			return err
		}
		return c.print(deleted)
	case "versions":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource versions <name|id>")
		}
		return listResourceVersions(c, args[1])
	case "current":
		if len(args) != 2 {
			return fmt.Errorf("usage: stars-sandbox resource current <name|id>")
		}
		resource, err := resolveResource(c, args[1])
		if err != nil {
			return err
		}
		version, err := getResourceVersion(c, resource, "")
		if err != nil {
			return err
		}
		return c.print(version)
	case "push":
		if len(args) != 3 {
			return fmt.Errorf("usage: stars-sandbox resource push <dir> <name>")
		}
		version, err := c.PushResource(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		return c.print(version)
	case "pull":
		if len(args) != 3 {
			return fmt.Errorf("usage: stars-sandbox resource pull <name>[@version] <dir>")
		}
		return c.PullResource(ctx, args[1], args[2])
	default:
		return fmt.Errorf("unknown resource command %q", args[0])
	}
}

// resolveResource accepts an id directly or lists pages until the name matches.
func resolveResource(c *client, arg string) (*starsclient.Resource, error) {
	return c.ResolveResource(ctx, arg)
}

func listResources(c *client, limit int, cursor string) error {
	page, err := c.ListResources(ctx, limit, cursor)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCURRENT\tCREATED")
	for _, item := range page.Items {
		current := "-"
		if item.CurrentVersionID != nil {
			current = *item.CurrentVersionID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.ID, item.Name, current, item.CreatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != nil {
		fmt.Fprintln(c.out, "next_cursor:", *page.NextCursor)
	}
	return nil
}

func listResourceVersions(c *client, ref string) error {
	resource, err := resolveResource(c, ref)
	if err != nil {
		return err
	}
	page, err := c.ListResourceVersions(ctx, resource.ID, 200, "")
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tADDRESS\tSIZE\tSTATE\tCREATED")
	for _, item := range page.Items {
		marker := " "
		if resource.CurrentVersionID != nil && *resource.CurrentVersionID == item.ID {
			marker = "*"
		}
		address := "-"
		if item.Address != nil {
			address = *item.Address
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%d\t%s\t%s\n", marker, item.ID, address, item.Size, item.State, item.CreatedAt)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.NextCursor != nil {
		fmt.Fprintln(c.out, "next_cursor:", *page.NextCursor)
	}
	return nil
}

// getResourceVersion resolves an explicit version, or the current one.
func getResourceVersion(c *client, resource *starsclient.Resource, version string) (*starsclient.ResourceVersion, error) {
	if version == "" {
		if resource.CurrentVersionID == nil {
			return nil, fmt.Errorf("Resource %q has no current version", resource.Name)
		}
		version = *resource.CurrentVersionID
	}
	return c.GetResourceVersion(ctx, resource.ID, version)
}
