package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// autoCheck runs a cheap syntax check for the file type. ran is false when
// there is no check for this kind of file.
func (a *Agent) autoCheck(ctx context.Context, path string) (msg string, ok, ran bool) {
	run := func(timeout time.Duration, name string, args ...string) (string, bool, bool) {
		if _, err := exec.LookPath(name); err != nil {
			return "", false, false
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, name, args...)
		cmd.Dir = a.cwd
		out, err := cmd.CombinedOutput()
		label := name + " " + strings.Join(args[:len(args)-1], " ")
		if err != nil {
			return strings.TrimSpace(label) + " failed:\n" + truncate(strings.TrimSpace(string(out)), 3000), false, true
		}
		return strings.TrimSpace(label) + " OK", true, true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
		return run(10*time.Second, "node", "--check", path)
	case ".json":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", false, false
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			line := ""
			if se, ok := err.(*json.SyntaxError); ok {
				line = fmt.Sprintf(" at line %d", strings.Count(string(b[:se.Offset]), "\n")+1)
			}
			return fmt.Sprintf("JSON invalid%s: %v", line, err), false, true
		}
		return "JSON valid", true, true
	case ".py":
		return run(15*time.Second, "python3", "-m", "py_compile", path)
	case ".ts", ".tsx", ".mts", ".cts":
		if !a.cfg.CheckTS || !a.env.HasTSConfig {
			return "", false, false
		}
		return run(90*time.Second, "npx", "--no-install", "tsc", "--noEmit", "-p", ".")
	}
	return "", false, false
}

// applyCheck appends the auto-check result to a write/edit result.
func (a *Agent) applyCheck(ctx context.Context, path string, res *ToolResult) {
	msg, ok, ran := a.autoCheck(ctx, path)
	if !ran {
		return
	}
	res.CheckOK = ok
	if ok {
		res.Output += "\nAuto-check: " + msg
		res.Detail = append(res.Detail, "✓ "+msg)
		return
	}
	res.Failed = true
	res.Output += "\nAuto-check FAILED: " + msg + "\nThe file is saved but broken. Fix it before continuing."
	res.Detail = append(res.Detail, lastLines("✗ "+msg, 6)...)
}
