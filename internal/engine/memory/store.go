package memory

import (
	"context"
	"sync"

	"daidai/internal/core"
	"daidai/internal/db"
)

// MemoryEngine 长期事实记忆与会话历史引擎 (直接实现 entity.Memory，纯粹 SQLite 驱动)
type MemoryEngine struct {
	db          *db.Manager
	llm         core.LLMProvider
	embedder    core.EmbeddingProvider
	mu          sync.RWMutex
	userLocksMu sync.Mutex
	userLocks   map[userLockKey]*userLockEntry
	asyncMu     sync.Mutex
	asyncWG     sync.WaitGroup
	workerWG    sync.WaitGroup
	asyncJobs   chan postCommitJob
	workersOn   bool
	asyncCtx    context.Context
	asyncCancel context.CancelFunc
	asyncClosed bool
}

const (
	postCommitWorkerCount = 4
	postCommitQueueSize   = 128
)

type userLockKey struct {
	platform string
	userID   string
}

type userLockEntry struct {
	gate chan struct{}
	refs int
}

// NewMemoryEngine 创建记忆引擎，可选注入后台事实提炼模型。
func NewMemoryEngine(provider core.LLMProvider) *MemoryEngine {
	var emb core.EmbeddingProvider
	if ep, ok := provider.(core.EmbeddingProvider); ok {
		emb = ep
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	return &MemoryEngine{
		llm:         provider,
		embedder:    emb,
		asyncCtx:    asyncCtx,
		asyncCancel: asyncCancel,
		asyncJobs:   make(chan postCommitJob, postCommitQueueSize),
	}
}

// SetEmbedder 显式绑定稠密向量编码器 (如硅基流动 BAAI/bge-m3)，并同步设为全局默认编码器
func (m *MemoryEngine) SetEmbedder(emb core.EmbeddingProvider) {
	m.mu.Lock()
	m.embedder = emb
	m.mu.Unlock()
	SetDefaultEmbedder(emb)
}

func (m *MemoryEngine) getEmbedder() core.EmbeddingProvider {
	m.mu.RLock()
	emb := m.embedder
	m.mu.RUnlock()
	if emb != nil {
		return emb
	}
	return GetDefaultEmbedder()
}

func (m *MemoryEngine) lockUser(ctx context.Context, platform, userID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := userLockKey{platform: db.NormPlatform(platform), userID: userID}
	m.userLocksMu.Lock()
	if m.userLocks == nil {
		m.userLocks = make(map[userLockKey]*userLockEntry)
	}
	entry := m.userLocks[key]
	if entry == nil {
		entry = &userLockEntry{gate: make(chan struct{}, 1)}
		m.userLocks[key] = entry
	}
	entry.refs++
	m.userLocksMu.Unlock()

	releaseRef := func() {
		m.userLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 && m.userLocks[key] == entry {
			delete(m.userLocks, key)
		}
		m.userLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case entry.gate <- struct{}{}:
		return func() {
			<-entry.gate
			releaseRef()
		}, nil
	}
}

// SetDBManager 设置自定义数据库管理器 (单测或特殊隔离环境使用)
func (m *MemoryEngine) SetDBManager(mgr *db.Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.db = mgr
}

func (m *MemoryEngine) getDB() *db.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.db
}
