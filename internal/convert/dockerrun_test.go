package convert

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitWords(t *testing.T) {
	got, err := SplitWords(`docker run -e "A=b c" -e 'X=$Y' \
  --name web nginx:1.27`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "run", "-e", "A=b c", "-e", "X=$Y", "--name", "web", "nginx:1.27"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if _, err := SplitWords(`echo "open`); err == nil {
		t.Fatal("unclosed quote must fail")
	}
}

func TestDockerRunFull(t *testing.T) {
	r, err := DockerRun(`docker run -d --name=uptime --restart unless-stopped -p 3001:3001 -p8080:80 ` +
		`-v uptime-data:/app/data -v /var/run/docker.sock:/var/run/docker.sock:ro -e TZ=Europe/Prague ` +
		`--network proxy -dit louislam/uptime-kuma:2 node server.js`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Service != "uptime" {
		t.Fatalf("service = %q", r.Service)
	}
	for _, want := range []string{
		"image: louislam/uptime-kuma:2",
		"container_name: uptime",
		"restart: unless-stopped",
		"- 3001:3001",
		"- 8080:80",
		"- uptime-data:/app/data",
		"- /var/run/docker.sock:/var/run/docker.sock:ro",
		"TZ: Europe/Prague",
		"command: [node, server.js]",
		"tty: true",
		"stdin_open: true",
		"networks:\n  proxy:\n    external: true",
		"services:\n  uptime:\n    image:",
		"uptime-data: {}",
	} {
		if !strings.Contains(r.YAML, want) {
			t.Errorf("missing %q in:\n%s", want, r.YAML)
		}
	}
	if len(r.Warnings) != 1 { // the external network note
		t.Errorf("warnings = %q", r.Warnings)
	}
}

func TestDockerRunServiceNameFromImage(t *testing.T) {
	r, err := DockerRun(`docker container run ghcr.io/amir20/dozzle:v11.5@sha256:abc`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Service != "dozzle" {
		t.Fatalf("service = %q", r.Service)
	}
}

func TestDockerRunErrors(t *testing.T) {
	for _, cmd := range []string{"ls -la", "docker run -d", "docker run --name"} {
		if _, err := DockerRun(cmd); err == nil {
			t.Errorf("%q should fail", cmd)
		}
	}
}

func TestDockerRunWarnsUnsupported(t *testing.T) {
	r, err := DockerRun(`docker run --gpus all --link db:db --frobnicate nginx`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 3 {
		t.Fatalf("want 3 warnings, got %q", r.Warnings)
	}
}
