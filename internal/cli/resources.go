package cli

import (
	"context"
	"flag"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

func listResources(ctx context.Context, c *client, stdout *output, resourceType, path string, resourceAlias, decodeDocuments bool, options listOptions) error {
	stdout.kind = resourceType
	if resourceType == "guide_items" {
		options.pageSize = min(options.pageSize, 50)
		if options.query != "" {
			return failure("usage", "This action does not accept --query. Check its input schema with command list.")
		}
	} else if resourceAlias && !isResource(resourceType) || resourceType == "access_levels" || resourceType == "chats" || resourceType == "changelog" || resourceType == "frameworks" {
		result, handled, err := c.specialResourceRead(ctx, resourceType+".list", 0, options)
		if err != nil {
			return err
		}
		if handled {
			return specialListSuccess(stdout, result, options)
		}
	}
	page, err := c.resources(ctx, resourceType, options)
	if err != nil {
		return err
	}
	var data any = page
	if decodeDocuments {
		documents, err := documentsFromResources(page.Resources)
		if err != nil {
			return err
		}
		list := map[string]any{"type": page.Type, "resources": documents, "count": page.Count, "complete": page.Complete}
		if page.NextCursor != "" {
			list["next_cursor"] = page.NextCursor
		}
		data = list
	}
	return listSuccess(stdout, data, path, page.NextCursor, options)
}

func listTasks(call *invocation) error { return call.list("tasks", "tasks list", false) }
func listRisks(call *invocation) error { return call.list("risks", "risks list", false) }
func readTask(call *invocation) error  { return call.readByID("tasks") }
func readRisk(call *invocation) error  { return call.readByID("risks") }

func listResource(call *invocation) error {
	kind, err := call.takeResourceType()
	if err != nil {
		return err
	}
	return call.list(kind, "resource list "+kind, false)
}

func (call *invocation) list(kind, path string, decodeDocuments bool) error {
	options, err := call.parseListOptions(maxListItems, true)
	if err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		return listResources(ctx, client, call.output, kind, path, call.resourceAlias, decodeDocuments, options)
	})
}

func readResource(call *invocation) error {
	kind, id, err := call.takeResourceReference()
	if err != nil {
		return err
	}
	return call.readRecord(kind, id)
}

func (call *invocation) readByID(kind string) error {
	id, err := call.takeResourceID()
	if err != nil {
		return err
	}
	return call.readRecord(kind, id)
}

func (call *invocation) readRecord(kind string, id int64) error {
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		call.output.kind = kind
		if call.resourceAlias && !isResource(kind) {
			result, handled, err := client.specialResourceRead(ctx, kind+".show", id, listOptions{})
			if err != nil {
				return err
			}
			if handled {
				return commandSuccess(call.output, result)
			}
		}
		item, err := client.readResource(ctx, kind, id)
		if err != nil {
			return err
		}
		return success(call.output, item)
	})
}

func (call *invocation) takeResourceType() (string, error) {
	kind, err := call.takeArgument("Expected a resource type before options.")
	if err != nil {
		return "", err
	}
	return validateResourceType(kind)
}

func (call *invocation) takeResourceReference() (string, int64, error) {
	if len(call.args) < 2 || strings.TrimSpace(call.args[0]) == "" {
		return "", 0, usageError("Expected a resource type and positive resource ID before options.")
	}
	rawKind := call.args[0]
	call.args = call.args[1:]
	id, err := call.takeResourceID()
	if err != nil {
		return "", 0, err
	}
	kind, err := validateResourceType(rawKind)
	return kind, id, err
}

func validateResourceType(raw string) (string, error) {
	kind := strings.ToLower(raw)
	if !resourceTypePattern.MatchString(kind) {
		return "", usageError("Resource type must use lowercase letters, numbers and underscores and cannot exceed 64 characters.")
	}
	return kind, nil
}

func listExamples(call *invocation) error {
	kind, err := call.takeResourceType()
	if err != nil {
		return err
	}
	options, err := call.parseListOptions(maxListItems, true)
	if err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		result, err := client.examples(ctx, kind, options)
		if err != nil {
			return err
		}
		return listSuccess(call.output, result, "resource examples "+kind, result.NextCursor, options)
	})
}

