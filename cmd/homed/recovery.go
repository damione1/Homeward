package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
)

// runRecovery is deliberately offline and runs before server/startup admission.
// It never renders a key, an argument, a path or an upstream error.
func runRecovery(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	return runRecoveryWithRestore(ctx, args, out, diagnostics, restoreOffline)
}

type offlineRestore func(context.Context, io.Reader, backups.ArtifactID, backuparchive.RecoveryPrivate, string, string, backuparchive.Limits) (backuparchive.RestoreState, error)
type recoveryIndeterminateError struct{}

func (recoveryIndeterminateError) Error() string { return "recovery publication indeterminate" }

type recoveryCompletion uint8

const (
	recoveryNotCompleted recoveryCompletion = iota
	recoveryRestoreCompleted
	recoveryExportCompleted
	recoveryCopyCompleted
	recoveryVerificationCompleted
)

// Diagnostics are best effort. A broken or panicking diagnostic writer must
// never replace an already-established operation result with a generic failure.
func recoveryDiagnostic(writer io.Writer, message string) {
	defer func() { _ = recover() }()
	if !homedMissingWriter(writer) {
		_, _ = fmt.Fprintln(writer, message)
	}
}
func recoveryCompletedOutputUnavailable(writer io.Writer, completion recoveryCompletion) {
	switch completion {
	case recoveryRestoreCompleted:
		recoveryDiagnostic(writer, "recovery completed durably; preserve the complete destination and inspect it; success output unavailable")
	case recoveryExportCompleted, recoveryCopyCompleted:
		recoveryDiagnostic(writer, "recovery export completed; preserve the exported files and verify the retained copy; success output unavailable")
	case recoveryVerificationCompleted:
		recoveryDiagnostic(writer, "recovery verification completed; success output unavailable")
	case recoveryNotCompleted:
		recoveryDiagnostic(writer, "recovery output unavailable")
	}
}
func restoreOffline(ctx context.Context, source io.Reader, id backups.ArtifactID, identity backuparchive.RecoveryPrivate, parent, destination string, limits backuparchive.Limits) (backuparchive.RestoreState, error) {
	result, err := backuparchive.Recover(ctx, source, id, identity, parent, destination, limits)
	return result.State(), err
}
func runRecoveryWithRestore(ctx context.Context, args []string, out, diagnostics io.Writer, restore offlineRestore) (code int) {
	completion := recoveryNotCompleted
	defer func() {
		if recover() != nil {
			if completion != recoveryNotCompleted {
				recoveryCompletedOutputUnavailable(diagnostics, completion)
				code = 4
			} else {
				recoveryDiagnostic(diagnostics, "recovery failed")
				code = 1
			}
		}
	}()
	if ctx == nil || homedMissingWriter(out) || homedMissingWriter(diagnostics) || restore == nil {
		return 2
	}
	failure := func(code int) int { recoveryDiagnostic(diagnostics, "recovery failed"); return code }
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: homed recovery export|copy|verify|restore [options]\nRecovery operations are offline. Keep private exports outside the server data directory and outside Git, notes, logs and audit. Use a private directory on separate operator storage; verify the retained copy regularly. Restore publishes a new directory only; it never starts the server or imports a database.")
		if err != nil {
			return 1
		}
		return 0
	}
	flags := flag.NewFlagSet("recovery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var operation func() (string, error)
	var completedOperation recoveryCompletion
	switch args[0] {
	case "export":
		completedOperation = recoveryExportCompleted
		privateOutput := flags.String("private-output", "", "new private operator export file")
		publicOutput := flags.String("recipient-output", "", "new public recipient file")
		dataDir := flags.String("server-data-dir", "", "server data directory, excluded from private exports")
		operation = func() (string, error) { return backuparchive.ExportRecovery(*privateOutput, *publicOutput, *dataDir) }
	case "copy":
		completedOperation = recoveryCopyCompleted
		privateSource := flags.String("private-file", "", "retained operator copy")
		privateOutput := flags.String("private-output", "", "new private operator export file")
		dataDir := flags.String("server-data-dir", "", "server data directory, excluded from private exports")
		operation = func() (string, error) { return backuparchive.CopyRecovery(*privateSource, *privateOutput, *dataDir) }
	case "verify":
		completedOperation = recoveryVerificationCompleted
		privateFile := flags.String("private-file", "", "retained operator copy")
		publicFile := flags.String("recipient-file", "", "public recipient file")
		dataDir := flags.String("server-data-dir", "", "server data directory, excluded from private exports")
		operation = func() (string, error) {
			return backuparchive.VerifyRetainedRecovery(*privateFile, *publicFile, *dataDir)
		}
	case "restore":
		completedOperation = recoveryRestoreCompleted
		privateFile := flags.String("private-file", "", "retained operator copy")
		archiveFile := flags.String("archive", "", "exported raw age archive payload")
		artifact := flags.String("artifact-id", "", "stable archive identity")
		output := flags.String("output", "", "new restore directory under a private parent")
		operation = func() (string, error) {
			private, err := backuparchive.LoadRecoveryPrivate(*privateFile)
			if err != nil {
				return "", err
			}
			id, err := backups.ParseArtifactID(*artifact)
			if err != nil || !filepath.IsAbs(*output) || filepath.Clean(*output) != *output {
				return "", fmt.Errorf("invalid recovery input")
			}
			before, err := os.Lstat(*archiveFile)
			if err != nil || !before.Mode().IsRegular() {
				return "", fmt.Errorf("invalid archive input")
			}
			file, err := os.OpenFile(*archiveFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return "", err
			}
			defer file.Close()
			opened, err := file.Stat()
			if err != nil || !os.SameFile(before, opened) {
				return "", fmt.Errorf("invalid archive input")
			}
			state, err := restore(ctx, file, id, private, filepath.Dir(*output), filepath.Base(*output), backuparchive.DefaultLimits())
			if state == backuparchive.RestoreIndeterminate {
				return "", recoveryIndeterminateError{}
			}
			if err != nil || state != backuparchive.Restored {
				return "", fmt.Errorf("archive recovery failed")
			}
			completion = recoveryRestoreCompleted
			return "restored", nil
		}
	default:
		return failure(2)
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return failure(2)
	}
	if ctx.Err() != nil {
		return failure(1)
	}
	result, err := operation()
	if err != nil {
		var indeterminate recoveryIndeterminateError
		if errors.As(err, &indeterminate) {
			recoveryDiagnostic(diagnostics, "recovery indeterminate: a complete destination may already be published; preserve it and inspect before retrying")
			return 3
		}
		return failure(1)
	}
	completion = completedOperation
	if _, err := fmt.Fprintln(out, "recovery", result); err != nil {
		recoveryCompletedOutputUnavailable(diagnostics, completion)
		return 4
	}
	return 0
}
