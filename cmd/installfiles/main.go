// Command installfiles writes the release of install.sh and the agent's
// manifest that the release workflow publishes next to each agent version:
// the script with no address of its own, which takes LADDER_URL, and the
// manifest with the image by digest. People who read them from the release,
// rather than from a Ladder cloud, run what the workflow built whatever that
// cloud serves.
//
//	go run ./cmd/installfiles -image ghcr.io/k8-ladder/ladder-agent@sha256:… -out dist
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/k8-ladder/ladder-agent/install"
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
	for _, name := range install.Names {
		b, err := install.File(name, install.Params{Image: *image, PullPolicy: "IfNotPresent"})
		if err != nil {
			log.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(name) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(*out, name), b, mode); err != nil {
			log.Fatal(err)
		}
	}
}
