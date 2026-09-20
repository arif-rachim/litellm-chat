package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// onePixelPNG is a real 1x1 PNG.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0a, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1,
	0x0d, 0x0a, 0x2d, 0xb4, 0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func TestMessageWithImageJSON(t *testing.T) {
	m := Message{Role: "user", Content: "apa ini?", Images: []Image{{Mime: "image/png", Data: onePixelPNG, Name: "x.png"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	parts, _ := got["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %v", got["content"])
	}
	text := parts[0].(map[string]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if text["text"] != "apa ini?" || !strings.HasPrefix(img, "data:image/png;base64,iVBOR") {
		t.Fatalf("parts: %v / %.40s", text, img)
	}
	// Without images the content stays a plain string.
	b, _ = json.Marshal(Message{Role: "user", Content: "halo"})
	if string(b) != `{"role":"user","content":"halo"}` {
		t.Fatalf("plain message changed: %s", b)
	}
}

func TestLoadImageAndPaths(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	os.WriteFile(png, onePixelPNG, 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "fake.png"), []byte("not an image at all"), 0o644)

	img, err := loadImage(png)
	if err != nil || img.Mime != "image/png" || img.Name != "shot.png" {
		t.Fatalf("loadImage: %+v %v", img, err)
	}
	if _, err := loadImage(filepath.Join(dir, "fake.png")); err == nil {
		t.Fatal("a file that is not an image should be rejected")
	}
	if _, err := loadImage(filepath.Join(dir, "missing.png")); err == nil {
		t.Fatal("missing file should error")
	}
	got := imagePathsIn("lihat " + png + " dan " + filepath.Join(dir, "notes.txt"))
	if len(got) != 1 || got[0] != png {
		t.Fatalf("imagePathsIn = %v", got)
	}
	if p := imagePathsIn("file://" + png); len(p) != 1 {
		t.Fatalf("file:// prefix not handled: %v", p)
	}
	if p := imagePathsIn(filepath.Join(dir, "tidak-ada.png")); len(p) != 0 {
		t.Fatalf("a path that does not exist is not an attachment: %v", p)
	}
}

func TestReadClipboard(t *testing.T) {
	old, oldLook := runCmd, lookPath
	t.Cleanup(func() { runCmd, lookPath = old, oldLook })

	// wl-paste present, holding an image
	lookPath = func(n string) (string, error) {
		if n == "wl-paste" {
			return "/usr/bin/wl-paste", nil
		}
		return "", errors.New("nope")
	}
	runCmd = func(_ context.Context, n string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "image/png" {
			return onePixelPNG, nil
		}
		return []byte("teks biasa"), nil
	}
	img, text, hint := readClipboard()
	if img == nil || text != "" || hint != "" {
		t.Fatalf("image clipboard: %v %q %q", img, text, hint)
	}

	// clipboard has text only
	runCmd = func(_ context.Context, n string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "image/png" {
			return nil, errors.New("no image")
		}
		return []byte("halo dari clipboard\n"), nil
	}
	if img, text, _ := readClipboard(); img != nil || !strings.Contains(text, "halo dari clipboard") {
		t.Fatalf("text clipboard: %v %q", img, text)
	}

	// no clipboard tool installed: tell the user what to install
	lookPath = func(string) (string, error) { return "", errors.New("nope") }
	if _, _, hint := readClipboard(); !strings.Contains(hint, "wl-clipboard") || !strings.Contains(hint, "/img") {
		t.Fatalf("hint: %q", hint)
	}
}

func TestImageSentAndDroppedWhenContextFills(t *testing.T) {
	a, f, _, _ := newFakeAgent(t, textReply("Itu gambar kotak merah."))
	a.Attach(Image{Mime: "image/png", Data: onePixelPNG, Name: "shot.png"})
	a.RunTurn(context.Background(), "apa ini?")
	b, _ := json.Marshal(f.reqs[0]["messages"])
	if !strings.Contains(string(b), "data:image/png;base64") {
		t.Fatal("image not sent to the model")
	}

	// Under context pressure the oldest images go first.
	a.cfg.Ctx = 500
	a.msgs = append(a.msgs, Message{Role: "user", Content: "dan ini?", Images: []Image{{Mime: "image/png", Data: onePixelPNG}}})
	a.turnStart = len(a.msgs) - 1
	a.fitContext()
	n := 0
	for _, m := range a.msgs {
		n += len(m.Images)
	}
	if n != 1 {
		t.Fatalf("only the newest image should survive, got %d", n)
	}
	if !strings.Contains(a.msgs[1].Content, "gambar lama dihapus") {
		t.Fatalf("no note left in place of the dropped image: %q", a.msgs[1].Content)
	}
}
