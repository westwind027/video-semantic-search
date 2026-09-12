package api

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxPreviewDimension = 4096

type previewOptions struct {
	width     int
	height    int
	quality   int
	requested bool
	reencode  bool
}

func parsePreviewOptions(values url.Values) (previewOptions, error) {
	options := previewOptions{quality: 78}
	size, hasSize := queryValue(values, "size")
	if hasSize {
		switch strings.ToLower(strings.TrimSpace(size)) {
		case "", "original":
		case "small":
			options.width, options.height, options.requested = 320, 240, true
		case "tiny", "thumbnail":
			options.width, options.height, options.requested = 160, 120, true
		default:
			return previewOptions{}, fmt.Errorf("size must be original, small, or tiny")
		}
	}
	if raw, ok := queryValue(values, "width", "w"); ok {
		width, err := parsePreviewDimension("width", raw)
		if err != nil {
			return previewOptions{}, err
		}
		options.width = width
		options.requested = true
	}
	if raw, ok := queryValue(values, "height", "h"); ok {
		height, err := parsePreviewDimension("height", raw)
		if err != nil {
			return previewOptions{}, err
		}
		options.height = height
		options.requested = true
	}
	if raw, ok := queryValue(values, "quality"); ok {
		quality, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || quality < 1 || quality > 100 {
			return previewOptions{}, fmt.Errorf("quality must be an integer between 1 and 100")
		}
		options.quality = quality
		options.requested = true
		options.reencode = true
	}
	return options, nil
}

func queryValue(values url.Values, names ...string) (string, bool) {
	for _, name := range names {
		if raw, ok := values[name]; ok {
			if len(raw) == 0 {
				return "", true
			}
			return raw[0], true
		}
	}
	return "", false
}

func parsePreviewDimension(name, raw string) (int, error) {
	dimension, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || dimension < 1 || dimension > maxPreviewDimension {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", name, maxPreviewDimension)
	}
	return dimension, nil
}

func (s *Server) serveFramePreview(response http.ResponseWriter, request *http.Request, framePath string) {
	options, err := parsePreviewOptions(request.URL.Query())
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if !options.requested {
		http.ServeFile(response, request, framePath)
		return
	}

	file, err := os.Open(framePath)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	defer file.Close()
	source, _, err := image.Decode(file)
	if err != nil {
		writeError(response, http.StatusUnprocessableEntity, fmt.Errorf("decode preview: %w", err))
		return
	}
	width, height, resize := previewBounds(source.Bounds(), options.width, options.height)
	if !resize && !options.reencode {
		http.ServeFile(response, request, framePath)
		return
	}
	resized := source
	if resize {
		resized = resizePreview(source, width, height)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, resized, &jpeg.Options{Quality: options.quality}); err != nil {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("encode preview: %w", err))
		return
	}
	info, err := os.Stat(framePath)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "image/jpeg")
	response.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(response, request, filepath.Base(framePath), info.ModTime(), bytes.NewReader(encoded.Bytes()))
}

func previewBounds(bounds image.Rectangle, requestedWidth, requestedHeight int) (int, int, bool) {
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if sourceWidth < 1 || sourceHeight < 1 {
		return sourceWidth, sourceHeight, false
	}
	scale := 1.0
	if requestedWidth > 0 && requestedWidth < sourceWidth {
		scale = math.Min(scale, float64(requestedWidth)/float64(sourceWidth))
	}
	if requestedHeight > 0 && requestedHeight < sourceHeight {
		scale = math.Min(scale, float64(requestedHeight)/float64(sourceHeight))
	}
	if scale >= 1 {
		return sourceWidth, sourceHeight, false
	}
	width := maxInt(1, int(math.Round(float64(sourceWidth)*scale)))
	height := maxInt(1, int(math.Round(float64(sourceHeight)*scale)))
	return width, height, true
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func resizePreview(source image.Image, width, height int) image.Image {
	sourceBounds := source.Bounds()
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	xScale := float64(sourceBounds.Dx()) / float64(width)
	yScale := float64(sourceBounds.Dy()) / float64(height)
	for y := 0; y < height; y++ {
		sourceY := float64(sourceBounds.Min.Y) + (float64(y)+0.5)*yScale - 0.5
		if sourceY < float64(sourceBounds.Min.Y) {
			sourceY = float64(sourceBounds.Min.Y)
		}
		if sourceY > float64(sourceBounds.Max.Y-1) {
			sourceY = float64(sourceBounds.Max.Y - 1)
		}
		y0 := int(math.Floor(sourceY))
		y1 := minInt(y0+1, sourceBounds.Max.Y-1)
		yWeight := sourceY - float64(y0)
		for x := 0; x < width; x++ {
			sourceX := float64(sourceBounds.Min.X) + (float64(x)+0.5)*xScale - 0.5
			if sourceX < float64(sourceBounds.Min.X) {
				sourceX = float64(sourceBounds.Min.X)
			}
			if sourceX > float64(sourceBounds.Max.X-1) {
				sourceX = float64(sourceBounds.Max.X - 1)
			}
			x0 := int(math.Floor(sourceX))
			x1 := minInt(x0+1, sourceBounds.Max.X-1)
			xWeight := sourceX - float64(x0)
			top := blendPreviewColors(color.NRGBAModel.Convert(source.At(x0, y0)).(color.NRGBA), color.NRGBAModel.Convert(source.At(x1, y0)).(color.NRGBA), xWeight)
			bottom := blendPreviewColors(color.NRGBAModel.Convert(source.At(x0, y1)).(color.NRGBA), color.NRGBAModel.Convert(source.At(x1, y1)).(color.NRGBA), xWeight)
			destination.Set(x, y, blendPreviewColors(top, bottom, yWeight))
		}
	}
	return destination
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func blendPreviewColors(left, right color.NRGBA, weight float64) color.NRGBA {
	blend := func(first, second uint8) uint8 {
		value := float64(first) + (float64(second)-float64(first))*weight
		if value < 0 {
			return 0
		}
		if value > 255 {
			return 255
		}
		return uint8(math.Round(value))
	}
	return color.NRGBA{R: blend(left.R, right.R), G: blend(left.G, right.G), B: blend(left.B, right.B), A: blend(left.A, right.A)}
}
