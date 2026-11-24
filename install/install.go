// Package install renders install.sh, which installs the agent into the
// cluster of the current kubectl context, and agent.yaml, the agent's
// manifest that install.sh applies.
//
// The release workflow renders both with no address, for the release next to
// each agent version (cmd/installfiles): that install.sh needs LADDER_URL and
// reads agent.yaml, with the image by digest, from LADDER_INSTALL_URL. A Ladder
// cloud that runs an agent image of its own, as on a laptop with k3d, renders
// them with its address and that image and serves them itself.
package install

import (
	"bytes"
	"embed"
	"text/template"
)

//go:embed install.sh agent.yaml
var files embed.FS

var tmpl = template.Must(template.New("").Option("missingkey=error").ParseFS(files, "install.sh", "agent.yaml"))

// Names are the files File renders.
var Names = []string{"install.sh", "agent.yaml"}

// Params fill the templates.
type Params struct {
	// PublicURL is the address of the Ladder cloud that serves the files.
	// Empty is the release: install.sh then needs LADDER_URL.
	PublicURL string
	// Image is the agent's image; by digest in a release.
	Image string
	// PullPolicy is the image's imagePullPolicy.
	PullPolicy string
}

// File renders install.sh or agent.yaml.
func File(name string, p Params) ([]byte, error) {
	var buf bytes.Buffer
	err := tmpl.ExecuteTemplate(&buf, name, p)
	return buf.Bytes(), err
}
