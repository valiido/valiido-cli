package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

func readDocument(ctx context.Context, client *client, stdout *output, id int64, filename string, text, dryRun bool) error {
	stdout.kind = "documents"
	item, err := client.readResource(ctx, "documents", id)
	if err != nil {
		return err
	}
	document, err := documentFromResource(item)
	if err != nil {
		return err
	}
	switch {
	case filename != "" && document.Kind != "pdf":
		return usageError("--output is only for PDF documents; use --text for text documents.")
	case filename != "":
		if err := client.downloadPDF(ctx, "documents", id, filename, dryRun); err != nil {
			return err
		}
		return pdfSuccess(stdout, "", id, filename, dryRun)
	case text && (document.Kind != "rich_text" || document.ContentState != "text"):
		return usageError("Document has no text body. For PDFs, use --output FILE.pdf.")
	case text:
		_, err := fmt.Fprintln(stdout, strings.Map(documentTextRune, document.ContentText))
		return err
	default:
		return writeResponse(stdout, response{
			OK: true, Data: document, URL: recordURL(stdout.origin, stdout.tenant, item.URL),
		})
	}
}

func documentTextRune(character rune) rune {
	if unicode.IsControl(character) && character != '\n' && character != '\t' ||
		unicode.Is(unicode.Cf, character) && character != '\u200c' && character != '\u200d' {
		return -1
	}
	return character
}

func listDocuments(call *invocation) error { return call.list("documents", "documents list", true) }

func runDocumentRead(call *invocation) error {
	id, err := call.takeResourceID()
	if err != nil {
		return err
	}
	var text, dryRun bool
	var filename string
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.BoolVar(&text, "text", false, "Plain text output")
		flags.StringVar(&filename, "output", "", "PDF output filename")
		flags.BoolVar(&dryRun, "dry-run", false, "Preview saving a PDF")
	}, func() error {
		if text && filename != "" {
			return usageError("Use either --text or --output, not both.")
		}
		if dryRun && filename == "" {
			return usageError("--dry-run requires --output FILE.pdf on documents show.")
		}
		return nil
	}); err != nil {
		return err
	}
	if text && call.output.mode != "" {
		return usageError("Use either --text or --" + call.output.mode + ", not both.")
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		return readDocument(ctx, client, call.output, id, filename, text, dryRun)
	})
}

func uploadDocument(call *invocation) error {
	filename, err := call.takeArgument("Expected a filename before options.")
	if err != nil {
		return err
	}
	var dryRun bool
	if _, err := call.parse(func(flags *flag.FlagSet) {
		flags.BoolVar(&dryRun, "dry-run", false, "Check the local file without uploading")
	}, nil); err != nil {
		return err
	}
	if dryRun {
		data, contentType, err := readUpload(filename)
		if err != nil {
			return err
		}
		return writeResponse(call.output, response{OK: true, DryRun: true, Summary: "Upload preview",
			Data: map[string]any{"filename": filepath.Base(filename), "content_type": contentType, "byte_size": len(data)}})
	}
	return call.withSession(func(ctx context.Context, client *client) error {
		upload, err := client.uploadFile(ctx, filename)
		if err != nil {
			return err
		}
		return success(call.output, upload.identifiers())
	})
}
