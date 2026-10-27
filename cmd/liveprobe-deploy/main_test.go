package main

import (
	"os"
	"strings"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	for in, want := range map[string]string{"": "latest", "  ": "latest", "1.0.0": "1.0.0", " v2 ": "v2"} {
		if got, err := resolveVersion(in); err != nil || got != want {
			t.Errorf("resolveVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"liveprobe:1.0.0", "repo/x", "a:b", "liveprobe-1"} {
		if _, err := resolveVersion(in); err == nil {
			t.Errorf("resolveVersion(%q) should be rejected", in)
		}
	}
}

func TestValidateRemoteDir(t *testing.T) {
	for _, ok := range []string{"/var/www/liveprobe", "apps/liveprobe", "/opt/lp-1.2_x@a,b=c+d%e:f"} {
		if err := validateRemoteDir(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/a b", "/a;rm -rf /", "/a$(x)", "/a'b", "~/apps" /* shell-expanded */, "/a\nb"} {
		if validateRemoteDir(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPrepareRemoteComposeSameLine(t *testing.T) {
	in := "services:\n  liveprobe:\n    build: .\n    ports:\n      - \"4319:4319\"\n"
	want := "services:\n  liveprobe:\n    image: liveprobe:1.0\n    ports:\n      - \"4319:4319\"\n"
	if got := prepareRemoteCompose(in, "liveprobe:1.0"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestPrepareRemoteComposeBlock(t *testing.T) {
	in := "services:\n  a:\n    build:\n      context: .\n      dockerfile: Dockerfile\n    restart: always\n"
	want := "services:\n  a:\n    image: lp:x\n    restart: always\n"
	if got := prepareRemoteCompose(in, "lp:x"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// The repo's own compose file must come out with no build keys and every built service pinned.
func TestPrepareRemoteComposeRepoFile(t *testing.T) {
	raw, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	got := prepareRemoteCompose(string(raw), "liveprobe:9")
	if strings.Contains(got, "build:") {
		t.Errorf("build: survived:\n%s", got)
	}
	if n := strings.Count(got, "image: liveprobe:9"); n != 2 {
		t.Errorf("pinned %d services, want 2 (liveprobe, collector)", n)
	}
	if !strings.Contains(got, "image: otel/opentelemetry-collector:0.115.1") {
		t.Error("third-party image changed")
	}
}
