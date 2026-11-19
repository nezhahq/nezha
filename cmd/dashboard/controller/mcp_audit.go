package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"gorm.io/gorm"
	"log"
	"sync"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// mcpAuditWrite 异步写一条 MCP 审计日志。写入失败仅 log。
// 队列有界：匿名失败（TokenID == 0）在队列满时丢弃并打日志；带凭据的调用
// 不允许丢审计，队列满时由请求自身等待写入方腾出位置。
//
// argsBytes：tool 的 raw JSON 参数（dispatcher 已经反序列化过）。
// 只记录 sha256 全文哈希，不保留任何明文片段：server.exec 的 env/stdin、
// fs.write 的 content 等字段会包含 token、密码、密钥、文件内容等敏感数据，
// 任何长度的 peek 都可能让审计表本身成为 secret 仓库；以哈希做关联即可。
//
// 测试可以把 mcpAuditSync 置为 true 让写入同步，避免 goroutine 与测试 teardown
// 形成竞态（不同测试 swap 全局 singleton.DB 时尤其明显）。
func mcpAuditWrite(entry model.MCPAuditLog, argsBytes []byte) {
	if len(argsBytes) > 0 {
		sum := sha256.Sum256(argsBytes)
		entry.ArgsHash = hex.EncodeToString(sum[:])
	}
	entry.ArgsPeek = ""
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	db := singleton.DB
	write := func(e model.MCPAuditLog) {
		if db == nil {
			return
		}
		if err := db.Create(&e).Error; err != nil {
			log.Printf("NEZHA>> mcp audit write failed: %v", err)
		}
	}
	if mcpAuditSync {
		write(entry)
		return
	}
	// One bounded writer replaces one goroutine per anonymous failure.
	mcpAuditWorkerOnce.Do(func() {
		go func() {
			for job := range mcpAuditQueue {
				job.write()
			}
		}()
	})
	job := mcpAuditJob{db: db, entry: entry}
	if entry.TokenID != 0 {
		// A credentialed call must leave its row: wait for a slot instead of
		// dropping. The waiter is the request's own handler, so a full queue
		// slows callers to the writer's pace and still adds no goroutine.
		mcpAuditQueue <- job
		return
	}
	// Anonymous failures stay best-effort so unauthenticated peers cannot park handlers.
	select {
	case mcpAuditQueue <- job:
	default:
		log.Printf("NEZHA>> mcp audit queue full: dropped anonymous %s/%s row", entry.Tool, entry.Outcome)
	}
}

// mcpAuditSync 仅供测试切换为同步写入，生产保持 false。
var mcpAuditSync = false

type mcpAuditJob struct {
	db    *gorm.DB
	entry model.MCPAuditLog
}

var mcpAuditQueue = make(chan mcpAuditJob, 128)
var mcpAuditWorkerOnce sync.Once

func (job mcpAuditJob) write() {
	if job.db == nil {
		return
	}
	if err := job.db.Create(&job.entry).Error; err != nil {
		log.Printf("NEZHA>> mcp audit write failed: %v", err)
	}
}
