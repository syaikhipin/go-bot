package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go-bot/plugin"
	"regexp"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

func failure(code, message string) error { return &plugin.Error{Code: code, Message: message} }

type denySchemaLoader struct{}

func (denySchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are not supported: %s", url)
}
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(denySchemaLoader{})
	const url = "https://go-bot.invalid/plugin-schema.json"
	if err := c.AddResource(url, value); err != nil {
		return nil, err
	}
	return c.Compile(url)
}
func validateSchema(schema *jsonschema.Schema, raw json.RawMessage) error {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return schema.Validate(value)
}
