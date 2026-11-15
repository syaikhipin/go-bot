package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

const toolSchemaURL = "https://serveragents.invalid/tool-parameters.json"

// Tool parameter schemas are compiled once when the Agent is assembled. In
// particular, a schema reference must never cause a runtime network or file
// read from the control plane.
type denyToolSchemaLoader struct{}

func (denyToolSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external tool schema reference is not allowed: %s", url)
}

func compileToolParameters(raw json.RawMessage) (*jsonschema.Schema, error) {
	declaration, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	root, ok := declaration.(map[string]any)
	if !ok || root["type"] != "object" {
		return nil, errors.New("tool parameters must declare an object root")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyToolSchemaLoader{})
	if err := compiler.AddResource(toolSchemaURL, declaration); err != nil {
		return nil, err
	}
	return compiler.Compile(toolSchemaURL)
}

func validateToolParameters(schema *jsonschema.Schema, raw json.RawMessage) error {
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return schema.Validate(value)
}

func presentedToolArgumentError(err error) string {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return "Error: invalid arguments: " + err.Error()
	}
	var problems []string
	var visit func(*jsonschema.ValidationError)
	visit = func(failure *jsonschema.ValidationError) {
		if len(failure.Causes) != 0 {
			for _, cause := range failure.Causes {
				visit(cause)
			}
			return
		}
		path := ""
		for _, part := range failure.InstanceLocation {
			if _, err := strconv.Atoi(part); err == nil {
				path += "[" + part + "]"
			} else if path == "" {
				path = part
			} else {
				path += "." + part
			}
		}
		switch detail := failure.ErrorKind.(type) {
		case *kind.Type:
			if path == "" {
				path = "arguments"
			}
			wanted := strings.Join(detail.Want, " or ")
			article := "a"
			if wanted == "object" || wanted == "array" || wanted == "integer" {
				article = "an"
			}
			problems = append(problems, fmt.Sprintf("%q must be %s %s", path, article, wanted))
		case *kind.Required:
			for _, name := range detail.Missing {
				if path != "" {
					name = path + "." + name
				}
				problems = append(problems, fmt.Sprintf("missing required property %q", name))
			}
		default:
			problems = append(problems, failure.Error())
		}
	}
	visit(validation)
	return "Error: invalid arguments: " + strings.Join(problems, "; ")
}
