package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
)

// These are EXEMPLAR read tools proving the Tool shape end-to-end (typed Decode,
// risk class, path containment + secret guard, ToolResult envelope). Full tool
// families live in internal/tools/<group>; these stay here as the canonical
// pattern the registry's own tests exercise.

const maxReadBytes = 256 * 1024 // cap a single read so the audit/conv history can't balloon

// fsReadArgs is the fs.read argument shape.
type fsReadArgs struct {
	Path string `json:"path"`
}

var fsReadSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string", "description": "Project-relative file path to read." }
  }
}`)

// NewFsReadTool builds the fs.read tool (risk read). It refuses to read
// credential-bearing files (secret guard) and enforces project-root containment.
func NewFsReadTool() *Tool {
	return &Tool{
		Name:        "fs.read",
		Description: "Read a UTF-8 text file inside the project. Refuses credential files.",
		Risk:        domain.RiskRead,
		Schema:      fsReadSchema,
		Decode:      StrictDecoder(func() any { return &fsReadArgs{} }),
		Handle:      handleFsRead,
	}
}

func handleFsRead(_ context.Context, args json.RawMessage, tctx *ToolContext) ToolResult {
	var a fsReadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Fail(codeInvalidArgs, "Invalid arguments for fs.read: "+err.Error())
	}
	if a.Path == "" {
		return Fail(codeInvalidArgs, "fs.read: path is required")
	}
	// Secret guard FIRST (cheap, and the strongest invariant): never surface a
	// credential file's contents into the durable audit log / conversation history.
	if safety.IsSensitivePath(a.Path) {
		return Fail(domain.CodeDenied,
			fmt.Sprintf("Refusing to read %s: it looks like a credential file.", a.Path),
			Unrecoverable())
	}
	abs, err := safety.ResolveInsideProject(tctx.ProjectPath, a.Path)
	if err != nil {
		return Fail(domain.CodeDenied, err.Error(), Unrecoverable())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Fail(domain.CodeNotFound, fmt.Sprintf("fs.read: %s", err.Error()))
	}
	if info.IsDir() {
		return Fail(domain.CodeValidation, fmt.Sprintf("fs.read: %s is a directory (use fs.list)", a.Path))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Fail(domain.CodeInternal, fmt.Sprintf("fs.read: %s", err.Error()))
	}
	truncated := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncated = true
	}
	return Ok(fmt.Sprintf("Read %s (%d bytes%s).", a.Path, len(data), truncatedNote(truncated)),
		map[string]any{
			"path":      a.Path,
			"content":   string(data),
			"truncated": truncated,
		})
}

// fsListArgs is the fs.list argument shape.
type fsListArgs struct {
	Path string `json:"path"`
}

var fsListSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "description": "Project-relative directory (default: project root)." }
  }
}`)

// NewFsListTool builds the fs.list tool (risk read). Lists immediate entries,
// hiding credential-bearing names from the listing.
func NewFsListTool() *Tool {
	return &Tool{
		Name:        "fs.list",
		Description: "List the immediate entries of a directory inside the project.",
		Risk:        domain.RiskRead,
		Schema:      fsListSchema,
		Decode:      StrictDecoder(func() any { return &fsListArgs{} }),
		Handle:      handleFsList,
	}
}

func handleFsList(_ context.Context, args json.RawMessage, tctx *ToolContext) ToolResult {
	var a fsListArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Fail(codeInvalidArgs, "Invalid arguments for fs.list: "+err.Error())
	}
	rel := a.Path
	if rel == "" {
		rel = "."
	}
	abs, err := safety.ResolveInsideProject(tctx.ProjectPath, rel)
	if err != nil {
		return Fail(domain.CodeDenied, err.Error(), Unrecoverable())
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return Fail(domain.CodeNotFound, fmt.Sprintf("fs.list: %s", err.Error()))
	}
	type entry struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		// Hide credential-bearing names so a directory listing can't leak that a
		// secret exists by name (defense in depth with the read-time guard).
		if safety.IsSensitiveSegment(filepath.Base(rel)) || safety.IsSensitivePath(e.Name()) {
			continue
		}
		out = append(out, entry{Name: e.Name(), Dir: e.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return Ok(fmt.Sprintf("Listed %s (%d entries).", rel, len(out)),
		map[string]any{"path": rel, "entries": out})
}

func truncatedNote(t bool) string {
	if t {
		return ", truncated"
	}
	return ""
}
