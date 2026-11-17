package session

import "go-bot/harness/content"

// Retired single-Session code keeps aliases while production callers move to
// the independent ServerAgents content package.
type ImageDimensions = content.ImageDimensions
type ImageAttachment = content.ImageAttachment
type ContentBlock = content.ContentBlock

func ImageBlock(ref ImageAttachment) ContentBlock { return content.ImageBlock(ref) }

func ImageAttachmentFromBlock(block ContentBlock) ImageAttachment {
	return content.ImageAttachmentFromBlock(block)
}

func CloneContentBlocks(blocks []ContentBlock) []ContentBlock {
	return content.CloneContentBlocks(blocks)
}

func ValidateDurableContentBlocks(blocks []ContentBlock) error {
	return content.ValidateDurableContentBlocks(blocks)
}
