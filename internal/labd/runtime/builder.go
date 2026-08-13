package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bio4554/lab/stacks"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// Builder builds agent images lazily. Tags are content-addressed:
// lab/agent-<stack>:<hash> where the hash covers the stack Dockerfile,
// the base Dockerfile, the build args, and the embedded CLI binaries —
// so any input change yields a new tag and an automatic rebuild, while
// unchanged inputs hit the existing image and cost nothing.
type Builder struct {
	rt  *Runtime
	log *slog.Logger

	moduleDir         string
	skipClaudeInstall bool
}

// BuilderOptions configures a Builder.
type BuilderOptions struct {
	// ModuleDir is the lab repo checkout from which the in-container
	// CLI stubs (cmd/kbase, cmd/lab-agent) are cross-compiled with the
	// host go toolchain. Empty resolves it via `go env GOMOD`, which
	// covers the v1 deployment mode of running labd from the checkout.
	ModuleDir string
	// SkipClaudeInstall builds base images without the Claude Code
	// install layer. Test knob: that layer needs network access to
	// claude.ai. It participates in the content hash, so test images
	// never alias real ones.
	SkipClaudeInstall bool
	Logger            *slog.Logger
}

// NewBuilder returns a Builder that talks to rt's Docker daemon.
func NewBuilder(rt *Runtime, opts BuilderOptions) *Builder {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Builder{
		rt:                rt,
		log:               log,
		moduleDir:         opts.ModuleDir,
		skipClaudeInstall: opts.SkipClaudeInstall,
	}
}

// EnsureImage makes sure the image for stack exists locally, building
// the base image and the stack layer as needed, and returns its tag.
func (b *Builder) EnsureImage(ctx context.Context, stack string) (string, error) {
	if !ValidStack(stack) {
		return "", fmt.Errorf("runtime: unknown stack %q (valid: %s)", stack, strings.Join(Stacks(), ", "))
	}

	arch, err := b.daemonArch(ctx)
	if err != nil {
		return "", err
	}
	bins, err := b.buildStubs(ctx, arch)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(bins.dir)

	baseDockerfile, err := stacks.FS.ReadFile("base/Dockerfile")
	if err != nil {
		return "", fmt.Errorf("runtime: embedded base Dockerfile: %w", err)
	}
	baseArgs := map[string]string{}
	if b.skipClaudeInstall {
		baseArgs["SKIP_CLAUDE_INSTALL"] = "1"
	}
	baseHash := contentHash(baseDockerfile, baseArgs, bins.kbaseHash, bins.labAgentHash)
	baseTag := "lab/base:" + baseHash

	if err := b.ensureBuilt(ctx, baseTag, baseDockerfile, baseArgs, map[string]string{
		"kbase":     bins.kbase,
		"lab-agent": bins.labAgent,
	}); err != nil {
		return "", err
	}

	if stack == "base" {
		// The base stack has no toolchain layer; alias the base image
		// under the uniform lab/agent-<stack> naming.
		tag := "lab/agent-base:" + baseHash
		if err := b.rt.cli.ImageTag(ctx, baseTag, tag); err != nil {
			return "", fmt.Errorf("runtime: tag %s: %w", tag, err)
		}
		return tag, nil
	}

	stackDockerfile, err := stacks.FS.ReadFile(stack + "/Dockerfile")
	if err != nil {
		return "", fmt.Errorf("runtime: embedded %s Dockerfile: %w", stack, err)
	}
	stackArgs := map[string]string{
		"BASE_IMAGE": baseTag, // contains baseHash, chaining the hashes
		"TARGETARCH": arch,
	}
	tag := "lab/agent-" + stack + ":" + contentHash(stackDockerfile, stackArgs)
	if err := b.ensureBuilt(ctx, tag, stackDockerfile, stackArgs, nil); err != nil {
		return "", err
	}
	return tag, nil
}

// contentHash hashes a Dockerfile, its build args (order-independent),
// and any extra inputs into a short hex tag component.
func contentHash(dockerfile []byte, args map[string]string, extra ...string) string {
	h := sha256.New()
	h.Write(dockerfile)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, args[k])
	}
	for _, e := range extra {
		fmt.Fprintf(h, "\x00%s", e)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ensureBuilt builds tag from dockerfile unless it already exists.
// files maps context paths to host paths added alongside the
// Dockerfile in the build context.
func (b *Builder) ensureBuilt(ctx context.Context, tag string, dockerfile []byte, args, files map[string]string) error {
	exists, err := b.imageExists(ctx, tag)
	if err != nil {
		return err
	}
	if exists {
		b.log.Debug("image up to date", "tag", tag)
		return nil
	}
	b.log.Info("building image", "tag", tag)
	buildCtx, err := tarContext(dockerfile, files)
	if err != nil {
		return err
	}
	buildArgs := make(map[string]*string, len(args))
	for k, v := range args {
		buildArgs[k] = &v
	}
	resp, err := b.rt.cli.ImageBuild(ctx, buildCtx, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
		BuildArgs:  buildArgs,
	})
	if err != nil {
		return fmt.Errorf("runtime: build %s: %w", tag, err)
	}
	defer resp.Body.Close()
	return b.streamBuildOutput(tag, resp.Body)
}

