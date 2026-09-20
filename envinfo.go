package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Env is what the model knows about the machine and project before its first
// step, so it does not spend steps exploring.
type Env struct {
	OS, Shell, Cwd, Date string
	Tools                []string
	Git                  string
	Project              []string
	VerifyCmds           []string
	Tree                 []string
	Instructions         string
	InstructionsFile     string
	HasTSConfig          bool
}

var toolProbes = []struct {
	name string
	args []string
}{
	{"node", []string{"--version"}}, {"npm", []string{"--version"}}, {"pnpm", []string{"--version"}},
	{"yarn", []string{"--version"}}, {"bun", []string{"--version"}}, {"python3", []string{"--version"}},
	{"uv", []string{"--version"}}, {"git", []string{"--version"}}, {"rg", []string{"--version"}},
	{"go", []string{"version"}},
}

var reVersion = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func DetectEnv(cwd string) *Env {
	e := &Env{Cwd: cwd, Date: time.Now().Format("2006-01-02"), Shell: "bash"}
	e.OS = osName()
	e.Tools = detectTools()
	e.Git = gitInfo(cwd)
	e.detectProject()
	e.Tree = fileTree(cwd, 2, 60)
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(cwd, name)); err == nil {
			e.Instructions = truncate(strings.TrimSpace(string(b)), 2048)
			e.InstructionsFile = name
			break
		}
	}
	return e
}

func osName() string {
	name := runtime.GOOS
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				name = strings.Trim(v, `"`)
			}
		}
	}
	return fmt.Sprintf("%s (%s/%s)", name, runtime.GOOS, runtime.GOARCH)
}

func detectTools() []string {
	out := make([]string, len(toolProbes))
	var wg sync.WaitGroup
	for i, t := range toolProbes {
		if _, err := exec.LookPath(t.name); err != nil {
			continue
		}
		wg.Add(1)
		go func(i int, name string, args []string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			b, _ := exec.CommandContext(ctx, name, args...).Output()
			v := reVersion.FindString(string(b))
			out[i] = strings.TrimSpace(name + " " + v)
		}(i, t.name, t.args)
	}
	wg.Wait()
	var res []string
	for _, s := range out {
		if s != "" {
			res = append(res, s)
		}
	}
	return res
}

