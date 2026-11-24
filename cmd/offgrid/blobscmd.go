package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"offgrid/internal/blobstore"
	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

// The attachment commands of "offgrid store" (Days 47 and 48):
//
//	offgrid store -db F attach -task T3 -file photo.jpg [-type image/jpeg] [-name pump.jpg]
//	offgrid store -db F attachments -task T3     list a task's files and whether their bytes are on this device
//	offgrid store -db F blobs                    what the blob folder holds against what the log says
//	offgrid store -db F gc [-grace 168h] [-dry-run]  remove blobs no op names, once older than the grace period

// extTypes are the file extensions attach understands when -type is not given.
var extTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif",
	".heic": "image/heic", ".heif": "image/heif", ".pdf": "application/pdf", ".txt": "text/plain", ".mp4": "video/mp4",
}

func storeBlobCmd(ctx context.Context, sub string, st *sqlstore.Store, taskRef, file, mediaType, name string, grace time.Duration, dry bool, stdout, stderr io.Writer) int {
	switch sub {
	case "attach", "attachments":
		if taskRef == "" {
			fmt.Fprintf(stderr, "usage: offgrid store -db FILE %s -task T3%s\nerror: -task is required\n", sub, map[bool]string{true: " -file PATH [-type TYPE] [-name NAME]", false: ""}[sub == "attach"])
			return 2
		}
		all, err := st.List(ctx, task.Filter{})
		if err != nil {
			return storeError(stderr, err)
		}
		tk, err := task.Resolve(all, taskRef)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if sub == "attachments" {
			list, err := st.Attachments(ctx, tk.ID)
			if err != nil {
				return storeError(stderr, err)
			}
			if len(list) == 0 {
				fmt.Fprintf(stdout, "%s has no attachments\n", task.ShortID(tk.ID))
			}
			for _, a := range list {
				here := "bytes here"
				if !a.Present {
					here = "NOT on this device yet"
				}
				fmt.Fprintf(stdout, "%s  %-16s %9d bytes  %s  %s  %s\n", a.Hash[:12], a.Type, a.Size, a.At.UTC().Format("2006-01-02 15:04"), here, a.Name)
			}
			return 0
		}
		return attachFile(ctx, st, tk, file, mediaType, name, stdout, stderr)
	case "blobs":
		s, err := st.BlobStats(ctx)
		if err != nil {
			return storeError(stderr, err)
		}
		fmt.Fprintf(stdout, "blob folder        %d files, %s\n", s.Blobs, mb(s.Bytes))
		fmt.Fprintf(stdout, "named by the log   %d distinct files\n", s.Referenced)
		fmt.Fprintf(stdout, "named, not here    %d (%s) arrive when this device syncs with one that has them\n", s.Missing, mb(s.MissingBytes))
		fmt.Fprintf(stdout, "named by no op     %d (%s) collected by: offgrid store -db FILE gc\n", s.Orphans, mb(s.OrphanBytes))
		if bs, err := st.Blobs(); err == nil {
			if free, ok := bs.Free(); ok {
				fmt.Fprintf(stdout, "free on this disk  %s (new files keep %s in reserve)\n", mb(int64(free)), mb(blobstore.DefaultReserve))
			}
		}
		return 0
	case "gc":
		res, err := st.CollectBlobs(ctx, grace, dry)
		switch {
		case errors.Is(err, sqlstore.ErrGraceTooShort):
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		case err != nil:
			return storeError(stderr, err)
		}
		verb := "removed"
		if res.DryRun {
			verb = "would remove"
		}
		fmt.Fprintf(stdout, "%d files looked at: %d named by the log (kept), %d unnamed but newer than %s (kept), %s %d (%s)\n",
			res.Scanned, res.Referenced, res.Young, grace, verb, res.Removed, mb(res.Freed))
		return 0
	}
	return 2
}

func mb(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
}

func attachFile(ctx context.Context, st *sqlstore.Store, tk task.Task, file, mediaType, name string, stdout, stderr io.Writer) int {
	if file == "" {
		fmt.Fprintln(stderr, "usage: offgrid store -db FILE attach -task T3 -file PATH [-type TYPE] [-name NAME]\nerror: -file is required")
		return 2
	}
	if mediaType == "" {
		mediaType = extTypes[strings.ToLower(filepath.Ext(file))]
		if mediaType == "" {
			fmt.Fprintf(stderr, "error: cannot tell the type of %q from its extension: give -type (for example image/jpeg)\n", file)
			return 2
		}
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		fmt.Fprintf(stderr, "error: %q is not a media type\n", mediaType)
		return 2
	}
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		fmt.Fprintf(stderr, "error: %q is not a regular file\n", file)
		return 1
	}
	if name == "" {
		name = filepath.Base(file)
	}
	a, err := st.AddAttachment(ctx, tk.ID, f, fi.Size(), mediaType, name)
	switch {
	case errors.Is(err, blobstore.ErrNoSpace):
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	case errors.Is(err, blobstore.ErrTooLarge):
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	case errors.Is(err, sqlstore.ErrBadAttachment), errors.Is(err, blobstore.ErrEmpty):
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	case err != nil:
		return storeError(stderr, err)
	}
	fmt.Fprintf(stdout, "attached %s (%d bytes, %s) to %s; hash %s\n", a.Name, a.Size, a.Type, task.ShortID(tk.ID), a.Hash)
	return 0
}
