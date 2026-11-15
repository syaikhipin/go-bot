package workflow

import (
	"errors"
	"fmt"
)

// validateSchema checks the same object-rooted keyword vocabulary accepted by
// DeepSeek's workflow agent() hook. This validates the declaration shape; the
// child provider remains responsible for producing/validating output data.
func validateSchema(schema map[string]any, root bool) error {
	if root && schema["type"] != "object" {
		return errors.New("root type must be object")
	}
	for key, raw := range schema {
		switch key {
		case "type":
			name, ok := raw.(string)
			if !ok || (name != "object" && name != "array" && name != "string" && name != "number" && name != "integer" && name != "boolean" && name != "null") {
				return errors.New("unsupported type")
			}
		case "properties":
			properties, ok := raw.(map[string]any)
			if !ok {
				return errors.New("invalid properties")
			}
			for _, child := range properties {
				item, ok := child.(map[string]any)
				if !ok {
					return errors.New("invalid property schema")
				}
				if err := validateSchema(item, false); err != nil {
					return err
				}
			}
		case "required":
			list, ok := raw.([]any)
			if !ok {
				return errors.New("invalid required")
			}
			for _, item := range list {
				if _, ok := item.(string); !ok {
					return errors.New("invalid required item")
				}
			}
		case "additionalProperties":
			if _, ok := raw.(bool); !ok {
				item, ok := raw.(map[string]any)
				if !ok {
					return errors.New("invalid additionalProperties")
				}
				if err := validateSchema(item, false); err != nil {
					return err
				}
			}
		case "items":
			item, ok := raw.(map[string]any)
			if !ok {
				return errors.New("invalid items")
			}
			if err := validateSchema(item, false); err != nil {
				return err
			}
		case "oneOf":
			list, ok := raw.([]any)
			if !ok || len(list) == 0 {
				return errors.New("invalid oneOf")
			}
			for _, child := range list {
				item, ok := child.(map[string]any)
				if !ok {
					return errors.New("invalid oneOf branch")
				}
				if err := validateSchema(item, false); err != nil {
					return err
				}
			}
		case "enum":
			if _, ok := raw.([]any); !ok {
				return errors.New("invalid enum")
			}
		case "const":
			// The VM's JSON materializer has already copied this losslessly.
		default:
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	return nil
}