func importResource(call *invocation) error {
	kind, err := call.takeResourceType()
	if err != nil {
		return err
	}
	var filename string
	var dryRun bool
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.StringVar(&filename, "file", "", "JSON array to import")
		flags.BoolVar(&dryRun, "dry-run", false, "Validate without preparing changes")
	}, nil); err != nil {
		return err
	}
	if filename == "" {
		return usageError("--file is required for resource import.")
	}
	batches, total, err := readImportBatches(filename)
	if err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		results, err := client.importResources(ctx, kind, batches, dryRun)
		if err != nil {
			return err
		}
		return writeResponse(call.output, response{OK: true, DryRun: dryRun, Data: map[string]any{
			"type": kind, "count": total, "batches": results,
			"dry_run": dryRun, "confirmation_required": !dryRun,
		}})
	})
}

func saveResourcePDF(call *invocation) error {
	kind, id, err := call.takeResourceReference()
	if err != nil {
		return err
	}
	var filename string
	var dryRun bool
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.StringVar(&filename, "output", "", "PDF output filename")
		flags.BoolVar(&dryRun, "dry-run", false, "Preview saving a PDF")
	}, nil); err != nil {
		return err
	}
	if filename == "" {
		return usageError("--output is required for resource pdf.")
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		if err := client.downloadPDF(ctx, kind, id, filename, dryRun); err != nil {
			return err
		}
		return pdfSuccess(call.output, kind, id, filename, dryRun)
	})
}

func pdfSuccess(out *output, kind string, id int64, filename string, dryRun bool) error {
	data := map[string]any{"id": id}
	if kind != "" {
		data["type"] = kind
	}
	if dryRun {
		data["output"] = filename
		return writeResponse(out, response{OK: true, DryRun: true, Summary: "PDF export preview", Data: data})
	}
	data["saved_to"] = filename
	return success(out, data)
}

// Use the web path supplied by Rails after the profile identity was verified.
func recordURL(origin, tenant, raw string) string {
	if !tenantPattern.MatchString(tenant) || origin == "" || raw == "" {
		return ""
	}
	base, _ := url.Parse(origin)
	link, err := url.Parse(raw)
	if err != nil || base == nil || link.User != nil || link.Opaque != "" || link.RawPath != "" ||
		link.RawQuery != "" || link.ForceQuery || link.Fragment != "" ||
		path.Clean(link.Path) != link.Path || strings.ContainsAny(link.Path, "\\%") ||
		!strings.HasPrefix(link.Path, "/"+tenant+"/isms/") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") && !link.IsAbs() || strings.HasPrefix(raw, "//") ||
		link.IsAbs() && (link.Scheme != base.Scheme || link.Host != base.Host) {
		return ""
	}
	return base.ResolveReference(link).String()
}

func openRecord(call *invocation) error {
	if len(call.args) == 0 {
		return usageError("Expected a resource noun and ID.")
	}
	kind, depth := resolveNoun(call.args)
	call.args = append([]string{kind}, call.args[depth:]...)
	kind, id, err := call.takeResourceReference()
	if err != nil {
		return err
	}
	if _, err := call.parse(nil, nil); err != nil {
		return err
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		item, err := client.readResource(ctx, kind, id)
		if err != nil {
			return err
		}
		link := recordURL(call.output.origin, call.output.tenant, item.URL)
		if link == "" {
			return failure("no_url", "This record has no browser URL.")
		}
		if terminal(call.output.Writer) && (call.output.mode == "" || call.output.mode == "styled") {
			if err := launchBrowser(link); err != nil {
				return err
			}
		}
		return writeResponse(call.output, response{
			OK: true, Summary: "Browser URL", URL: link, Data: map[string]any{"url": link},
		})
	})
}

func launchBrowser(link string) error {
	args := []string{"xdg-open", link}
	switch runtime.GOOS {
	case "darwin":
		args[0] = "open"
	case "windows":
		args = []string{"rundll32.exe", "url.dll,FileProtocolHandler", link}
	}
	command := exec.Command(args[0], args[1:]...)
	if err := command.Start(); err != nil {
		return failure("browser", "Could not start the browser.")
	}
	_ = command.Process.Release()
	return nil
}
