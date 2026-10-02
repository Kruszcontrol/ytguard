package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"ytguard"
)

func fakeGitHub(t *testing.T, tag string, binary []byte, sum string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/ytguard/releases/latest":
			json.NewEncoder(w).Encode(Release{Tag: tag, URL: "https://example/rel", Notes: "notes", Assets: []Asset{
				{Name: BinaryName(), URL: srv.URL + "/bin"}, {Name: "SHA256SUMS", URL: srv.URL + "/sums"}}})
		case "/bin":
			w.Write(binary)
		case "/sums":
			fmt.Fprintf(w, "%s  %s\n%s  other\n", sum, BinaryName(), sum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })
	return srv
}

func TestCheckAndNotifyOnce(t *testing.T) {
	ytguard.Version = "v1.0.0"
	fakeGitHub(t, "v1.1.0", nil, "")
	notified := 0
	c := &Checker{Repo: "o/ytguard", Enabled: func() bool { return true }, OnNew: func(Status) { notified++ }}
	st := c.Check(context.Background())
	if !st.Available || st.Latest != "v1.1.0" || st.Current != "v1.0.0" {
		t.Fatalf("%+v", st)
	}
	c.Check(context.Background())
	if notified != 1 {
		t.Fatalf("notified %d times", notified)
	}
	ytguard.Version = "v1.1.0"
	if c.Status().Available {
		t.Fatal("same version reported as update")
	}
}

func TestDownloadVerifies(t *testing.T) {
	bin := []byte("#!/bin/sh\necho hi\n")
	h := sha256.Sum256(bin)
	fakeGitHub(t, "v9.0.0", bin, hex.EncodeToString(h[:]))
	r, err := Latest(context.Background(), "o/ytguard")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Download(context.Background(), r, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(bin) {
		t.Fatal("content mismatch")
	}

	fakeGitHub(t, "v9.0.0", bin, "0000000000000000000000000000000000000000000000000000000000000000")
	r, _ = Latest(context.Background(), "o/ytguard")
	if _, err := Download(context.Background(), r, t.TempDir()); err == nil {
		t.Fatal("bad checksum accepted")
	}
}
