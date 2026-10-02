package appscan

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestClassify(t *testing.T) {
	yes := map[string]string{
		"/home/k/Downloads/firefox/firefox": "Firefox", "firefox-bin": "Firefox", "/app/lib/firefox/firefox": "Firefox",
		"start-tor-browser": "Tor Browser", "Brave-Browser": "Brave", "FreeTube": "FreeTube (YouTube app)",
		"yt-dlp": "yt-dlp (YouTube downloader)", "LibreWolf-128.0.AppImage": "LibreWolf (AppImage)",
	}
	for in, want := range yes {
		if got, ok := Classify(in); !ok || got != want {
			t.Errorf("Classify(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, no := range []string{"bash", "chrome_crashpad_handler", "chrome-sandbox", "Game.AppImage", "torrent"} {
		if got, ok := Classify(no); ok {
			t.Errorf("Classify(%q) = %q, want no match", no, got)
		}
	}
}

func writeProc(t *testing.T, root string, pid, uid int, cmdline, comm string) {
	dir := filepath.Join(root, itoa(pid))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\t"+comm+"\nUid:\t"+itoa(uid)+"\t"+itoa(uid)+"\t"+itoa(uid)+"\t"+itoa(uid)+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644)
	os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestProcs(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 10, 1001, "/opt/google/chrome/chrome\x00--type=renderer\x00", "chrome")
	writeProc(t, root, 11, 1001, "/home/kid/Downloads/firefox/firefox\x00", "firefox-bin")
	writeProc(t, root, 12, 1001, "/home/kid/Downloads/firefox/firefox\x00-contentproc\x00", "Isolated Web Co")
	writeProc(t, root, 13, 1000, "/usr/lib/firefox/firefox\x00", "firefox") // parent: not a kid
	writeProc(t, root, 14, 1001, "/tmp/.mount_FreeTuXYZ/freetube\x00", "freetube")
	writeProc(t, root, 15, 1001, "bash\x00", "bash")
	got := Procs(root, map[int]bool{1001: true})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, f := range got {
		if f.UID != 1001 || f.Kind != KindRunning {
			t.Errorf("%+v", f)
		}
	}
	if got[0].How != "portable copy in a user folder" && got[1].How != "portable copy in a user folder" {
		t.Errorf("location: %+v", got)
	}
}

func TestHome(t *testing.T) {
	home := t.TempDir()
	mk := func(rel string, data []byte, mode os.FileMode) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1}
	mk("Downloads/firefox/firefox", elf, 0o755)                   // portable browser
	mk("Downloads/firefox/firefox-notes.txt", []byte("x"), 0o644) // not a program
	mk("Music/firefox", []byte("not elf"), 0o755)                 // not a program
	mk("Desktop/tor-browser-linux-x86_64-14.0.tar.xz", []byte("x"), 0o644)
	mk("Apps/LibreWolf-128.AppImage", elf, 0o755)
	mk(".local/share/flatpak/app/org.mozilla.firefox/current", []byte("x"), 0o644)
	mk(".local/share/flatpak/app/org.gnome.Calculator/current", []byte("x"), 0o644)
	mk(".config/google-chrome/Default/chrome", elf, 0o755) // managed Chrome's profile: skipped
	os.Symlink("/usr/bin", filepath.Join(home, "linked"))  // not followed
	got := Home(home, 1001)
	want := map[string]bool{
		filepath.Join(home, "Downloads/firefox/firefox"):                    true,
		filepath.Join(home, "Desktop/tor-browser-linux-x86_64-14.0.tar.xz"): true,
		filepath.Join(home, "Apps/LibreWolf-128.AppImage"):                  true,
		"org.mozilla.firefox": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings: %+v", len(got), got)
	}
	for _, f := range got {
		if !want[f.Where] {
			t.Errorf("unexpected %+v", f)
		}
	}
}
