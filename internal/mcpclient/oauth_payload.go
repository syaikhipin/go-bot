package mcpclient

import (
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// This is the upstream McpOAuthGrant payload, not a serialized oauth2.Config.
// Client authentication lives in the separately addressed client information.
type oauthGrantPayload struct {
	ID                 string             `json:"id"`
	ServerURL          string             `json:"serverUrl"`
	Issuer             string             `json:"issuer"`
	ClientKey          string             `json:"clientKey"`
	ClientID           string             `json:"clientId"`
	ConfiguredClientID *string            `json:"configuredClientId,omitempty"`
	ConfiguredScope    *string            `json:"configuredScope,omitempty"`
	Resource           string             `json:"resource,omitempty"`
	Metadata           json.RawMessage    `json:"metadata,omitempty"`
	Tokens             oauthTokensPayload `json:"tokens"`
	ExpiresAt          *int64             `json:"expiresAt,omitempty"`
}

type oauthTokensPayload struct {
	AccessToken  string   `json:"access_token"`
	TokenType    string   `json:"token_type"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresIn    *float64 `json:"expires_in,omitempty"`
	Scope        *string  `json:"scope,omitempty"`
	IDToken      *string  `json:"id_token,omitempty"`
	Issuer       *string  `json:"issuer,omitempty"`
}

func (g oauthGrant) MarshalJSON() ([]byte, error) {
	p := oauthGrantPayload{ID: g.ID, ServerURL: g.ServerURL, Issuer: g.Issuer, ClientKey: g.ClientKey, ClientID: g.Config.ClientID,
		ConfiguredClientID: g.ConfiguredClientID, ConfiguredScope: g.ConfiguredScope, Resource: g.Resource, Metadata: g.Metadata,
		Tokens: oauthTokensPayload{AccessToken: g.Token.AccessToken, TokenType: g.Token.TokenType, RefreshToken: g.Token.RefreshToken}}
	if g.ScopePresent || g.Scope != "" {
		p.Tokens.Scope = &g.Scope
	}
	p.Tokens.ExpiresIn = g.TokenExpiresIn
	if p.Tokens.ExpiresIn == nil {
		p.Tokens.ExpiresIn = oauthTokenExpiresIn(&g.Token)
	}
	if value, ok := g.Token.Extra("id_token").(string); ok {
		p.Tokens.IDToken = &value
	}
	if value, ok := g.Token.Extra("issuer").(string); ok {
		p.Tokens.Issuer = &value
	}
	if !g.Token.Expiry.IsZero() {
		expiry := g.Token.Expiry.UnixMilli()
		p.ExpiresAt = &expiry
	}
	return json.Marshal(p)
}

func (g *oauthGrant) UnmarshalJSON(data []byte) error {
	var p oauthGrantPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*g = oauthGrant{ID: p.ID, ServerURL: p.ServerURL, Issuer: p.Issuer, ClientKey: p.ClientKey, ConfiguredClientID: p.ConfiguredClientID, ConfiguredScope: p.ConfiguredScope,
		Resource: p.Resource, Metadata: p.Metadata, Config: oauth2.Config{ClientID: p.ClientID}, Token: oauth2.Token{AccessToken: p.Tokens.AccessToken, TokenType: p.Tokens.TokenType, RefreshToken: p.Tokens.RefreshToken}}
	g.Binding = oauthIdentityBinding(p.ServerURL, p.ConfiguredClientID, p.ConfiguredScope)
	if p.Metadata != nil {
		var metadata oauthex.AuthServerMeta
		if err := json.Unmarshal(p.Metadata, &metadata); err != nil {
			return err
		}
		g.Config.Endpoint = oauth2.Endpoint{AuthURL: metadata.AuthorizationEndpoint, TokenURL: metadata.TokenEndpoint}
	} else {
		issuer, err := url.Parse(p.Issuer)
		if err != nil {
			return err
		}
		g.Config.Endpoint = oauth2.Endpoint{AuthURL: issuer.ResolveReference(&url.URL{Path: "/authorize"}).String(), TokenURL: issuer.ResolveReference(&url.URL{Path: "/token"}).String()}
	}
	extra := map[string]any{}
	if p.Tokens.Scope != nil {
		g.Scope = *p.Tokens.Scope
		g.ScopePresent = true
		extra["scope"] = g.Scope
	}
	if p.Tokens.ExpiresIn != nil {
		g.TokenExpiresIn = p.Tokens.ExpiresIn
		g.Token.ExpiresIn = int64(*p.Tokens.ExpiresIn)
		extra["expires_in"] = *p.Tokens.ExpiresIn
	}
	if p.Tokens.IDToken != nil {
		extra["id_token"] = *p.Tokens.IDToken
	}
	if p.Tokens.Issuer != nil {
		extra["issuer"] = *p.Tokens.Issuer
	}
	g.Token = *g.Token.WithExtra(extra)
	if p.ExpiresAt != nil {
		g.Token.Expiry = time.UnixMilli(*p.ExpiresAt).UTC()
	}
	return nil
}

func oauthTokenExpiresIn(token *oauth2.Token) *float64 {
	var value float64
	if raw := token.Extra("expires_in"); raw != nil {
		var err error
		switch number := raw.(type) {
		case float64:
			value = number
		case json.Number:
			value, err = number.Float64()
		case string:
			value, err = strconv.ParseFloat(number, 64)
		default:
			value = float64(token.ExpiresIn)
		}
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		return &value
	}
	if token.ExpiresIn == 0 {
		return nil
	}
	value = float64(token.ExpiresIn)
	return &value
}
