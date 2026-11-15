package attachments

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"

	"github.com/gen2brain/webp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	MaxImageSourceBytes    = 50 << 20
	maxSourcePixels        = 64_000_000
	maxSourceDimension     = 65535
	maxNormalizedDimension = 8192
	maxNormalizedBytes     = 4 << 20
	maxNormalizedPixels    = 2048 * 2048
)

type normalizedImage struct {
	Data                          []byte
	MIME                          string
	Width, Height                 int
	OriginalWidth, OriginalHeight int
}

func supportedMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func inspectImage(data []byte) (width, height int, mime string, err error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", fmt.Errorf("decode image header: %w", err)
	}
	switch format {
	case "png", "jpeg", "webp", "gif":
		mime = "image/" + format
	default:
		return 0, 0, "", errors.New("image format must be PNG, JPEG, WebP, or GIF")
	}
	return config.Width, config.Height, mime, nil
}

func normalizeImage(source []byte, declaredMIME string) (normalizedImage, error) {
	if len(source) == 0 || len(source) > MaxImageSourceBytes {
		return normalizedImage{}, errors.New("image exceeds the 50 MiB source byte limit")
	}
	width, height, actualMIME, err := inspectImage(source)
	if err != nil {
		return normalizedImage{}, err
	}
	if declaredMIME != "" && declaredMIME != actualMIME {
		return normalizedImage{}, fmt.Errorf("image bytes are %s, not declared %s", actualMIME, declaredMIME)
	}
	if width <= 0 || height <= 0 || width > maxSourceDimension || height > maxSourceDimension || int64(width)*int64(height) > maxSourcePixels {
		return normalizedImage{}, errors.New("image source dimensions exceed the 65535-pixel side or 64-million-pixel limit")
	}
	decoded, format, err := image.Decode(bytes.NewReader(source))
	if err != nil || "image/"+format != actualMIME {
		return normalizedImage{}, errors.New("image bytes do not decode as their declared format")
	}
	// A stored normalized object is already a safe request source. Preserve its
	// exact bytes so readimage of an offload path keeps the same attachment ID.
	orientation := imageOrientation(source, actualMIME)
	if orientation == 1 && width <= maxNormalizedDimension && height <= maxNormalizedDimension &&
		canPassThroughImage(source, actualMIME, width, height) {
		return normalizedImage{Data: source, MIME: actualMIME, Width: width, Height: height, OriginalWidth: width, OriginalHeight: height}, nil
	}
	// Normalization strips source metadata and bounds the pixels retained in
	// every future model request. Alpha images use WebP and opaque images use
	// JPEG; both remain provider-independent.
	decoded = orientImage(decoded, orientation)
	width, height = decoded.Bounds().Dx(), decoded.Bounds().Dy()
	originalWidth, originalHeight := width, height
	scale := min(1.0, math.Sqrt(float64(maxNormalizedPixels)/float64(int64(width)*int64(height))),
		float64(maxNormalizedDimension)/float64(max(width, height)))
	if scale < 1 {
		width = max(1, int(math.Floor(float64(width)*scale)))
		height = max(1, int(math.Floor(float64(height)*scale)))
		decoded = resized(decoded, width, height)
	}
	data, mime, err := encodeNormalized(decoded, maxNormalizedBytes, encodedAlphaPlane(source, actualMIME))
	if err != nil {
		return normalizedImage{}, err
	}
	return normalizedImage{Data: data, MIME: mime, Width: width, Height: height, OriginalWidth: originalWidth, OriginalHeight: originalHeight}, nil
}

func resized(source image.Image, width, height int) image.Image {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(target, target.Bounds(), source, source.Bounds(), draw.Over, nil)
	return target
}

func hasAlpha(source image.Image) bool {
	bounds := source.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := source.At(x, y).RGBA()
			if alpha != 0xffff {
				return true
			}
		}
	}
	return false
}

func encodeNormalized(source image.Image, targetBytes int, preferWebP bool) ([]byte, string, error) {
	useWebP := preferWebP || hasAlpha(source)
	mediaType := "image/jpeg"
	if useWebP {
		mediaType = "image/webp"
	}
	var smallest []byte
	for _, quality := range []int{85, 75, 60} {
		var output bytes.Buffer
		var err error
		if useWebP {
			err = webp.Encode(&output, source, webp.Options{Quality: quality, Method: webp.DefaultMethod})
		} else {
			err = jpeg.Encode(&output, source, &jpeg.Options{Quality: quality})
		}
		if err != nil {
			return nil, "", err
		}
		data := output.Bytes()
		if len(smallest) == 0 || len(data) < len(smallest) {
			smallest = data
		}
		if len(data) <= targetBytes {
			return data, mediaType, nil
		}
	}
	return smallest, mediaType, nil
}

// A source may declare an alpha plane whose pixels happen to be opaque.
func encodedAlphaPlane(data []byte, mediaType string) bool {
	switch mediaType {
	case "image/png":
		return len(data) > 25 && (data[25] == 4 || data[25] == 6)
	case "image/webp":
		return len(data) > 20 && string(data[12:16]) == "VP8X" && data[20]&0x10 != 0
	}
	return false
}
