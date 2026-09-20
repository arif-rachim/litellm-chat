package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Images the model can be sent. The terminal never hands us image bytes on
// stdin, so they come from the system clipboard, a pasted path, or /img.

const maxImageBytes = 5 << 20

type Image struct {
	Mime string
	Data []byte
	Name string
}

func (i Image) label() string {
	size := fmt.Sprintf("%d KB", len(i.Data)/1024)
	if len(i.Data) < 1024 {
		size = fmt.Sprintf("%d B", len(i.Data))
	}
	return fmt.Sprintf("%s (%s, %s)", i.Name, strings.TrimPrefix(i.Mime, "image/"), size)
}

var imageExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// loadImage reads an image file, checking its real type rather than trusting
// the extension.
func loadImage(path string) (Image, error) {
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, path[2:])
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return Image{}, err
	}
	if st.Size() > maxImageBytes {
		return Image{}, fmt.Errorf("gambar %d KB, batasnya %d KB", st.Size()/1024, maxImageBytes/1024)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Image{}, err
	}
	return imageFromBytes(b, filepath.Base(path))
}

func imageFromBytes(b []byte, name string) (Image, error) {
	if len(b) == 0 {
		return Image{}, fmt.Errorf("kosong")
	}
	if len(b) > maxImageBytes {
		return Image{}, fmt.Errorf("gambar %d KB, batasnya %d KB", len(b)/1024, maxImageBytes/1024)
	}
	mime := strings.SplitN(http.DetectContentType(b), ";", 2)[0]
	if !strings.HasPrefix(mime, "image/") {
		return Image{}, fmt.Errorf("bukan gambar (%s)", mime)
	}
	return Image{Mime: mime, Data: b, Name: name}, nil
}

// clipTool is one way to read the clipboard; the first installed one wins.
type clipTool struct {
	name      string
	imageArgs []string
	textArgs  []string
	install   string
}

var clipTools = []clipTool{
	{"wl-paste", []string{"--no-newline", "--type", "image/png"}, []string{"--no-newline"}, "sudo apt install wl-clipboard"},
	{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, []string{"-selection", "clipboard", "-o"}, "sudo apt install xclip"},
	{"pbpaste", nil, nil, ""}, // macOS: text only
}

// runCmd is replaced in tests.
var runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

var lookPath = exec.LookPath

// readClipboard returns an image from the clipboard, or its text, or a hint
// about what is missing.
func readClipboard() (*Image, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var installed bool
	for _, t := range clipTools {
		if _, err := lookPath(t.name); err != nil {
			continue
		}
		installed = true
		if t.imageArgs != nil {
			if b, err := runCmd(ctx, t.name, t.imageArgs...); err == nil && len(b) > 0 {
				if img, err := imageFromBytes(b, "clipboard.png"); err == nil {
					return &img, "", ""
				}
			}
		}
		if t.textArgs != nil || t.name == "pbpaste" {
			if b, err := runCmd(ctx, t.name, t.textArgs...); err == nil {
				return nil, string(b), ""
			}
		}
	}
	if installed {
		return nil, "", "clipboard kosong atau tidak berisi gambar/teks"
	}
	return nil, "", "clipboard butuh wl-clipboard atau xclip (" + clipTools[0].install + "); atau ketik /img <path>"
}

// imagePathsIn picks out paths to existing image files in a line of text, so a
// pasted or dragged-in file just works.
func imagePathsIn(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' }) {
		tok = strings.TrimPrefix(tok, "file://")
		if imageExt[strings.ToLower(filepath.Ext(tok))] == "" {
			continue
		}
		p := tok
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, tok)
		}
	}
	return out
}
