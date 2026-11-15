// Package runtime defines the portable contract for running a personal Host.
// Drivers own infrastructure; they never own sessions or model requests.
package runtime

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("Host instance not found")
var ErrUnsupported = errors.New("runtime operation is unsupported")

type ResourceRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}
type Instance struct {
	Ref          ResourceRef `json:"ref"`
	RunID        string      `json:"runId"`
	DeploymentID string      `json:"deploymentId"`
	State        string      `json:"state"`
	Endpoint     string      `json:"endpoint"`
}
type Storage struct{ Type, MountTarget, Path string }
type DockerOptions struct{ EndpointHost, Platform string }
type ECIOptions struct {
	Region, VSwitchID, SecurityGroupID, ImageCacheID string
	CPU, MemoryGiB                                   float32
}
type Spec struct {
	DeploymentID, RunID, Image string
	Environment                map[string]string
	Storage                    Storage
	Docker                     DockerOptions
	ECI                        ECIOptions
	StartTimeout               time.Duration
}
type Capabilities struct {
	StorageTypes []string
	// RestartStopped preserves the same compute instance when Ensure restarts it.
	RestartStopped bool
	// NativeStop allows Stop after Host shutdown (or if Host is unreachable).
	// Otherwise controller waits for the Host process to exit on its own.
	NativeStop bool
}
type Driver interface {
	Name() string
	Capabilities() Capabilities
	Validate(Spec) error
	Preflight(context.Context, Spec) error
	Ensure(context.Context, Spec) (Instance, error)
	Find(context.Context, Spec) (Instance, error)
	Inspect(context.Context, ResourceRef) (Instance, error)
	Stop(context.Context, ResourceRef) error
	Delete(context.Context, ResourceRef) error
}
type Factory func() (Driver, error)
type RegistryDrivers map[string]Factory
