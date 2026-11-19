package controller

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
)

const fsCallTimeout = 30 * time.Second

// maxFsOptionLen bounds the short option strings of fs.read / fs.write /
// fs.upload_url (encoding name, octal mode), which are forwarded to the agent
// like the path.
const maxFsOptionLen = 16

func fsEntrySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"type":        map[string]any{"type": "string"},
			"size":        map[string]any{"type": "integer"},
			"mode":        map[string]any{"type": "string"},
			"mtime":       map[string]any{"type": "integer"},
			"is_symlink":  map[string]any{"type": "boolean"},
			"link_target": map[string]any{"type": "string"},
		},
		"required": []string{"name", "type", "size", "mode", "mtime"},
	}
}

func init() {
	registerMCPTool(&mcpTool{
		Name:        "fs.list",
		Description: "List entries of a directory on the target server.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id":   map[string]any{"type": "integer"},
				"path":        map[string]any{"type": "string", "description": "Absolute path."},
				"show_hidden": map[string]any{"type": "boolean"},
			},
			"required": []string{"server_id", "path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entries":   map[string]any{"type": "array", "items": fsEntrySchema()},
				"truncated": map[string]any{"type": "boolean"},
				"total":     map[string]any{"type": "integer"},
			},
			"required": []string{"entries"},
		},
		RequiredScope: model.ScopeServerRead,
		Handler:       handleFsList,
	})

	registerMCPTool(&mcpTool{
		Name:        "fs.read",
		Description: "Read a file. Default max 1MB; use offset/length for larger ranges, or fs.download_url for streaming up to 100MiB out-of-band.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id": map[string]any{"type": "integer"},
				"path":      map[string]any{"type": "string"},
				"offset":    map[string]any{"type": "integer", "minimum": 0},
				"length":    map[string]any{"type": "integer", "minimum": 1},
				"encoding":  map[string]any{"type": "string", "enum": []string{"utf8", "base64"}},
			},
			"required": []string{"server_id", "path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":   map[string]any{"type": "string"},
				"encoding":  map[string]any{"type": "string"},
				"size":      map[string]any{"type": "integer"},
				"sha256":    map[string]any{"type": "string"},
				"truncated": map[string]any{"type": "boolean"},
			},
			"required": []string{"content", "encoding", "size"},
		},
		RequiredScope: model.ScopeServerRead,
		Handler:       handleFsRead,
	})

	registerMCPTool(&mcpTool{
		Name:        "fs.write",
		Description: "Atomic write to a file. Supports utf8 / base64 content (max 1MiB inline; use fs.upload_url for larger files), optional sha256 optimistic lock, create_dirs.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id":       map[string]any{"type": "integer"},
				"path":            map[string]any{"type": "string"},
				"content":         map[string]any{"type": "string"},
				"encoding":        map[string]any{"type": "string", "enum": []string{"utf8", "base64"}},
				"mode":            map[string]any{"type": "string", "description": "Octal mode like '0644'."},
				"if_match_sha256": map[string]any{"type": "string"},
				"create_dirs":     map[string]any{"type": "boolean"},
			},
			"required": []string{"server_id", "path", "content"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"size":   map[string]any{"type": "integer"},
				"sha256": map[string]any{"type": "string"},
			},
			"required": []string{"size", "sha256"},
		},
		RequiredScope: model.ScopeServerWrite,
		Handler:       handleFsWrite,
	})

	registerMCPTool(&mcpTool{
		Name:        "fs.delete",
		Description: "Delete a file or directory. Pass recursive=true for non-empty directories.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id": map[string]any{"type": "integer"},
				"path":      map[string]any{"type": "string"},
				"recursive": map[string]any{"type": "boolean"},
			},
			"required": []string{"server_id", "path"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"deleted_count": map[string]any{"type": "integer"},
			},
			"required": []string{"deleted_count"},
		},
		RequiredScope: model.ScopeServerDelete,
		Handler:       handleFsDelete,
	})
}

