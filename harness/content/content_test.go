package content

import (
	"encoding/json"
	"testing"
)

func TestImageBlockRoundTripAndClone(t *testing.T) {
	ref := ImageAttachment{
		ID: "sha256:abc", MIMEType: "image/png", Bytes: 128,
		Width: 20, Height: 10, Name: "source.png", ReadPath: "/root/harness/workspaces/session/source.png",
		OriginalDimensions: &ImageDimensions{Width: 40, Height: 20},
	}
	encoded, err := json.Marshal(ImageBlock(ref))
	if err != nil {
		t.Fatal(err)
	}
	var block ContentBlock
	if err := json.Unmarshal(encoded, &block); err != nil {
		t.Fatal(err)
	}
	if got := ImageAttachmentFromBlock(block); got.ID != ref.ID || got.MIMEType != ref.MIMEType || got.Bytes != ref.Bytes || got.Width != ref.Width || got.Height != ref.Height || got.Name != ref.Name || got.ReadPath != ref.ReadPath || got.OriginalDimensions == nil || *got.OriginalDimensions != *ref.OriginalDimensions {
		t.Fatalf("image metadata changed across journal round trip: %+v", got)
	}
	cloned := CloneContentBlocks([]ContentBlock{block})
	cloned[0].OriginalDimensions.Width = 1
	if block.OriginalDimensions.Width != ref.OriginalDimensions.Width {
		t.Fatal("cloning content blocks must not share dimension pointers")
	}
	if err := ValidateDurableContentBlocks([]ContentBlock{block}); err != nil {
		t.Fatal(err)
	}
}
