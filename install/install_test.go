package install

import (
	"strings"
	"testing"
)

// The release next to each agent version has a script with no address of
// its own, which needs LADDER_URL and reads the manifest from
// LADDER_INSTALL_URL, and a manifest with the image by digest.
func TestRelease(t *testing.T) {
	image := "ghcr.io/k8-ladder/ladder-agent@sha256:" + strings.Repeat("a", 64)
	p := Params{Image: image, PullPolicy: "IfNotPresent"}
	script, err := File("install.sh", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`LADDER_URL="${LADDER_URL:-}"`, `INSTALL_URL="${LADDER_INSTALL_URL:-$LADDER_URL/install}"`, `[ -n "$LADDER_URL" ] || fail`, `curl -fsSL "$INSTALL_URL/agent.yaml"`} {
		if !strings.Contains(string(script), want) {
			t.Errorf("release script lacks %q", want)
		}
	}
	manifest, err := File("agent.yaml", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "image: "+image) {
		t.Errorf("release manifest image:\n%s", manifest)
	}
	for _, b := range [][]byte{script, manifest} {
		if strings.Contains(string(b), "{{") || strings.Contains(string(b), "<no value>") {
			t.Error("not fully rendered")
		}
	}
}

// A cloud's files name its address, and the manifest its image, the pull
// policy and the binding to cluster-admin for setup.
func TestCloud(t *testing.T) {
	p := Params{PublicURL: "https://ladder.example.com", Image: "ladder-agent:20261124112235", PullPolicy: "IfNotPresent"}
	script, err := File("install.sh", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `LADDER_URL="${LADDER_URL:-https://ladder.example.com}"`) {
		t.Errorf("script:\n%s", script)
	}
	manifest, err := File("agent.yaml", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"image: ladder-agent:20261124112235", "imagePullPolicy: IfNotPresent", "name: cluster-admin"} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("manifest lacks %q", want)
		}
	}
	if _, err := File("other.sh", p); err == nil {
		t.Error("an unknown file renders")
	}
}
