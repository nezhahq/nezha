package controller

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
)

// server.exec — 非交互一次性命令。
// 协议约束（agent 端强制）：
//   - 不开 pty
//   - 默认 30s 超时，硬上限 300s
//   - stdout/stderr 各自最多 64KB（默认），硬上限 1MB
//   - 受 agent 配置 DisableCommandExecute 影响
//   - 命令返回或超时时，agent 会回收整个进程组/JobObject：'cmd &'、nohup、
//     disown 这类普通后台进程都会被一并杀掉。要留长驻进程必须脱离会话
//     （setsid / screen -dmS / tmux new -d / systemd-run；Windows 需 breakaway）。
//
// LLM 要用 shell 特性（管道、重定向）必须显式传 cmd="sh" args=["-c","..."]，
// 这样审计日志能完整记录被执行的指令。
const mcpExecMaxTimeoutSec uint32 = 300

type execArgs struct {
	ServerID       uint64            `json:"server_id"`
	Cmd            string            `json:"cmd"`
	Args           []string          `json:"args,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds uint32            `json:"timeout_seconds,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	MaxOutputBytes uint32            `json:"max_output_bytes,omitempty"`
}

func init() {
	registerMCPTool(&mcpTool{
		Name:        "server.exec",
		Description: "Run a non-interactive command on the target server and return stdout/stderr/exit_code. No pty. Use cmd='sh' args=['-c', '...'] for shell features. The entire process tree is killed when the command returns or times out, so plain background jobs ('cmd &', nohup, disown) do NOT survive; to leave a process running after the call, fully detach it from the session (e.g. setsid, 'screen -dmS', 'tmux new -d', systemd-run; on Windows it must break away from the job object).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id":        map[string]any{"type": "integer"},
				"cmd":              map[string]any{"type": "string"},
				"args":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"cwd":              map[string]any{"type": "string"},
				"env":              map[string]any{"type": "object"},
				"timeout_seconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": 300},
				"stdin":            map[string]any{"type": "string"},
				"max_output_bytes": map[string]any{"type": "integer"},
			},
			"required": []string{"server_id", "cmd"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"exit_code":        map[string]any{"type": "integer"},
				"stdout":           map[string]any{"type": "string"},
				"stderr":           map[string]any{"type": "string"},
				"duration_ms":      map[string]any{"type": "integer"},
				"stdout_truncated": map[string]any{"type": "boolean"},
				"stderr_truncated": map[string]any{"type": "boolean"},
				"timed_out":        map[string]any{"type": "boolean"},
			},
			"required": []string{"exit_code", "stdout", "stderr", "duration_ms"},
		},
		RequiredScope: model.ScopeServerExec,
		Handler:       handleServerExec,
	})
}

func handleServerExec(c *gin.Context, raw json.RawMessage) (any, error) {
	var args execArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.TimeoutSeconds > mcpExecMaxTimeoutSec {
		return nil, errMCPInvalidArgs("timeout_seconds out of range; must be 1..300")
	}
	srv, err := requireServerAccess(c, args.ServerID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if args.Cmd == "" {
		return nil, errMCPInvalidArgs("cmd required")
	}
	if execInputSize(&args) > mcpMaxInlineArgBytes {
		return nil, errMCPInvalidArgs("exec input too large: cmd, args, cwd, env and stdin are limited to 1MiB in total")
	}

	req := model.ExecRequest{
		Cmd:            args.Cmd,
		Args:           args.Args,
		Cwd:            args.Cwd,
		Env:            args.Env,
		TimeoutSeconds: args.TimeoutSeconds,
		Stdin:          args.Stdin,
		MaxOutputBytes: args.MaxOutputBytes,
	}

	timeout := callAgentTimeout(args.TimeoutSeconds, 30)
	raw2, err := rpc.CallAgent(c.Request.Context(), args.ServerID, model.TaskTypeExec, req, timeout)
	if err != nil {
		return nil, err
	}
	var res model.ExecResult
	if err := json.Unmarshal(raw2, &res); err != nil {
		return nil, err
	}
	// ExecResult.Error means the agent refused / failed to run the command
	// (disabled, empty cmd, Start/process-group failure). Surface it like fs.*
	// handlers do, so MCP isError=true and audit outcome=agent_error. Non-zero
	// ExitCode alone is a normal command outcome, not a tool error.
	if res.Error != "" {
		return nil, &execToolError{mcpError: mcpError{Code: model.MCPOutcomeAgentError, Msg: res.Error}, result: res}
	}
	return res, nil
}

// execInputSize is the size of everything server.exec forwards to the agent.
// Each args / env element counts at least one byte so that a long list of
// empty strings is bounded as well.
func execInputSize(args *execArgs) int {
	size := len(args.Cmd) + len(args.Cwd) + len(args.Stdin)
	for _, arg := range args.Args {
		size += len(arg) + 1
	}
	for key, value := range args.Env {
		size += len(key) + len(value) + 1
	}
	return size
}

type execToolError struct {
	mcpError
	result model.ExecResult
}

func (err *execToolError) StructuredResult() any { return err.result }

func (err *execToolError) Error() string {
	return fmt.Sprintf("%s: %s", err.Code, err.Msg)
}

// callAgentTimeout 给 dashboard 侧 CallAgent 计算等待上限。
// 在用户请求的 timeout 基础上加 5s buffer，让 agent 端的 hard timeout 先触发，
// 这样 dashboard 收到的总是结构化结果（包含 timed_out=true），
// 而不是 ErrAgentTimeout。
func callAgentTimeout(reqTimeoutSec uint32, defaultSec uint32) time.Duration {
	t := reqTimeoutSec
	if t == 0 {
		t = defaultSec
	}
	return time.Duration(t+5) * time.Second
}
