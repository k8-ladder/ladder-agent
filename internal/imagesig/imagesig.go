// Package imagesig checks that an agent image was signed by Ladder's release
// workflow before the agent runs it (spec §5.9). The workflow signs each image
// with cosign, keyless: Sigstore's Fulcio gives the workflow's GitHub identity
// a certificate, Rekor logs the signature, and the signature is a Sigstore
// bundle stored next to the image as an OCI referrer. The agent resolves the
// tag to a digest, checks a bundle of that digest against Sigstore's trusted
// root and the workflow's identity, and runs the image by digest: a cloud, or
// whoever pushes to the image repository, cannot make it run an image the
// workflow did not build.
package imagesig

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const (
	// BundleMediaType is the artifact type of cosign's signature bundles.
	BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	// SignPredicate is the predicate type of cosign sign's in-toto statement,
	// whose subject is the image's digest.
	SignPredicate = "https://sigstore.dev/cosign/sign/v1"

	emptyMediaType = "application/vnd.oci.empty.v1+json"

	// DefaultIssuer is GitHub Actions' OIDC issuer.
	DefaultIssuer = "https://token.actions.githubusercontent.com"
	// DefaultSigner is the workflow that builds and signs the agent.
	DefaultSigner = `^https://github\.com/k8-ladder/ladder/\.github/workflows/agent\.yml@refs/heads/main$`
)

// ErrUnsigned means the image has no signature bundle at all.
var ErrUnsigned = errors.New("the image has no signature")

// Policy says which signatures an agent image must carry.
type Policy struct {
	// Off runs images by tag, unchecked: for images in no registry, such as
	// those install-local.sh imports into k3d.
	Off bool
	// Issuer and Signer, a regular expression, are the OIDC issuer and the
	// identity (the workflow's URL) of the signing certificate.
	Issuer, Signer string
}

// Verifier pins and checks agent images.
type Verifier struct {
	Policy Policy
	// Trusted returns the trusted root; nil fetches Sigstore's public-good
	// root through TUF, cached in CachePath.
	Trusted   func(ctx context.Context) (root.TrustedMaterial, error)
	CachePath string
	Remote    []remote.Option
	Name      []name.Option
}

// Pin resolves image, repository:tag, to its digest and checks a signature
// of that digest by the policy's signer. It returns repository@digest, or
// image as it is with the policy off.
func (v *Verifier) Pin(ctx context.Context, image string) (string, error) {
	if v.Policy.Off {
		return image, nil
	}
	ref, err := name.ParseReference(image, v.Name...)
	if err != nil {
		return "", err
	}
	opts := append([]remote.Option{remote.WithContext(ctx)}, v.Remote...)
	desc, err := remote.Head(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", image, err)
	}
	digest := ref.Context().Digest(desc.Digest.String())
	raws, err := Bundles(digest, opts...)
	if err != nil {
		return "", fmt.Errorf("read the signatures of %s: %w", digest, err)
	}
	if len(raws) == 0 {
		return "", fmt.Errorf("%s: %w", digest, ErrUnsigned)
	}
	tm, err := v.trusted(ctx)
	if err != nil {
		return "", fmt.Errorf("Sigstore's trusted root: %w", err)
	}
	var errs []error
	for _, raw := range raws {
		b := &bundle.Bundle{}
		if err := b.UnmarshalJSON(raw); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := Check(tm, v.Policy, b, desc.Digest.Hex); err != nil {
			errs = append(errs, err)
			continue
		}
		return digest.String(), nil
	}
	return "", fmt.Errorf("no signature of %s by %s: %w", digest, v.Policy.Signer, errors.Join(errs...))
}

func (v *Verifier) trusted(ctx context.Context) (root.TrustedMaterial, error) {
	if v.Trusted != nil {
		return v.Trusted(ctx)
	}
	opts := tuf.DefaultOptions().WithContext(ctx)
	if v.CachePath != "" {
		opts = opts.WithCachePath(v.CachePath)
	}
	return root.FetchTrustedRootWithOptions(opts)
}

// Bundles reads the signature bundles that refer to digest: the layers of
// bundle type of its referrers. Registries name a referrer's artifact type
// either by the manifest's artifactType or, as some do, by its config's
// media type, which for cosign's is the empty one: both are read.
func Bundles(digest name.Digest, opts ...remote.Option) ([][]byte, error) {
	idx, err := remote.Referrers(digest, opts...)
	if err != nil {
		return nil, err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, d := range m.Manifests {
		if d.ArtifactType != BundleMediaType && d.ArtifactType != emptyMediaType {
			continue
		}
		img, err := remote.Image(digest.Context().Digest(d.Digest.String()), opts...)
		if err != nil {
			return nil, err
		}
		layers, err := img.Layers()
		if err != nil {
			return nil, err
		}
		for _, l := range layers {
			if mt, err := l.MediaType(); err != nil || string(mt) != BundleMediaType {
				continue
			}
			rc, err := l.Uncompressed()
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			_, err = io.Copy(&buf, io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
			out = append(out, buf.Bytes())
		}
	}
	return out, nil
}

// checkOptions are what Check requires of a signature's evidence: a
// certificate transparency timestamp, a Rekor entry, and a time it was
// observed at, within the certificate's ten minutes.
var checkOptions = []verify.VerifierOption{
	verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1),
}

// Check verifies a signed entity: a certificate of the policy's signer from
// Fulcio, with a certificate transparency timestamp, logged in Rekor, over an
// in-toto statement of cosign sign whose subject is the image's digest.
func Check(tm root.TrustedMaterial, p Policy, e verify.SignedEntity, digestHex string) error {
	sev, err := verify.NewVerifier(tm, checkOptions...)
	if err != nil {
		return err
	}
	id, err := verify.NewShortCertificateIdentity(p.Issuer, "", "", p.Signer)
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return err
	}
	res, err := sev.Verify(e, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		return err
	}
	if res.Statement == nil || res.Statement.PredicateType != SignPredicate {
		return fmt.Errorf("the signature is not cosign sign's statement")
	}
	return nil
}