// streamBuildOutput decodes the daemon's JSON build stream, logging
// progress and surfacing the output tail on failure.
func (b *Builder) streamBuildOutput(tag string, body io.Reader) error {
	const tailLen = 20
	var tail []string
	dec := json.NewDecoder(body)
	for {
		var msg struct {
			Stream      string `json:"stream"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("runtime: build %s: reading build stream: %w", tag, err)
		}
		if msg.Error != "" {
			detail := msg.ErrorDetail.Message
			if detail == "" {
				detail = msg.Error
			}
			return fmt.Errorf("runtime: build %s failed: %s\nlast output:\n%s",
				tag, detail, strings.Join(tail, "\n"))
		}
		if line := strings.TrimRight(msg.Stream, "\n"); line != "" {
			b.log.Debug("build", "tag", tag, "output", line)
			tail = append(tail, line)
			if len(tail) > tailLen {
				tail = tail[1:]
			}
		}
	}
	return nil
}

func (b *Builder) imageExists(ctx context.Context, tag string) (bool, error) {
	list, err := b.rt.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", tag)),
	})
	if err != nil {
		return false, fmt.Errorf("runtime: list images for %s: %w", tag, err)
	}
	return len(list) > 0, nil
}

// daemonArch maps the daemon's reported architecture to GOARCH/Docker
// platform naming.
func (b *Builder) daemonArch(ctx context.Context) (string, error) {
	info, err := b.rt.cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("runtime: docker info: %w", err)
	}
	switch info.Architecture {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("runtime: unsupported docker daemon architecture %q", info.Architecture)
	}
}

// stubBins holds the cross-compiled in-container CLI binaries.
type stubBins struct {
	dir          string
	kbase        string
	labAgent     string
	kbaseHash    string
	labAgentHash string
}

// buildStubs cross-compiles cmd/kbase and cmd/lab-agent for
// linux/<arch> with the host go toolchain into a temp dir.
func (b *Builder) buildStubs(ctx context.Context, arch string) (stubBins, error) {
	moduleDir, err := b.resolveModuleDir(ctx)
	if err != nil {
		return stubBins{}, err
	}
	dir, err := os.MkdirTemp("", "lab-stubs-")
	if err != nil {
		return stubBins{}, fmt.Errorf("runtime: temp dir for stubs: %w", err)
	}
	bins := stubBins{
		dir:      dir,
		kbase:    filepath.Join(dir, "kbase"),
		labAgent: filepath.Join(dir, "lab-agent"),
	}
	for name, out := range map[string]string{"kbase": bins.kbase, "lab-agent": bins.labAgent} {
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/"+name)
		cmd.Dir = moduleDir
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
		if outp, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(dir)
			return stubBins{}, fmt.Errorf("runtime: cross-compile %s: %w\n%s", name, err, outp)
		}
	}
	if bins.kbaseHash, err = fileHash(bins.kbase); err != nil {
		os.RemoveAll(dir)
		return stubBins{}, err
	}
	if bins.labAgentHash, err = fileHash(bins.labAgent); err != nil {
		os.RemoveAll(dir)
		return stubBins{}, err
	}
	return bins, nil
}

func (b *Builder) resolveModuleDir(ctx context.Context) (string, error) {
	if b.moduleDir != "" {
		return b.moduleDir, nil
	}
	out, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("runtime: go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("runtime: not in a go module and no ModuleDir configured")
	}
	return filepath.Dir(gomod), nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("runtime: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("runtime: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tarContext assembles an in-memory tar build context: the Dockerfile
// plus the given context-path → host-path files.
func tarContext(dockerfile []byte, files map[string]string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0o644,
		Size: int64(len(dockerfile)),
	}); err != nil {
		return nil, fmt.Errorf("runtime: tar context: %w", err)
	}
	if _, err := tw.Write(dockerfile); err != nil {
		return nil, fmt.Errorf("runtime: tar context: %w", err)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(files[name])
		if err != nil {
			return nil, fmt.Errorf("runtime: tar context: %w", err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(data)),
		}); err != nil {
			return nil, fmt.Errorf("runtime: tar context: %w", err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("runtime: tar context: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("runtime: tar context: %w", err)
	}
	return &buf, nil
}
