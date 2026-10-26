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

// mcpAuditWrite 异步写一条 MCP 审计日志。失败仅 log，不阻塞业务。
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
	select {
	case mcpAuditQueue <- mcpAuditJob{db: db, entry: entry}:
	default:
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
