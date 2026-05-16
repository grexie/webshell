package gui

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"
)

const (
	dirtyTileSize           = 64
	maxDirtyRects           = 32
	fullFrameAreaThreshold  = 0.70
	pixelDifferenceBoundary = 32
)

type frameUpdate struct {
	full         bool
	screenWidth  int
	screenHeight int
	x            int
	y            int
	width        int
	height       int
	data         []byte
}

type dirtyEncoder struct {
	previous *image.RGBA
	width    int
	height   int
	quality  int
}

func newDirtyEncoder(quality int) *dirtyEncoder {
	return &dirtyEncoder{
		quality: clamp(quality, 1, 100),
	}
}

func (e *dirtyEncoder) Reset() {
	e.previous = nil
	e.width = 0
	e.height = 0
}

func (e *dirtyEncoder) Encode(frame frameResult) ([]frameUpdate, error) {
	current, err := decodeJPEGToRGBA(frame.data)
	if err != nil {
		e.Reset()
		return []frameUpdate{fullFrameUpdate(frame.width, frame.height, frame.data)}, nil
	}

	if e.previous == nil || e.width != frame.width || e.height != frame.height {
		e.previous = current
		e.width = frame.width
		e.height = frame.height
		return []frameUpdate{fullFrameUpdate(frame.width, frame.height, frame.data)}, nil
	}

	rects := dirtyRects(e.previous, current)
	e.previous = current
	if len(rects) == 0 {
		return nil, nil
	}

	if shouldSendFullFrame(rects, frame.width, frame.height) {
		return []frameUpdate{fullFrameUpdate(frame.width, frame.height, frame.data)}, nil
	}

	updates := make([]frameUpdate, 0, len(rects))
	for _, rect := range rects {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, current.SubImage(rect), &jpeg.Options{Quality: e.quality}); err != nil {
			return nil, err
		}
		updates = append(updates, frameUpdate{
			screenWidth:  frame.width,
			screenHeight: frame.height,
			x:            rect.Min.X,
			y:            rect.Min.Y,
			width:        rect.Dx(),
			height:       rect.Dy(),
			data:         buf.Bytes(),
		})
	}
	return updates, nil
}

func fullFrameUpdate(width, height int, data []byte) frameUpdate {
	return frameUpdate{
		full:         true,
		screenWidth:  width,
		screenHeight: height,
		width:        width,
		height:       height,
		data:         data,
	}
}

func decodeJPEGToRGBA(data []byte) (*image.RGBA, error) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	return rgba, nil
}

func dirtyRects(previous, current *image.RGBA) []image.Rectangle {
	bounds := current.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	tilesX := ceilDiv(width, dirtyTileSize)
	tilesY := ceilDiv(height, dirtyTileSize)
	dirty := make([]bool, tilesX*tilesY)

	for tileY := 0; tileY < tilesY; tileY++ {
		for tileX := 0; tileX < tilesX; tileX++ {
			rect := image.Rect(
				tileX*dirtyTileSize,
				tileY*dirtyTileSize,
				min((tileX+1)*dirtyTileSize, width),
				min((tileY+1)*dirtyTileSize, height),
			)
			if tileChanged(previous, current, rect) {
				dirty[tileY*tilesX+tileX] = true
			}
		}
	}

	visited := make([]bool, len(dirty))
	rects := make([]image.Rectangle, 0)
	for tileY := 0; tileY < tilesY; tileY++ {
		for tileX := 0; tileX < tilesX; tileX++ {
			index := tileY*tilesX + tileX
			if !dirty[index] || visited[index] {
				continue
			}
			rects = append(rects, componentRect(dirty, visited, tilesX, tilesY, tileX, tileY, width, height))
		}
	}
	return rects
}

func tileChanged(previous, current *image.RGBA, rect image.Rectangle) bool {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		prevOffset := previous.PixOffset(rect.Min.X, y)
		currOffset := current.PixOffset(rect.Min.X, y)
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if pixelChanged(previous.Pix[prevOffset:prevOffset+4], current.Pix[currOffset:currOffset+4]) {
				return true
			}
			prevOffset += 4
			currOffset += 4
		}
	}
	return false
}

func pixelChanged(previous, current []byte) bool {
	diff := absInt(int(previous[0])-int(current[0])) +
		absInt(int(previous[1])-int(current[1])) +
		absInt(int(previous[2])-int(current[2]))
	return diff > pixelDifferenceBoundary
}

func componentRect(dirty, visited []bool, tilesX, tilesY, startX, startY, width, height int) image.Rectangle {
	type tile struct {
		x int
		y int
	}

	stack := []tile{{x: startX, y: startY}}
	minX, minY := startX, startY
	maxX, maxY := startX, startY
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]

		if current.x < 0 || current.x >= tilesX || current.y < 0 || current.y >= tilesY {
			continue
		}
		index := current.y*tilesX + current.x
		if visited[index] || !dirty[index] {
			continue
		}
		visited[index] = true
		minX = min(minX, current.x)
		minY = min(minY, current.y)
		maxX = max(maxX, current.x)
		maxY = max(maxY, current.y)

		stack = append(stack,
			tile{x: current.x + 1, y: current.y},
			tile{x: current.x - 1, y: current.y},
			tile{x: current.x, y: current.y + 1},
			tile{x: current.x, y: current.y - 1},
		)
	}

	return image.Rect(
		minX*dirtyTileSize,
		minY*dirtyTileSize,
		min((maxX+1)*dirtyTileSize, width),
		min((maxY+1)*dirtyTileSize, height),
	)
}

func shouldSendFullFrame(rects []image.Rectangle, width, height int) bool {
	if len(rects) > maxDirtyRects {
		return true
	}

	area := 0
	for _, rect := range rects {
		area += rect.Dx() * rect.Dy()
	}
	return float64(area) > float64(width*height)*fullFrameAreaThreshold
}

func ceilDiv(value, divisor int) int {
	return (value + divisor - 1) / divisor
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
