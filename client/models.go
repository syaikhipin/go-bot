package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

const settingsPath = "/v1/settings/models"

type ModelSelection struct {
	EndpointID string `json:"endpointId"`
	ModelID    string `json:"modelId"`
}

type ModelEndpoint struct {
	Protocol  string   `json:"protocol"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"baseUrl"`
	Models    []string `json:"models"`
	HasAPIKey bool     `json:"hasApiKey"`
}

type ModelSettings struct {
	DefaultEndpointID string                   `json:"defaultEndpointId"`
	Endpoints         map[string]ModelEndpoint `json:"endpoints"`
}

type SessionModel struct {
	Selection *ModelSelection `json:"selection"`
	Status    string          `json:"status"`
}

type ModelEndpointInput struct {
	// Protocol is responses (the default) or anthropic_messages.
	Protocol string   `json:"protocol,omitempty"`
	Name     string   `json:"name"`
	BaseURL  string   `json:"baseUrl"`
	Models   []string `json:"models"`
	APIKey   string   `json:"apiKey,omitempty"`
}

type ModelUpdate struct {
	EndpointID string `json:"endpointId"`
	Status     string `json:"status"`
}

func (c Client) RegisterAccount(ctx context.Context, userID string) error {
	var created struct {
		UserID string `json:"userId"`
	}
	return c.readJSON(ctx, http.MethodPost, "/v1/accounts", map[string]string{"userId": userID}, &created)
}

func (c Client) GetModelSettings(ctx context.Context) (settings ModelSettings, err error) {
	err = c.readJSON(ctx, http.MethodGet, settingsPath, nil, &settings)
	return
}

// SaveModelEndpoint creates an endpoint when endpointID is empty. An empty
// API key during an edit retains the saved key.
func (c Client) SaveModelEndpoint(ctx context.Context, endpointID string, input ModelEndpointInput) (update ModelUpdate, err error) {
	method, path := http.MethodPost, settingsPath+"/endpoints"
	if endpointID != "" {
		method, path = http.MethodPatch, path+"/"+url.PathEscape(endpointID)
	}
	err = c.readJSON(ctx, method, path, input, &update)
	return
}

func (c Client) SetDefaultEndpoint(ctx context.Context, endpointID string) (update ModelUpdate, err error) {
	err = c.readJSON(ctx, http.MethodPut, settingsPath+"/default", map[string]string{"endpointId": endpointID}, &update)
	return
}

func (c Client) DeleteModelEndpoint(ctx context.Context, endpointID string) (update ModelUpdate, err error) {
	err = c.readJSON(ctx, http.MethodDelete, settingsPath+"/endpoints/"+url.PathEscape(endpointID), nil, &update)
	return
}

func (c Client) GetSessionModel(ctx context.Context, sessionID string) (model SessionModel, err error) {
	err = c.readJSON(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/model", nil, &model)
	return
}

func (c Client) SetSessionModel(ctx context.Context, sessionID string, selection ModelSelection) (model SessionModel, err error) {
	err = c.readJSON(ctx, http.MethodPut, "/v1/sessions/"+url.PathEscape(sessionID)+"/model", selection, &model)
	return
}

func (c Client) readJSON(ctx context.Context, method, path string, input, output any) error {
	res, err := c.Do(ctx, method, path, input)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(output)
}
