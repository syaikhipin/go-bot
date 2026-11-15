package attachments

import (
	"bytes"
	"encoding/binary"
	"image"
)

func imageOrientation(data []byte, mediaType string) int {
	switch mediaType {
	case "image/jpeg":
		for offset := 2; offset+4 <= len(data) && data[offset] == 0xff; {
			marker := data[offset+1]
			if marker == 0xda || marker == 0xd9 {
				break
			}
			size := int(binary.BigEndian.Uint16(data[offset+2:]))
			end := offset + 2 + size
			if size < 2 || end > len(data) {
				break
			}
			if payload, ok := bytes.CutPrefix(data[offset+4:end], []byte("Exif\x00\x00")); marker == 0xe1 && ok {
				return tiffOrientation(payload)
			}
			offset = end
		}
	case "image/png":
		for offset := 8; offset+12 <= len(data); {
			size := int(binary.BigEndian.Uint32(data[offset:]))
			if size > len(data)-offset-12 {
				break
			}
			if string(data[offset+4:offset+8]) == "eXIf" {
				return tiffOrientation(data[offset+8 : offset+8+size])
			}
			offset += 12 + size
		}
	case "image/webp":
		for offset := 12; offset+8 <= len(data); {
			size := int(binary.LittleEndian.Uint32(data[offset+4:]))
			if size > len(data)-offset-8 {
				break
			}
			if string(data[offset:offset+4]) == "EXIF" {
				payload := data[offset+8 : offset+8+size]
				if trimmed, ok := bytes.CutPrefix(payload, []byte("Exif\x00\x00")); ok {
					payload = trimmed
				}
				return tiffOrientation(payload)
			}
			offset += 8 + size + size%2
		}
	}
	return 1
}

func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(data[2:]) != 42 {
		return 1
	}
	index := int(order.Uint32(data[4:]))
	if index < 8 || index > len(data)-2 {
		return 1
	}
	count := int(order.Uint16(data[index:]))
	index += 2
	for i := 0; i < count && index <= len(data)-12; i, index = i+1, index+12 {
		entry := data[index : index+12]
		if order.Uint16(entry) == 0x0112 && order.Uint16(entry[2:]) == 3 && order.Uint32(entry[4:]) == 1 {
			if value := int(order.Uint16(entry[8:])); value >= 1 && value <= 8 {
				return value
			}
		}
	}
	return 1
}

func orientImage(source image.Image, orientation int) image.Image {
	if orientation == 1 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := width, height
	if orientation >= 5 {
		outWidth, outHeight = height, width
	}
	result := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = width - 1 - x
			case 3:
				dx, dy = width-1-x, height-1-y
			case 4:
				dy = height - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = height-1-y, x
			case 7:
				dx, dy = height-1-y, width-1-x
			case 8:
				dx, dy = y, width-1-x
			}
			result.Set(dx, dy, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return result
}