type fsListArgs struct {
	ServerID   uint64 `json:"server_id"`
	Path       string `json:"path"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
}

func handleFsList(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsListArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	srv, err := requireServerAccess(c, args.ServerID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if err := validateTransferPath(args.Path); err != nil {
		return nil, err
	}
	out, err := rpc.CallAgent(c.Request.Context(), args.ServerID, model.TaskTypeFsList,
		model.FsListRequest{Path: args.Path, ShowHidden: args.ShowHidden}, fsCallTimeout)
	if err != nil {
		return nil, err
	}
	var res model.FsListResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return res, nil
}

type fsReadArgs struct {
	ServerID uint64 `json:"server_id"`
	Path     string `json:"path"`
	Offset   int64  `json:"offset,omitempty"`
	Length   int64  `json:"length,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

func handleFsRead(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsReadArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	srv, err := requireServerAccess(c, args.ServerID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if err := validateTransferPath(args.Path); err != nil {
		return nil, err
	}
	if args.Offset < 0 {
		return nil, errMCPInvalidArgs("offset must be >= 0")
	}
	if args.Length < 0 {
		return nil, errMCPInvalidArgs("length must be >= 0")
	}
	if len(args.Encoding) > maxFsOptionLen {
		return nil, errMCPInvalidArgs("encoding too long")
	}
	out, err := rpc.CallAgent(c.Request.Context(), args.ServerID, model.TaskTypeFsRead,
		model.FsReadRequest{
			Path:     args.Path,
			Offset:   args.Offset,
			Length:   args.Length,
			Encoding: args.Encoding,
		}, fsCallTimeout)
	if err != nil {
		return nil, err
	}
	var res model.FsReadResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return res, nil
}

type fsWriteArgs struct {
	ServerID      uint64 `json:"server_id"`
	Path          string `json:"path"`
	Content       string `json:"content"`
	Encoding      string `json:"encoding,omitempty"`
	Mode          string `json:"mode,omitempty"`
	IfMatchSHA256 string `json:"if_match_sha256,omitempty"`
	CreateDirs    bool   `json:"create_dirs,omitempty"`
}

func handleFsWrite(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsWriteArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	srv, err := requireServerAccess(c, args.ServerID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if err := validateTransferPath(args.Path); err != nil {
		return nil, err
	}
	if len(args.Content) > mcpMaxInlineArgBytes {
		return nil, errMCPInvalidArgs("content too large; use fs.upload_url")
	}
	if len(args.Encoding) > maxFsOptionLen {
		return nil, errMCPInvalidArgs("encoding too long")
	}
	if len(args.Mode) > maxFsOptionLen {
		return nil, errMCPInvalidArgs("mode too long")
	}
	if args.IfMatchSHA256 != "" {
		if _, decErr := hex.DecodeString(args.IfMatchSHA256); decErr != nil || len(args.IfMatchSHA256) != 64 {
			return nil, errMCPInvalidArgs("if_match_sha256 must be 64 hex chars")
		}
	}
	out, err := rpc.CallAgent(c.Request.Context(), args.ServerID, model.TaskTypeFsWrite,
		model.FsWriteRequest{
			Path:          args.Path,
			Content:       args.Content,
			Encoding:      args.Encoding,
			Mode:          args.Mode,
			IfMatchSHA256: args.IfMatchSHA256,
			CreateDirs:    args.CreateDirs,
		}, fsCallTimeout)
	if err != nil {
		return nil, err
	}
	var res model.FsWriteResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return res, nil
}

type fsDeleteArgs struct {
	ServerID  uint64 `json:"server_id"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

func handleFsDelete(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsDeleteArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	srv, err := requireServerAccess(c, args.ServerID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if err := validateTransferPath(args.Path); err != nil {
		return nil, err
	}
	out, err := rpc.CallAgent(c.Request.Context(), args.ServerID, model.TaskTypeFsDelete,
		model.FsDeleteRequest{Path: args.Path, Recursive: args.Recursive}, fsCallTimeout)
	if err != nil {
		return nil, err
	}
	var res model.FsDeleteResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return res, nil
}
