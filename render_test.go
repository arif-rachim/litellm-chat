package main

import (
	"math/rand"
	"strings"
	"testing"
)

const sampleMD = "# Judul\nIni **tebal** dan `kode` di sini.\n- satu\n- dua **x**\n\n```js\nconst a = 1; // **bukan bold**\n```\n1. nomor\n* bintang\nakhir*"

func render(color bool, chunks []string) string {
	var sb strings.Builder
	m := NewMarkdown(&sb, color)
	for _, c := range chunks {
		m.Write(c)
	}
	m.Flush()
	return sb.String()
}

func TestMarkdownSplitIndependent(t *testing.T) {
	want := render(true, []string{sampleMD})
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		var chunks []string
		s := sampleMD
		for s != "" {
			n := 1 + rng.Intn(5)
			if n > len(s) {
				n = len(s)
			}
			chunks = append(chunks, s[:n])
			s = s[n:]
		}
		if got := render(true, chunks); got != want {
			t.Fatalf("split %q\ngot  %q\nwant %q", chunks, got, want)
		}
	}
}

func TestMarkdownRendering(t *testing.T) {
	out := render(true, []string{sampleMD})
	for _, want := range []string{
		sgrHeading + "Judul",
		sgrBold + "tebal",
		sgrCyan + "kode",
		"• satu",
		"┌ js",
		"│ " + sgrReset + "const a = 1; // **bukan bold**", // code is left untouched
		"1. nomor",
		"• bintang",
		"akhir*",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%q", want, out)
		}
	}
	if strings.Contains(out, "# Judul") || strings.Contains(out, "```") {
		t.Errorf("markdown markers left in output: %q", out)
	}
}

func TestMarkdownRawPassthrough(t *testing.T) {
	if got := render(false, []string{sampleMD[:10], sampleMD[10:]}); got != sampleMD {
		t.Fatalf("raw mode changed text: %q", got)
	}
}
