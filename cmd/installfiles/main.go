// Command installfiles writes the release of install.sh and the agent's
// manifest that the agent workflow publishes next to each agent version
// (spec §5.3): the script with no address of its own, which takes LADDER_URL,
// and the manifest with the image by digest. People who read them from the
// release, rather than from a cloud, run what the workflow built whatever
// that cloud serves.
//
//	go run ./cmd/installfiles -image ghcr.io/k8-ladder/ladder-agent@sha256:… -out dist
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"ladder-be/internal/api"
)

var digestImage = regexp.MustCompile(`^[a-z0-9.:/_-]+@sha256:[0-9a-f]{64}$`)

func main() {
	image := flag.String("image", "", "the agent's image, by digest")
	out := flag.String("out", "dist", "directory to write install.sh and agent.yaml into")
	flag.Parse()
	if !digestImage.MatchString(*image) {
		log.Fatalf("-image %q is not an image by digest", *image)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, f := range []struct {
		name string
		mode os.FileMode
	}{{"install.sh", 0o755}, {"agent.yaml", 0o644}} {
		b, err := api.InstallFile(f.name, "", *image, "IfNotPresent")
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, f.name), b, f.mode); err != nil {
			log.Fatal(err)
		}
	}
}
