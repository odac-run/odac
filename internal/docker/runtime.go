package docker

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
)

// Runtime names the language runtime a git app's image was built on, so the
// Cloud can show "Node 20.11.1 on node:20-alpine" without guessing from the
// repository. It is decided by the builder, because only the builder knows
// which strategy ran and which image carried the language.
type Runtime struct {
	// Strategy is one of RuntimeStrategies.
	Strategy string
	// Image is the reference of the image carrying the language, empty when
	// it cannot be named honestly (a Dockerfile whose final base is built
	// from ARGs).
	Image string
	// Version is the language's exact version from that image's Env, empty
	// when the image does not declare one.
	Version string
}

// RuntimeStrategies is the closed wire enum of Runtime.Strategy. The Cloud
// renders these, so a builder strategy must map onto one of them rather than
// leak its internal key.
var RuntimeStrategies = map[string]bool{
	"node-npm": true, "node-pnpm": true, "node-yarn": true, "bun": true,
	"python": true, "go": true, "rust": true, "php": true, "static": true,
	"dockerfile": true,
}

// runtimeIdentity maps a builder strategy onto the wire: the Strategy value,
// the Env key the language image declares its exact version under, and
// whether that image is the compiler rather than the runtime (compiled
// languages run on a plain alpine that knows nothing about the toolchain).
type runtimeIdentity struct {
	strategy   string
	versionEnv string
	compiler   bool
}

var runtimeIdentities = map[string]runtimeIdentity{
	"BUN":       {strategy: "bun", versionEnv: "BUN_VERSION"},
	"GO":        {strategy: "go", versionEnv: "GOLANG_VERSION", compiler: true},
	"NODE_NPM":  {strategy: "node-npm", versionEnv: "NODE_VERSION"},
	"NODE_PNPM": {strategy: "node-pnpm", versionEnv: "NODE_VERSION"},
	"NODE_YARN": {strategy: "node-yarn", versionEnv: "NODE_VERSION"},
	"PHP":       {strategy: "php", versionEnv: "PHP_VERSION"},
	"PYTHON":    {strategy: "python", versionEnv: "PYTHON_VERSION"},
	"RUST":      {strategy: "rust", versionEnv: "RUST_VERSION", compiler: true},
	"STATIC":    {strategy: "static", versionEnv: "NGINX_VERSION"},
}

// dockerfileVersionEnvs are the version keys a custom Dockerfile's image may
// inherit from an official base. A Dockerfile names no language, so a version
// is reported only when exactly one of them is present.
var dockerfileVersionEnvs = []string{
	"NODE_VERSION", "PYTHON_VERSION", "GOLANG_VERSION", "PHP_VERSION",
	"RUST_VERSION", "BUN_VERSION", "NGINX_VERSION",
}

var (
	// runtimeVersionRe is the shape a reported version must have. The value
	// comes from an image's Env, which whoever published the image wrote.
	runtimeVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,31}$`)
	// runtimeImageRe bounds a reported image reference: registry, path, tag
	// and digest characters only.
	runtimeImageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]{0,254}$`)
)

// Map renders the Runtime for the app record and the build summary, omitting
// members that carry nothing (the Cloud diffs rows by serialization). Nil
// when the strategy is unknown.
func (r Runtime) Map() map[string]any {
	if !RuntimeStrategies[r.Strategy] {
		return nil
	}
	out := map[string]any{"strategy": r.Strategy}
	if runtimeImageRe.MatchString(r.Image) {
		out["image"] = r.Image
	}
	if runtimeVersionRe.MatchString(r.Version) {
		out["version"] = r.Version
	}
	return out
}

// RuntimeRow re-validates a stored runtime (the app record, or a build
// summary on disk) before it is sent, nil when it is not one. Both stores
// can be edited by hand, so nothing in them is relayed verbatim.
func RuntimeRow(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	strategy, _ := m["strategy"].(string)
	image, _ := m["image"].(string)
	version, _ := m["version"].(string)
	return Runtime{Strategy: strategy, Image: image, Version: version}.Map()
}

// buildRuntime decides the Runtime of a build. built reports whether
// imageName now holds this build: on a failed build the tag still names the
// previous image, whose Env must not be read as this build's.
func (c *Client) buildRuntime(strategy *buildStrategy, bctx BuildContext, imageName string, built bool) Runtime {
	if strategy == nil {
		return Runtime{}
	}
	if strategy.custom {
		rt := Runtime{Strategy: "dockerfile"}
		if content, err := readProjectFile(filepath.Join(bctx.InternalPath, "Dockerfile")); err == nil {
			rt.Image = dockerfileRuntimeImage(string(content))
		}
		if built {
			rt.Version = soleEnvVersion(c.imageEnv(imageName))
		}
		return rt
	}
	identity, ok := runtimeIdentities[strategy.key]
	if !ok {
		return Runtime{}
	}
	image := strategy.runtime
	if identity.compiler {
		image = strategy.image
	}
	return Runtime{
		Strategy: identity.strategy,
		Image:    image,
		Version:  envVersion(c.imageEnv(image), identity.versionEnv),
	}
}

// imageEnv reads a local image's config Env, nil when the image is missing.
// The builder refreshed every image it reads here, so no pull is needed.
func (c *Client) imageEnv(ref string) []string {
	if ref == "" || !c.available {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	data, _, err := c.api.ImageInspectWithRaw(ctx, ref)
	if err != nil || data.Config == nil {
		return nil
	}
	return data.Config.Env
}

// envVersion returns key's value from an image Env when it has a version's
// shape, "" otherwise.
func envVersion(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			if runtimeVersionRe.MatchString(v) {
				return v
			}
			return ""
		}
	}
	return ""
}

// soleEnvVersion is envVersion for an image whose language is unknown: the
// version is reported only when exactly one known key is present, because a
// multi-language image has no single answer.
func soleEnvVersion(env []string) string {
	found := ""
	for _, key := range dockerfileVersionEnvs {
		if v := envVersion(env, key); v != "" {
			if found != "" {
				return ""
			}
			found = v
		}
	}
	return found
}

// dockerfileRuntimeImage names the image the final stage of a Dockerfile
// starts from, "" when that is not a plain reference. A final stage named
// after an earlier one inherits that stage's answer; an ARG-substituted
// reference, `scratch` or anything outside the reference character set is
// unnamed. Flags such as --platform and the `AS name` suffix are not part of
// the reference.
func dockerfileRuntimeImage(content string) string {
	stages := map[string]string{}
	last := ""
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		args := fields[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		ref := args[0]
		resolved, isStage := stages[strings.ToLower(ref)]
		if !isStage {
			resolved = ""
			if !strings.Contains(ref, "$") && !strings.EqualFold(ref, "scratch") && runtimeImageRe.MatchString(ref) {
				resolved = ref
			}
		}
		if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
			stages[strings.ToLower(args[2])] = resolved
		}
		last = resolved
	}
	return last
}
