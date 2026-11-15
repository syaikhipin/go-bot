package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"go-harness/runtime"
	api "go-harness/thirdparty/docker"
)

const prefix = "io.go-harness."

var safe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Commands interface {
	Run(context.Context, map[string]string, ...string) ([]byte, error)
}
type Driver struct {
	Client Commands
	// PublishedHost is the Docker host address as seen by Controller.
	PublishedHost string
}

func New() *Driver           { return &Driver{Client: api.Client{}} }
func (*Driver) Name() string { return "docker" }
func (*Driver) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{StorageTypes: []string{"container"}, RestartStopped: true, NativeStop: true}
}
func (*Driver) Validate(s runtime.Spec) error {
	if !safe.MatchString(s.DeploymentID) || s.RunID == "" || s.Image == "" {
		return errors.New("invalid Docker Host identity/image")
	}
	if s.Storage.Type != "container" {
		return errors.New("unsupported Docker storage")
	}
	return nil
}
func (d *Driver) Preflight(ctx context.Context, s runtime.Spec) error {
	if err := d.Validate(s); err != nil {
		return err
	}
	if _, err := d.Client.Run(ctx, nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return err
	}
	if _, err := d.Client.Run(ctx, nil, "image", "inspect", s.Image); err != nil {
		return fmt.Errorf("Host image unavailable locally; build or load HOST_IMAGE before starting: %w", err)
	}
	return nil
}

type inspection struct {
	ID              string `json:"Id"`
	Config          struct{ Labels map[string]string }
	State           struct{ Running bool }
	NetworkSettings struct {
		Ports map[string][]struct{ HostIP, HostPort string }
	}
}

func (d *Driver) Inspect(ctx context.Context, ref runtime.ResourceRef) (runtime.Instance, error) {
	b, err := d.Client.Run(ctx, nil, "container", "inspect", ref.ID)
	if err != nil {
		if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "not found") {
			return runtime.Instance{}, runtime.ErrNotFound
		}
		return runtime.Instance{}, err
	}
	var objects []inspection
	if json.Unmarshal(b, &objects) != nil || len(objects) != 1 {
		return runtime.Instance{}, errors.New("invalid Docker inspect reply")
	}
	o := objects[0]
	labels := o.Config.Labels
	if labels[prefix+"component"] != "host" {
		return runtime.Instance{}, errors.New("Docker resource is not a go-harness Host")
	}
	state := "stopped"
	endpoint := ""
	if o.State.Running {
		state = "running"
		for _, p := range o.NetworkSettings.Ports["8080/tcp"] {
			if p.HostPort != "" {
				host := p.HostIP
				if d.PublishedHost != "" {
					host = d.PublishedHost
				} else if host == "0.0.0.0" || host == "::" {
					host = "127.0.0.1"
				}
				endpoint = "http://" + net.JoinHostPort(host, p.HostPort)
				break
			}
		}
	}
	return runtime.Instance{Ref: runtime.ResourceRef{Provider: "docker", ID: o.ID}, DeploymentID: labels[prefix+"deployment-id"], RunID: labels[prefix+"run-id"], State: state, Endpoint: endpoint}, nil
}

func containerName(s runtime.Spec) string {
	return "go-harness-" + s.RunID
}

func DataVolumeName(deploymentID string) string {
	return "gh-" + deploymentID + "-data"
}

func (d *Driver) Find(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
	return d.Inspect(ctx, runtime.ResourceRef{Provider: "docker", ID: containerName(s)})
}

func (d *Driver) Ensure(ctx context.Context, s runtime.Spec) (runtime.Instance, error) {
	if err := d.Validate(s); err != nil {
		return runtime.Instance{}, err
	}
	name := containerName(s)
	current, err := d.Find(ctx, s)
	if err == nil {
		if current.DeploymentID != s.DeploymentID || current.RunID != s.RunID {
			return current, errors.New("existing Docker Host belongs to a different deployment or run")
		}
		if current.State != "running" {
			if _, err = d.Client.Run(ctx, nil, "start", current.Ref.ID); err != nil {
				return current, err
			}
		}
		return d.Inspect(ctx, current.Ref)
	}
	if !errors.Is(err, runtime.ErrNotFound) {
		return current, err
	}
	endpointHost := s.Docker.EndpointHost
	if endpointHost == "" {
		endpointHost = "127.0.0.1"
	}
	// Chromium uses multiple processes and threads for each named browser.
	args := []string{
		"run", "--pull=never", "--detach", "--init", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", net.JoinHostPort(endpointHost, "") + ":8080",
		"--pids-limit", "1024", "--memory", "2048m", "--cpus", "2",
		"--shm-size", "512m", "--workdir", "/root/harness",
		"--mount", "type=volume,source=" + DataVolumeName(s.DeploymentID) + ",target=/root/harness",
	}
	for key, value := range map[string]string{"component": "host", "deployment-id": s.DeploymentID, "run-id": s.RunID} {
		args = append(args, "--label", prefix+key+"="+value)
	}
	if s.Docker.Platform != "" {
		args = append(args, "--platform", s.Docker.Platform)
	}
	keys := make([]string, 0, len(s.Environment))
	for key := range s.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key)
	}
	args = append(args, s.Image)
	if _, err = d.Client.Run(ctx, s.Environment, args...); err != nil {
		return current, err
	}
	return d.Inspect(ctx, runtime.ResourceRef{Provider: "docker", ID: name})
}

func (d *Driver) Stop(ctx context.Context, r runtime.ResourceRef) error {
	_, err := d.Client.Run(ctx, nil, "stop", "--time", "20", r.ID)
	if err != nil && strings.Contains(err.Error(), "No such") {
		return nil
	}
	return err
}
func (d *Driver) Delete(ctx context.Context, r runtime.ResourceRef) error {
	_, err := d.Client.Run(ctx, nil, "rm", "--force", r.ID)
	if err != nil && strings.Contains(err.Error(), "No such") {
		return nil
	}
	return err
}