func gitInfo(cwd string) string {
	git := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		b, err := cmd.Output()
		return strings.TrimSpace(string(b)), err == nil
	}
	if _, ok := git("rev-parse", "--is-inside-work-tree"); !ok {
		return "not a git repository"
	}
	branch, _ := git("symbolic-ref", "--short", "HEAD")
	status, _ := git("status", "--porcelain")
	n := 0
	if status != "" {
		n = len(strings.Split(status, "\n"))
	}
	return fmt.Sprintf("branch %s, %d changed files", branch, n)
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func (e *Env) detectProject() {
	d := e.Cwd
	e.HasTSConfig = exists(d, "tsconfig.json")
	if b, err := os.ReadFile(filepath.Join(d, "package.json")); err == nil {
		var pkg struct {
			Name            string            `json:"name"`
			Type            string            `json:"type"`
			Scripts         map[string]string `json:"scripts"`
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
			Engines         map[string]string `json:"engines"`
			PackageManager  string            `json:"packageManager"`
		}
		_ = json.Unmarshal(b, &pkg)
		pm := "npm"
		switch {
		case exists(d, "pnpm-lock.yaml"):
			pm = "pnpm"
		case exists(d, "yarn.lock"):
			pm = "yarn"
		case exists(d, "bun.lockb"), exists(d, "bun.lock"):
			pm = "bun"
		case pkg.PackageManager != "":
			pm = strings.SplitN(pkg.PackageManager, "@", 2)[0]
		}
		mod := "CommonJS (require)"
		if pkg.Type == "module" {
			mod = "ESM (import/export)"
		}
		line := fmt.Sprintf("Node.js project %q, modules: %s, package manager: %s", pkg.Name, mod, pm)
		if v := pkg.Engines["node"]; v != "" {
			line += ", node " + v
		}
		if b, err := os.ReadFile(filepath.Join(d, ".nvmrc")); err == nil {
			line += ", .nvmrc " + strings.TrimSpace(string(b))
		}
		if e.HasTSConfig {
			line += ", TypeScript (tsconfig.json)"
		}
		if !exists(d, "node_modules") && len(pkg.Dependencies)+len(pkg.DevDependencies) > 0 {
			line += fmt.Sprintf(", node_modules missing (run `%s install`)", pm)
		}
		e.Project = append(e.Project, line)
		if len(pkg.Scripts) > 0 {
			e.Project = append(e.Project, fmt.Sprintf("scripts (run with `%s run <name>`):", pm))
			names := sortedKeys(pkg.Scripts)
			for i, n := range names {
				if i == 15 {
					e.Project = append(e.Project, fmt.Sprintf("  ... %d more", len(names)-15))
					break
				}
				cmd := pkg.Scripts[n]
				if len(cmd) > 80 {
					cmd = cmd[:80] + "…"
				}
				e.Project = append(e.Project, fmt.Sprintf("  %s: %s", n, cmd))
			}
			for _, n := range []string{"test", "typecheck", "lint", "build"} {
				if s, ok := pkg.Scripts[n]; ok && !strings.Contains(s, "no test specified") {
					if n == "test" {
						e.VerifyCmds = append(e.VerifyCmds, pm+" test")
					} else {
						e.VerifyCmds = append(e.VerifyCmds, pm+" run "+n)
					}
				}
			}
		}
		deps := append(sortedKeys(pkg.Dependencies), sortedKeys(pkg.DevDependencies)...)
		if len(deps) > 30 {
			deps = append(deps[:30], "…")
		}
		if len(deps) > 0 {
			e.Project = append(e.Project, "dependencies: "+strings.Join(deps, ", "))
		}
	}
	if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil {
		mod := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindStringSubmatch(string(b))
		gv := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindStringSubmatch(string(b))
		line := "Go module"
		if mod != nil {
			line += " " + mod[1]
		}
		if gv != nil {
			line += ", go " + gv[1]
		}
		e.Project = append(e.Project, line)
		e.VerifyCmds = append(e.VerifyCmds, "go build ./...", "go test ./...")
	}
	if b, err := os.ReadFile(filepath.Join(d, "pyproject.toml")); err == nil {
		line := "Python project (pyproject.toml)"
		if m := regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`).FindStringSubmatch(string(b)); m != nil {
			line = fmt.Sprintf("Python project %q (pyproject.toml)", m[1])
		}
		if exists(d, "uv.lock") {
			line += ", uses uv"
		}
		e.Project = append(e.Project, line)
		if strings.Contains(string(b), "pytest") {
			e.VerifyCmds = append(e.VerifyCmds, "python3 -m pytest")
		}
	} else if exists(d, "requirements.txt") {
		e.Project = append(e.Project, "Python project (requirements.txt)")
	}
	if b, err := os.ReadFile(filepath.Join(d, "Cargo.toml")); err == nil {
		line := "Rust crate"
		if m := regexp.MustCompile(`(?m)^name\s*=\s*"([^"]+)"`).FindStringSubmatch(string(b)); m != nil {
			line += " " + m[1]
		}
		e.Project = append(e.Project, line)
		e.VerifyCmds = append(e.VerifyCmds, "cargo build", "cargo test")
	}
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// fileTree lists entries up to depth levels deep, at most limit lines.
func fileTree(root string, depth, limit int) []string {
	var out []string
	more := 0
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, en := range entries {
			name := en.Name()
			if ignoredDirs[name] || (level > 0 && strings.HasPrefix(name, ".")) {
				continue
			}
			if len(out) >= limit {
				more++
				continue
			}
			indent := strings.Repeat("  ", level)
			if en.IsDir() {
				out = append(out, indent+name+"/")
				if level+1 < depth {
					walk(filepath.Join(dir, name), level+1)
				}
			} else {
				out = append(out, indent+name)
			}
		}
	}
	walk(root, 0)
	if more > 0 {
		out = append(out, fmt.Sprintf("... (+%d more)", more))
	}
	return out
}

// Block renders the environment for the system prompt.
func (e *Env) Block() string {
	var b strings.Builder
	b.WriteString("<env>\n")
	fmt.Fprintf(&b, "os: %s, shell: %s\ncwd: %s\ndate: %s\n", e.OS, e.Shell, e.Cwd, e.Date)
	if len(e.Tools) > 0 {
		fmt.Fprintf(&b, "installed: %s\n", strings.Join(e.Tools, ", "))
	}
	fmt.Fprintf(&b, "git: %s\n", e.Git)
	if len(e.Project) > 0 {
		b.WriteString("project:\n")
		for _, l := range e.Project {
			b.WriteString("  " + l + "\n")
		}
	} else {
		b.WriteString("project: none detected (no package.json/go.mod/pyproject.toml)\n")
	}
	if len(e.VerifyCmds) > 0 {
		fmt.Fprintf(&b, "verify with: %s\n", strings.Join(e.VerifyCmds, ", "))
	}
	b.WriteString("files:\n")
	if len(e.Tree) == 0 {
		b.WriteString("  (empty directory)\n")
	}
	for _, l := range e.Tree {
		b.WriteString("  " + l + "\n")
	}
	if e.Instructions != "" {
		fmt.Fprintf(&b, "project instructions (%s):\n%s\n", e.InstructionsFile, e.Instructions)
	}
	b.WriteString("</env>")
	return b.String()
}
