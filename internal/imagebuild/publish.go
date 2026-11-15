// Package imagebuild publishes Host images using the operator's Docker login.
package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var pushDigestPattern = regexp.MustCompile(`(?m)^\S+: digest: (sha256:[0-9a-f]{64}) size: [0-9]+\s*$`)

func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

func PublishAMD64(ctx context.Context, localImage, pushImage string, logs io.Writer) (string, error) {
	// Registry authentication comes from the Docker CLI context. Application
	// and Alibaba credentials are only read by our own commands.
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "MODEL_API_KEY", "WEB_SEARCH_API_KEY", "HOST_API_TOKEN", "CONTROLLER_API_TOKEN",
			"ALIBABA_CLOUD_ACCESS_KEY_ID", "ALIBABA_CLOUD_ACCESS_KEY_SECRET",
			"REGISTRY_USERNAME", "REGISTRY_PASSWORD":
		default:
			env = append(env, entry)
		}
	}
	cmd := exec.CommandContext(ctx, "docker", "buildx", "build",
		"--platform", "linux/amd64", "--provenance=false", "--load",
		"--file", "build/Dockerfile.host", "--tag", localImage, "--tag", pushImage, ".")
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Run(); err != nil {
		return "", err
	}
	var output bytes.Buffer
	push := exec.CommandContext(ctx, "docker", "image", "push", pushImage)
	push.Env = env
	push.Stdout, push.Stderr = io.MultiWriter(logs, &output), logs
	if err := push.Run(); err != nil {
		return "", err
	}
	match := pushDigestPattern.FindSubmatch(output.Bytes())
	if len(match) != 2 {
		return "", errors.New("Docker did not report a valid published image digest")
	}
	return string(match[1]), nil
}
