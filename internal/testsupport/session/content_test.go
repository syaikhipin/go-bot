package session

import (
	"encoding/json"
	"testing"
)

func TestImageBlockKeepsServerAgentMetadataAcrossJournalAndClone(t *testing.T) {
	attachment := ImageAttachment{
		ID: "sha256:abc", MIMEType: "image/png", Bytes: 128,
		Width: 20, Height: 10, Name: "source.png", ReadPath: "/root/harness/workspaces/session/source.png",
		OriginalDimensions: &ImageDimensions{Width: 40, Height: 20},
	}
	encoded, err := json.Marshal(ImageBlock(attachment))
	if err != nil {
		t.Fatal(err)
	}
	var restored ContentBlock
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if got := ImageAttachmentFromBlock(restored); got.ID != attachment.ID || got.MIMEType != attachment.MIMEType || got.Bytes != attachment.Bytes || got.Width != attachment.Width || got.Height != attachment.Height || got.Name != attachment.Name || got.ReadPath != attachment.ReadPath || got.OriginalDimensions == nil || *got.OriginalDimensions != *attachment.OriginalDimensions {
		t.Fatalf("image metadata changed across journal round trip: %+v", got)
	}
	cloned := CloneContentBlocks([]ContentBlock{restored})
	cloned[0].OriginalDimensions.Width = 1
	if restored.OriginalDimensions.Width != 40 {
		t.Fatal("cloning content blocks must not share dimension pointers")
	}
}
