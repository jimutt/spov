package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	modal "github.com/modal-labs/modal-client/go"
)

// EnsureImage resolves the published image, building and publishing it on
// first use. This file holds only the Modal SDK calls; image.go's naming,
// validation and rendering stay under the coverage gate.
//
// @decision(DL-032)
func (p *ModalProvider) EnsureImage(ctx context.Context, spec ImageSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	name := spec.Name()

	// Only a genuine miss justifies a rebuild: an auth failure or a
	// cancelled context would otherwise be reported as a build error
	// after a pointless 25-second build.
	switch _, err := p.image(ctx, name); {
	case err == nil:
		return name, nil
	case !isNotFound(err):
		return "", fmt.Errorf("sandbox: look up image %q: %w", name, err)
	}

	return p.BuildImage(ctx, spec)
}

// BuildImage builds spec and publishes it under its name, whether or not
// the name already resolves.
//
// @decision(DL-016)
func (p *ModalProvider) BuildImage(ctx context.Context, spec ImageSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	name := spec.Name()
	// One DockerfileCommands call per layer: Modal caches each, so a
	// variant or a pin bump rebuilds only from its first changed layer.
	//
	// @decision(DL-032)
	img := p.client.Images.FromRegistry(baseImage, nil)
	for _, layer := range spec.Layers() {
		img = img.DockerfileCommands(layer, nil)
	}
	built, err := img.Build(ctx, p.app, nil)
	if err != nil {
		return "", fmt.Errorf("sandbox: build image %q: %w", name, err)
	}
	if err := built.Publish(ctx, name, nil); err != nil {
		return "", fmt.Errorf("sandbox: publish image %q: %w", name, err)
	}

	p.mu.Lock()
	p.images[name] = built
	p.mu.Unlock()
	return name, nil
}

// ImageID is the ID of the image published as name, looked up afresh.
//
// @decision(DL-016)
func (p *ModalProvider) ImageID(ctx context.Context, name string) (string, error) {
	img, err := p.client.Images.FromName(ctx, name, &modal.ImageFromNameParams{Environment: p.env})
	if err != nil {
		return "", fmt.Errorf("sandbox: look up image %q: %w", name, err)
	}
	return img.ImageID, nil
}

// DeleteImage deletes the image with this ID and forgets any cached
// lookup that resolves to it.
//
// @decision(DL-016)
func (p *ModalProvider) DeleteImage(ctx context.Context, id string) error {
	if err := p.client.Images.Delete(ctx, id, nil); err != nil {
		return fmt.Errorf("sandbox: delete image %s: %w", id, err)
	}
	p.mu.Lock()
	for name, img := range p.images {
		if img.ImageID == id {
			delete(p.images, name)
		}
	}
	p.mu.Unlock()
	return nil
}

// HasImage reports whether name is published.
//
// @decision(DL-016)
func (p *ModalProvider) HasImage(ctx context.Context, name string) (bool, error) {
	switch _, err := p.image(ctx, name); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrImageNotFound):
		return false, nil
	default:
		return false, err
	}
}

// EnsureProxyImage resolves the proxy image for binary, baking it on
// first use: a sandbox boots from the base image, the binary is written
// in, and a snapshot of it is published with no expiry. Two servers that
// bake at once publish the same contents under the same name.
//
// @decision(DL-034)
func (p *ModalProvider) EnsureProxyImage(ctx context.Context, binary []byte) (string, error) {
	if len(binary) == 0 {
		return "", fmt.Errorf("sandbox: no proxy binary to bake")
	}
	name := ProxyImageName(binary)
	switch _, err := p.image(ctx, name); {
	case err == nil:
		return name, nil
	case !isNotFound(err):
		return "", fmt.Errorf("sandbox: look up image %q: %w", name, err)
	}
	return p.bakeProxyImage(ctx, binary, name)
}

// RebakeProxyImage bakes the proxy image again without looking the name
// up first, and forgets the cached lookup.
//
// @decision(DL-034)
func (p *ModalProvider) RebakeProxyImage(ctx context.Context, binary []byte) (string, error) {
	if len(binary) == 0 {
		return "", fmt.Errorf("sandbox: no proxy binary to bake")
	}
	name := ProxyImageName(binary)
	p.mu.Lock()
	delete(p.images, name)
	p.mu.Unlock()
	return p.bakeProxyImage(ctx, binary, name)
}

// bakeProxyImage boots the base, writes the binary in and publishes a
// snapshot of it as name.
func (p *ModalProvider) bakeProxyImage(ctx context.Context, binary []byte, name string) (string, error) {
	base, err := p.client.Images.FromRegistry(proxyBaseImage, nil).
		DockerfileCommands(proxyBaseCommands, nil).Build(ctx, p.app, nil)
	if err != nil {
		return "", fmt.Errorf("sandbox: build the proxy base image: %w", err)
	}
	sb, err := p.client.Sandboxes.ExperimentalCreate(ctx, p.app, base, &modal.SandboxCreateParams{
		Command: []string{"sleep", "infinity"},
		Timeout: 10 * time.Minute,
		Tags:    map[string]string{"spov-bake": name},
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: boot the proxy bake: %w", err)
	}
	defer func() { _, _ = sb.Terminate(context.WithoutCancel(ctx), nil) }()
	h := &modalHandle{sb: sb}
	// @decision(DL-135)
	wctx, cancelWrite := context.WithTimeout(ctx, proxyBinaryWriteTimeout)
	defer cancelWrite()
	res, err := h.ExecInput(wctx, []string{"sh", "-c", "cat > " + ProxyBinaryPath + " && chmod 755 " + ProxyBinaryPath}, binary)
	if err != nil {
		return "", fmt.Errorf("sandbox: write the proxy binary: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("sandbox: write the proxy binary: exit %d: %s", res.ExitCode, res.Stderr)
	}
	snap, err := sb.SnapshotFilesystem(ctx, &modal.SandboxSnapshotFilesystemParams{TTL: modal.NoExpiryTTL})
	if err != nil {
		return "", fmt.Errorf("sandbox: snapshot the proxy image: %w", err)
	}
	if err := snap.Publish(ctx, name, &modal.ImagePublishParams{Environment: p.env}); err != nil {
		return "", fmt.Errorf("sandbox: publish image %q: %w", name, err)
	}
	p.mu.Lock()
	p.images[name] = snap
	p.mu.Unlock()
	return name, nil
}
