package gui

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

func TestDirtyEncoderSendsFullFirstFrame(t *testing.T) {
	encoder := newDirtyEncoder(60)
	frame := frameResult{width: 128, height: 128, data: testJPEG(t, nil)}

	updates, err := encoder.Encode(frame)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if len(updates) != 1 || !updates[0].full {
		t.Fatalf("Encode() = %#v, want one full frame", updates)
	}
}

func TestDirtyEncoderSkipsUnchangedFrame(t *testing.T) {
	encoder := newDirtyEncoder(60)
	frame := frameResult{width: 128, height: 128, data: testJPEG(t, nil)}

	if _, err := encoder.Encode(frame); err != nil {
		t.Fatalf("first Encode() error = %v", err)
	}
	updates, err := encoder.Encode(frame)
	if err != nil {
		t.Fatalf("second Encode() error = %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("second Encode() = %#v, want no updates", updates)
	}
}

func TestDirtyEncoderSendsChangedRect(t *testing.T) {
	encoder := newDirtyEncoder(60)
	first := frameResult{width: 128, height: 128, data: testJPEG(t, nil)}
	second := frameResult{
		width:  128,
		height: 128,
		data: testJPEG(t, func(img *image.RGBA) {
			draw.Draw(img, image.Rect(8, 8, 24, 24), image.NewUniform(color.White), image.Point{}, draw.Src)
		}),
	}

	if _, err := encoder.Encode(first); err != nil {
		t.Fatalf("first Encode() error = %v", err)
	}
	updates, err := encoder.Encode(second)
	if err != nil {
		t.Fatalf("second Encode() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("second Encode() returned %d updates, want 1", len(updates))
	}
	update := updates[0]
	if update.full {
		t.Fatalf("second Encode() sent full frame, want rect")
	}
	if update.x != 0 || update.y != 0 || update.width != dirtyTileSize || update.height != dirtyTileSize {
		t.Fatalf("dirty rect = (%d,%d %dx%d), want (0,0 %dx%d)", update.x, update.y, update.width, update.height, dirtyTileSize, dirtyTileSize)
	}
	if len(update.data) == 0 {
		t.Fatal("dirty rect JPEG is empty")
	}
}

func testJPEG(t *testing.T, mutate func(*image.RGBA)) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	if mutate != nil {
		mutate(img)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg.Encode() error = %v", err)
	}
	return buf.Bytes()
}
