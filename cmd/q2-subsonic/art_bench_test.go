package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// A row's cover as Navidrome sends one at size 64: a baseline JPEG already, which the firmware can
// take as it is.
func rowCover(tb testing.TB) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 85}); err != nil {
		tb.Fatal(err)
	}
	return b.Bytes()
}

func BenchmarkRowCover(b *testing.B) {
	in := rowCover(b)
	for i := 0; i < b.N; i++ {
		if _, err := rowBaseline(in); err != nil {
			b.Fatal(err)
		}
	}
}

// A row's cover that is a baseline JPEG already is kept as it came (the JFIF header added), not
// decoded and encoded again; anything else still is.
func TestARowCoverAlreadyBaselineIsKept(t *testing.T) {
	in := rowCover(t)
	out, err := rowBaseline(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out, in[2:]) {
		t.Fatal("decoded and encoded again")
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
}

// A first drawing of a long list queues artQueue covers at most, as later pages are held to it.
func TestAFirstDrawingsCoversAreCapped(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1, block: make(chan struct{})}, "art", true)
	var rows []q2spot.Row
	covers := map[string]string{}
	for i := 0; i < 800; i++ {
		ctx := fmt.Sprintf("al%d", i)
		rows = append(rows, q2spot.Row{Kind: "list", URI: "album" + artOp, Context: ctx})
		covers[ctx] = "cv" + ctx
	}
	d.art.want(rows, covers, true)
	d.art.mu.Lock()
	n := len(d.art.queue)
	d.art.mu.Unlock()
	if n > artQueue {
		t.Fatalf("%d covers queued", n)
	}
}
