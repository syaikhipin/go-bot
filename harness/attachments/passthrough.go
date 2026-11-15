package attachments

import (
	"bytes"
	"encoding/binary"
)

// canPassThroughImage is deliberately conservative. A full raster decode has
// already succeeded; these container checks establish that retaining the
// encoded bytes will not retain metadata, animation, a non-sRGB profile, or
// samples outside the native normalization policy.
func canPassThroughImage(data []byte, mime string, width, height int) bool {
	if len(data) > maxNormalizedBytes || width <= 0 || height <= 0 ||
		width > maxSourceDimension || height > maxSourceDimension ||
		int64(width)*int64(height) > maxNormalizedPixels {
		return false
	}
	switch mime {
	case "image/png":
		return cleanPNG(data)
	case "image/jpeg":
		return cleanJPEG(data)
	case "image/webp":
		return cleanWebP(data)
	default: // GIF is never passed through by the native normalizer.
		return false
	}
}

func cleanPNG(data []byte) bool {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	seenHeader, seenData := false, false
	for offset := 8; offset < len(data); {
		if len(data)-offset < 12 {
			return false
		}
		length := uint64(binary.BigEndian.Uint32(data[offset:]))
		if length > uint64(len(data)-offset-12) {
			return false
		}
		kind := string(data[offset+4 : offset+8])
		chunk := data[offset+8 : offset+8+int(length)]
		offset += 12 + int(length)
		switch kind {
		case "IHDR":
			if seenHeader || seenData || length != 13 || chunk[8] != 8 || (chunk[9] != 2 && chunk[9] != 6) || chunk[10] != 0 || chunk[11] != 0 || chunk[12] != 0 {
				return false
			}
			seenHeader = true
		case "IDAT":
			if !seenHeader || length == 0 {
				return false
			}
			seenData = true
		case "IEND":
			return seenHeader && seenData && length == 0 && offset == len(data)
		default:
			// Reject color profiles, EXIF, text, animation, and every unknown
			// chunk. Re-encoding also handles otherwise clean palette images.
			return false
		}
	}
	return false
}

func cleanJPEG(data []byte) bool {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return false
	}
	seenFrame, seenScan := false, false
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			return false
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset == len(data) {
			return false
		}
		marker := data[offset]
		offset++
		if marker == 0xd9 {
			return seenFrame && seenScan && offset == len(data)
		}
		if marker == 0x00 || marker == 0xd8 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 ||
			marker >= 0xe1 && marker <= 0xef || marker == 0xfe {
			return false
		}
		if len(data)-offset < 2 {
			return false
		}
		length := int(binary.BigEndian.Uint16(data[offset:]))
		if length < 2 || length > len(data)-offset {
			return false
		}
		payload := data[offset+2 : offset+length]
		offset += length
		switch marker {
		case 0xe0: // Standard JFIF marker without a thumbnail.
			if len(payload) != 14 || !bytes.Equal(payload[:5], []byte("JFIF\x00")) || payload[12] != 0 || payload[13] != 0 {
				return false
			}
		case 0xc0, 0xc2: // Eight-bit, three-component RGB/YCbCr only.
			if seenFrame || len(payload) < 6 || payload[0] != 8 || payload[5] != 3 {
				return false
			}
			seenFrame = true
		case 0xda:
			if !seenFrame {
				return false
			}
			seenScan = true
			// Entropy data may contain byte-stuffed FF and restart markers.
			for offset < len(data) {
				if data[offset] != 0xff {
					offset++
					continue
				}
				if offset+1 == len(data) {
					return false
				}
				next := data[offset+1]
				if next == 0x00 || next >= 0xd0 && next <= 0xd7 {
					offset += 2
					continue
				}
				break
			}
		case 0xc4, 0xdb, 0xdd:
			// Huffman, quantization, and restart interval tables.
		default:
			return false
		}
	}
	return false
}

func cleanWebP(data []byte) bool {
	if len(data) < 20 || !bytes.Equal(data[:4], []byte("RIFF")) ||
		!bytes.Equal(data[8:12], []byte("WEBP")) || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return false
	}
	seenImage, seenExtended, seenAlpha, extendedAlpha := false, false, false, false
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return false
		}
		length := uint64(binary.LittleEndian.Uint32(data[offset+4:]))
		padded := length + length%2
		if padded > uint64(len(data)-offset-8) {
			return false
		}
		kind := string(data[offset : offset+4])
		chunk := data[offset+8 : offset+8+int(length)]
		offset += 8 + int(padded)
		switch kind {
		case "VP8X":
			if seenExtended || seenImage || len(chunk) != 10 || chunk[0]&^0x10 != 0 || chunk[1] != 0 || chunk[2] != 0 || chunk[3] != 0 {
				return false // ICC, EXIF, XMP, or animation flags.
			}
			seenExtended = true
			extendedAlpha = chunk[0]&0x10 != 0
		case "ALPH":
			if !seenExtended || seenImage || seenAlpha || !extendedAlpha {
				return false
			}
			seenAlpha = true
		case "VP8 ", "VP8L":
			if seenImage {
				return false
			}
			seenImage = true
		default:
			return false
		}
	}
	return seenImage
}
