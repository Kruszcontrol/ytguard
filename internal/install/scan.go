package install

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"time"

	"ytguard/internal/appscan"
	"ytguard/internal/core"
	"ytguard/internal/store"
)

// Scan looks for other browsers and video apps in kids' home folders and
// system-wide, and records them for the daemon to report. Run as root
// (hourly, by ytguard-scan.timer) so it can read kids' home folders.
func Scan(dataDir string, verbose bool) error {
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer func() { st.Close(); FixOwnership(dataDir) }()
	s, err := st.Settings()
	if err != nil {
		return err
	}
	if !s.AppScan {
		if verbose {
			fmt.Println("Looking for other browsers is turned off in Settings.")
		}
		return nil
	}
	kids, err := st.Kids()
	if err != nil {
		return err
	}
	var found []appscan.Finding
	for _, k := range kids {
		u, err := user.Lookup(k.LinuxUser)
		if err != nil {
			continue
		}
		uid, _ := strconv.Atoi(u.Uid)
		if _, err := os.Stat(u.HomeDir); err != nil {
			continue
		}
		found = append(found, appscan.Home(u.HomeDir, uid)...)
	}
	found = append(found, appscan.System()...)
	if err := core.RecordFindings(st, found, time.Now()); err != nil {
		return err
	}
	if verbose {
		if len(found) == 0 {
			fmt.Println("No other browsers or video apps found.")
		}
		for _, f := range found {
			who := "everyone"
			for _, k := range kids {
				if u, err := user.Lookup(k.LinuxUser); err == nil && u.Uid == strconv.Itoa(f.UID) {
					who = k.Name
				}
			}
			fmt.Printf("%-10s %-28s %-38s %s\n", who, f.App, f.How, f.Where)
		}
	}
	return nil
}

// ScanUnits are the systemd service and timer for the hourly root scan.
func ScanUnits() (service, timer string) {
	service = `[Unit]
Description=YTGuard: look for other browsers and video apps
After=ytguard.service

[Service]
Type=oneshot
ExecStart=` + BinPath + ` scan --data ` + DataDir + ` --quiet
Nice=10
IOSchedulingClass=idle
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=` + DataDir + `
PrivateTmp=yes
PrivateDevices=yes
PrivateNetwork=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
CapabilityBoundingSet=CAP_DAC_READ_SEARCH CAP_DAC_OVERRIDE CAP_CHOWN CAP_FOWNER
`
	timer = `[Unit]
Description=YTGuard: hourly scan for other browsers and video apps

[Timer]
OnBootSec=5min
OnUnitActiveSec=1h
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
`
	return
}
