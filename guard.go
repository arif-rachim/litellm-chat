package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Guards for risky tool calls. A risky call always needs an explicit yes for
// that one call: --yolo and "always allow" do not cover it, and without a
// terminal to ask it is blocked. This is a safety net, not a sandbox.

var secretNames = map[string]bool{
	".netrc": true, ".npmrc": true, ".pypirc": true, ".git-credentials": true, ".htpasswd": true,
	"credentials": true, "credentials.json": true, "secrets.json": true, "secrets.yaml": true, "secrets.yml": true,
}

var secretExts = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true, ".keystore": true, ".kdbx": true}

var secretDirs = []string{".ssh", ".aws", ".gnupg", ".kube", ".docker"}

var envTemplates = map[string]bool{".env.example": true, ".env.sample": true, ".env.template": true, ".env.dist": true}

// isSecretPath reports whether a path looks like a credentials file.
func isSecretPath(p string) bool {
	p = filepath.ToSlash(p)
	base := strings.ToLower(filepath.Base(p))
	if base == ".env" || (strings.HasPrefix(base, ".env.") && !envTemplates[base]) {
		return true
	}
	if secretNames[base] || secretExts[filepath.Ext(base)] {
		return true
	}
	for _, k := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(base, k) && !strings.HasSuffix(base, ".pub") {
			return true
		}
	}
	for _, seg := range strings.Split(p, "/") {
		for _, d := range secretDirs {
			if seg == d {
				return true
			}
		}
	}
	return false
}

// realPath resolves symlinks; for a path that does not exist yet it resolves
// the nearest existing parent.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for dir != p {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		p = dir
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	return p
}

func within(p, root string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}

// pathRisk explains why touching p needs confirmation, or returns "".
func (a *Agent) pathRisk(p string) string {
	real := realPath(p)
	if !within(real, realPath(a.cwd)) {
		if home, err := os.UserHomeDir(); err == nil && within(real, home) {
			real = "~/" + strings.TrimPrefix(real, home+"/")
		}
		return "di luar folder proyek: " + real
	}
	if isSecretPath(real) {
		return "file rahasia: " + a.rel(real)
	}
	return ""
}

var dangerousCmds = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\brm\s+(-\S*[rRf]|--recursive|--force)`), "hapus paksa/rekursif (rm -r/-f)"},
	{regexp.MustCompile(`(^|[;&|]\s*|\s)(sudo|doas|su)\s`), "hak akses root (sudo)"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard|\bgit\s+clean\s+-\S*f|\bgit\s+branch\s+-D\b|\bgit\s+checkout\s+--\s`), "membuang perubahan git"},
	{regexp.MustCompile(`\bgit\s+push\b`), "git push ke remote"},
	{regexp.MustCompile(`\b(npm|pnpm|yarn)\s+publish\b`), "publish package"},
	{regexp.MustCompile(`(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`), "menjalankan script dari internet (curl | sh)"},
	{regexp.MustCompile(`\b(mkfs\S*|dd|shred|wipefs|fdisk|parted)\s`), "operasi disk"},
	{regexp.MustCompile(`\b(chmod|chown)\s+-\S*R`), "ubah izin rekursif"},
	{regexp.MustCompile(`\b(shutdown|reboot|poweroff|halt)\b|\bsystemctl\s+(stop|disable|mask|restart)\b`), "mengubah sistem"},
	{regexp.MustCompile(`\bkill\s+-9\s+-1\b|\b(pkill|killall)\s`), "mematikan banyak proses"},
	{regexp.MustCompile(`:\(\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|hd)`), "menulis langsung ke disk"},
}

// commandRisk explains why a bash command needs confirmation, or returns "".
func commandRisk(cmd string) string {
	for _, d := range dangerousCmds {
		if d.re.MatchString(cmd) {
			return d.reason
		}
	}
	tokens := strings.FieldsFunc(cmd, func(r rune) bool {
		return strings.ContainsRune(" \t\n\"'`=<>;|&(){}", r)
	})
	for _, t := range tokens {
		if isSecretPath(t) {
			return "menyentuh file rahasia: " + t
		}
	}
	return ""
}

// toolRisk returns why this call needs a per-call confirmation, or "".
func (a *Agent) toolRisk(name string, args map[string]any) string {
	switch name {
	case "read_file", "list_files", "write_file", "edit_file":
		p := str(args, "path")
		if p == "" {
			p = "."
		}
		return a.pathRisk(a.resolve(p))
	case "bash":
		return commandRisk(str(args, "command"))
	}
	return ""
}
