package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"ytguard"
	"ytguard/internal/update"
)

// UpgradeOptions for Upgrade.
type UpgradeOptions struct {
	File  string // install this binary instead of downloading
	Check bool   // only report whether an update exists
	Yes   bool   // don't ask for confirmation
}

// InstalledVersion asks the installed binary for its version.
func InstalledVersion() string {
	out, err := exec.Command(BinPath, "version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// Upgrade downloads (or takes) a new ytguard, verifies it, keeps the old
// one as a backup and reinstalls with the previous settings.
func Upgrade(ctx context.Context, o UpgradeOptions) error {
	if os.Geteuid() != 0 && !o.Check {
		return errors.New("run with sudo: sudo ytguard upgrade")
	}
	current := InstalledVersion()
	if o.Check && current == "" {
		current = ytguard.Version // not installed: compare this binary
	} else if _, err := SavedOptions(); err != nil {
		return err
	}
	if InstalledVersion() == "" {
		fmt.Printf("This binary:       %s (not installed)\n", orUnknown(current))
	} else {
		fmt.Printf("Installed version: %s\n", orUnknown(current))
	}

	newBin := o.File
	if newBin == "" {
		if ytguard.Repo == "" {
			return errors.New("this build doesn't know its GitHub repository; download a release yourself and run: sudo ytguard upgrade --file ./ytguard")
		}
		rel, err := update.Latest(ctx, ytguard.Repo)
		if err != nil {
			return err
		}
		fmt.Printf("Latest release:    %s  (%s)\n", rel.Tag, rel.URL)
		if !ytguard.Newer(rel.Tag, current) {
			fmt.Println("Already up to date.")
			return nil
		}
		if o.Check {
			fmt.Println("An update is available. Install it with: sudo ytguard upgrade")
			return nil
		}
		if notes := strings.TrimSpace(rel.Notes); notes != "" {
			fmt.Printf("\nWhat's new in %s:\n%s\n\n", rel.Tag, indent(notes))
		}
		if !o.Yes && !NewPrompter().YesNo(fmt.Sprintf("Upgrade %s from %s to %s?", hostname(), orUnknown(current), rel.Tag), true) {
			return errors.New("cancelled")
		}
		dir, err := os.MkdirTemp("", "ytguard-upgrade-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		fmt.Printf("Downloading %s… ", update.BinaryName())
		if newBin, err = update.Download(ctx, rel, dir); err != nil {
			fmt.Println()
			return err
		}
		fmt.Println("checksum OK.")
	} else if o.Check {
		return errors.New("--check can't be combined with --file")
	}

	// Sanity-check the new binary before touching anything.
	out, err := exec.Command(newBin, "version").Output()
	if err != nil || !strings.HasPrefix(string(out), "ytguard ") {
		return fmt.Errorf("%s doesn't look like a ytguard binary", newBin)
	}
	newVer := strings.TrimSpace(strings.TrimPrefix(string(out), "ytguard "))
	if o.File != "" && !o.Yes && !NewPrompter().YesNo(fmt.Sprintf("Install %s (version %s) over %s?", o.File, newVer, orUnknown(current)), true) {
		return errors.New("cancelled")
	}

	backup := PrevBinPath
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return err
	}
	if err := copyFile(BinPath, backup, 0o755); err != nil {
		return fmt.Errorf("back up current binary: %w", err)
	}
	fmt.Printf("Previous version saved as %s\n", backup)

	// Let the new binary do the install so its own install logic applies.
	cmd := exec.CommandContext(ctx, newBin, "install", "--upgrade")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install failed: %w\nTo go back: sudo %s install --upgrade", err, backup)
	}
	fmt.Printf("\nUpgraded %s → %s.\n", orUnknown(current), newVer)
	fmt.Printf("If something's wrong, go back with: sudo %s install --upgrade\n", backup)
	fmt.Printf("(If the new version changed the database, restore the matching %s/ytguard.db.backup-* file too.)\n", DataDir)
	return nil
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
