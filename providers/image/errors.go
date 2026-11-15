package imageproviders

// UnconfiguredError is returned while the built-in tool is available but the
// account has not supplied a key for its selected image provider.
type UnconfiguredError struct{}

func (UnconfiguredError) Error() string {
	return "Image generation is not configured. Add an API key in Settings → Image generation."
}

func (UnconfiguredError) ToolErrorInfo() (string, string) {
	return "ImageGenerationError", "IMAGE_GENERATION_NOT_CONFIGURED"
}
