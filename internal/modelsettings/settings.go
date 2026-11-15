// Package modelsettings defines the model catalog shared by Controller and Host.
package modelsettings

import (
	"errors"
	"slices"
)

const InternalPath = "/internal/host/model-settings"
const ScheduledSelectionPrefix = InternalPath + "/scheduled/"

var ErrUnconfigured = errors.New("model_unconfigured")
var ErrUnavailable = errors.New("model_unavailable")

type Selection struct {
	EndpointID string `json:"endpointId"`
	ModelID    string `json:"modelId"`
}

type Endpoint struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"baseUrl"`
	APIKey  string   `json:"apiKey"`
	Models  []string `json:"models"`
}

type Catalog struct {
	DefaultEndpointID string              `json:"defaultEndpointId"`
	Endpoints         map[string]Endpoint `json:"endpoints"`
}

func (c Catalog) Default() Selection {
	endpoint := c.Endpoints[c.DefaultEndpointID]
	if len(endpoint.Models) == 0 {
		return Selection{}
	}
	return Selection{EndpointID: c.DefaultEndpointID, ModelID: endpoint.Models[0]}
}

func (c Catalog) Resolve(selection Selection) (Endpoint, error) {
	if selection == (Selection{}) {
		return Endpoint{}, ErrUnconfigured
	}
	endpoint, ok := c.Endpoints[selection.EndpointID]
	if !ok || endpoint.BaseURL == "" || endpoint.APIKey == "" || !slices.Contains(endpoint.Models, selection.ModelID) {
		return Endpoint{}, ErrUnavailable
	}
	return endpoint, nil
}

type Snapshot struct {
	Catalog  Catalog              `json:"catalog"`
	Sessions map[string]Selection `json:"sessions"`
	Revision int64                `json:"revision"`
}
