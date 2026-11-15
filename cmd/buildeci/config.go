package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"go-harness/internal/config"
	"go-harness/internal/imagebuild"
	"go-harness/thirdparty/aliyun"
	"gopkg.in/yaml.v3"
)

type options struct {
	LocalImage string
	Cloud      aliyun.Credentials
	Region     string
	Request    aliyun.CreateImageCache
	Wait       time.Duration
}

type tuning struct {
	SizeGiB       int32         `yaml:"size_gib"`
	RetentionDays int32         `yaml:"retention_days"`
	Wait          time.Duration `yaml:"wait_timeout"`
}

func readOptions(profile, tuningPath string, publish bool) (options, error) {
	c, err := config.Load("", profile)
	if err != nil {
		return options{}, err
	}
	t := tuning{Wait: 90 * time.Minute}
	if tuningPath != "" {
		data, err := os.ReadFile(tuningPath)
		if err != nil {
			return options{}, err
		}
		d := yaml.NewDecoder(bytes.NewReader(data))
		d.KnownFields(true)
		if d.Decode(&t) != nil || d.Decode(new(any)) != io.EOF {
			return options{}, errors.New("invalid buildeci tuning YAML")
		}
	}
	if t.Wait <= 0 || (t.SizeGiB != 0 && t.SizeGiB < 20) || t.RetentionDays < 0 {
		return options{}, errors.New("buildeci requires positive wait_timeout, size_gib of at least 20 if set, and nonnegative retention_days")
	}
	image := c.Registry.PushImage
	server, name, qualified := strings.Cut(image, "/")
	last := name[strings.LastIndex(name, "/")+1:]
	_, tag, tagged := strings.Cut(last, ":")
	_, digest, pinned := strings.Cut(image, "@")
	u, urlErr := url.Parse("https://" + server)
	if !config.ValidImage(image) || !qualified || last == "" || urlErr != nil || u.User != nil ||
		u.Host != server || u.Hostname() == "" || !strings.ContainsAny(server, ".:") ||
		(pinned && !imagebuild.ValidDigest(digest)) || (publish && pinned) ||
		(!pinned && (!tagged || tag == "" || tag == "latest")) {
		return options{}, errors.New("REGISTRY_PUSH_IMAGE must be a registry-qualified, versioned image; prepare requires an explicit tag")
	}
	if publish && (!config.ValidImage(c.Runtime.Image) || strings.Contains(c.Runtime.Image, "@")) {
		return options{}, errors.New("HOST_IMAGE must be a valid local image tag")
	}
	r := c.Runtime.ECI
	a := c.Cloud.Aliyun.Credentials
	for _, field := range []struct{ name, value string }{
		{"ECI_REGION", r.Region}, {"ECI_VSWITCH_ID", r.VSwitchID}, {"ECI_SECURITY_GROUP_ID", r.SecurityGroupID},
		{"ALIBABA_CLOUD_ACCESS_KEY_ID", a.AccessKeyID}, {"ALIBABA_CLOUD_ACCESS_KEY_SECRET", a.AccessKeySecret},
	} {
		if strings.TrimSpace(field.value) == "" {
			return options{}, fmt.Errorf("missing %s", field.name)
		}
	}
	g := c.Registry
	registryServer := ""
	if g.Username != "" || g.Password != "" {
		if strings.TrimSpace(g.Username) == "" || strings.TrimSpace(g.Password) == "" {
			return options{}, errors.New("REGISTRY_USERNAME and REGISTRY_PASSWORD must both be set for a private registry")
		}
		registryServer = server
	}
	return options{
		LocalImage: c.Runtime.Image,
		Cloud:      aliyun.Credentials{AccessKeyID: a.AccessKeyID, AccessKeySecret: a.AccessKeySecret},
		Region:     r.Region, Wait: t.Wait,
		Request: aliyun.CreateImageCache{
			Image: image, VSwitchID: r.VSwitchID, SecurityGroupID: r.SecurityGroupID,
			RegistryServer: registryServer, RegistryUsername: g.Username, RegistryPassword: g.Password,
			SizeGiB: t.SizeGiB, RetentionDays: t.RetentionDays,
		},
	}, nil
}
