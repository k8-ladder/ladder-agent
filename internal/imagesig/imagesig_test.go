package imagesig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const workflow = "https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/main"

func statement(digestHex, predicate string) []byte {
	return fmt.Appendf(nil, `{"_type":"https://in-toto.io/Statement/v1","subject":[{"digest":{"sha256":%q}}],"predicateType":%q,"predicate":{}}`, digestHex, predicate)
}

var policy = Policy{Issuer: DefaultIssuer, Signer: DefaultSigner}

// A statement of cosign sign about the image's digest, signed by the release
// workflow, passes; another signer, another digest or another statement does
// not.
func TestCheck(t *testing.T) {
	// The virtual Sigstore has no certificate transparency log; Fulcio's
	// certificates carry their timestamps.
	defer func(o []verify.VerifierOption) { checkOptions = o }(checkOptions)
	checkOptions = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	good, err := vs.Attest(workflow, DefaultIssuer, statement(digest, SignPredicate))
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(vs, policy, good, digest); err != nil {
		t.Fatalf("release signature: %v", err)
	}
	if err := Check(vs, policy, good, strings.Repeat("cd", 32)); err == nil {
		t.Error("a signature of another digest passes")
	}
	fork, _ := vs.Attest("https://github.com/someone/ladder-agent/.github/workflows/release.yml@refs/heads/main", DefaultIssuer, statement(digest, SignPredicate))
	if err := Check(vs, policy, fork, digest); err == nil {
		t.Error("another repository's workflow passes")
	}
	branch, _ := vs.Attest("https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/evil", DefaultIssuer, statement(digest, SignPredicate))
	if err := Check(vs, policy, branch, digest); err == nil {
		t.Error("another branch passes")
	}
	// The workflow that signed the agent before it had a repository of its own.
	monorepo, _ := vs.Attest("https://github.com/k8-ladder/ladder/.github/workflows/agent.yml@refs/heads/main", DefaultIssuer, statement(digest, SignPredicate))
	if err := Check(vs, policy, monorepo, digest); err == nil {
		t.Error("the monorepo's agent workflow passes")
	}
	other, _ := vs.Attest(workflow, DefaultIssuer, statement(digest, "https://slsa.dev/provenance/v1"))
	if err := Check(vs, policy, other, digest); err == nil {
		t.Error("another statement passes")
	}
	issuer, _ := vs.Attest(workflow, "https://accounts.google.com", statement(digest, SignPredicate))
	if err := Check(vs, policy, issuer, digest); err == nil {
		t.Error("another issuer passes")
	}
	stranger, _ := ca.NewVirtualSigstore()
	if err := Check(stranger, policy, good, digest); err == nil {
		t.Error("a signature from another Sigstore passes")
	}
}

func testRegistry(t *testing.T) (string, []remote.Option, []name.Option) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), []remote.Option{remote.WithTransport(srv.Client().Transport)}, []name.Option{name.Insecure}
}

// Pin resolves the tag to a digest and reads the bundles that refer to it;
// an image without any is refused, and the policy off keeps the tag.
func TestPinAndBundles(t *testing.T) {
	ctx := context.Background()
	host, ropts, nopts := testRegistry(t)
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := name.ParseReference(host+"/ladder-agent:20261123", nopts...)
	if err := remote.Write(tag, img, ropts...); err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Policy: policy, Remote: ropts, Name: nopts,
		Trusted: func(context.Context) (root.TrustedMaterial, error) { return nil, errors.New("not reached") }}
	if _, err := v.Pin(ctx, tag.String()); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("unsigned image: %v", err)
	}
	off := &Verifier{Policy: Policy{Off: true}}
	if got, err := off.Pin(ctx, "ladder-agent:dev"); err != nil || got != "ladder-agent:dev" {
		t.Fatalf("policy off: %q, %v", got, err)
	}

	// A referrer of cosign's kind, with its bundle as the layer.
	d, _ := img.Digest()
	pushReferrer(t, tag.Context(), d, mustSize(t, img), []byte(`{"mediaType":"bundle"}`), ropts)
	raws, err := Bundles(tag.Context().Digest(d.String()), ropts...)
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 1 || string(raws[0]) != `{"mediaType":"bundle"}` {
		t.Fatalf("bundles = %q", raws)
	}
	// A bundle that does not verify is no signature.
	if _, err := v.Pin(ctx, tag.String()); err == nil || errors.Is(err, ErrUnsigned) {
		t.Fatalf("bad bundle: %v", err)
	}
}

// rawManifest is a manifest as cosign writes a referrer: with an artifact
// type, which go-containerregistry does not set.
type rawManifest struct{ raw []byte }

func (m rawManifest) RawManifest() ([]byte, error) { return m.raw, nil }

func pushReferrer(t *testing.T, repo name.Repository, subject v1.Hash, size int64, bundleJSON []byte, opts []remote.Option) {
	t.Helper()
	layer := static.NewLayer(bundleJSON, types.MediaType(BundleMediaType))
	config := static.NewLayer([]byte("{}"), "application/vnd.oci.empty.v1+json")
	desc := func(l v1.Layer) map[string]any {
		d, _ := l.Digest()
		n, _ := l.Size()
		mt, _ := l.MediaType()
		return map[string]any{"mediaType": string(mt), "digest": d.String(), "size": n}
	}
	for _, l := range []v1.Layer{layer, config} {
		if err := remote.WriteLayer(repo, l, opts...); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": string(types.OCIManifestSchema1), "artifactType": BundleMediaType,
		"config": desc(config), "layers": []any{desc(layer)},
		"subject": map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": subject.String(), "size": size},
	})
	h, _, _ := v1.SHA256(bytes.NewReader(raw))
	if err := remote.Put(repo.Digest(h.String()), rawManifest{raw}, opts...); err != nil {
		t.Fatal(err)
	}
}

func mustSize(t *testing.T, img v1.Image) int64 {
	t.Helper()
	n, err := img.Size()
	if err != nil {
		t.Fatal(err)
	}
	return n
}
