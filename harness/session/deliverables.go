package sessionformat

type PresentedFile struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type PresentedFiles struct {
	Turn   uint64          `json:"turn"`
	CallID string          `json:"callId"`
	Files  []PresentedFile `json:"files"`
}
