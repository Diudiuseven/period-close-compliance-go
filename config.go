package compliance

import "time"

// Config 是本地持久化与结账语义相关的配置项。
//
// 所有状态只保存在进程内与 DataDir 下的本地文件：
//   - wal.log      预写日志（明细、调整、规则、结账意图与提交等状态变更）
//   - snapshot.json 最近一次已提交版本快照（可选，用于加速重启）
//   - audit.log    只追加的审计日志
type Config struct {
	// DataDir 是 WAL、快照、审计日志的存放目录；为空表示纯内存模式。
	DataDir string

	// Fsync 控制每次提交是否 fsync。关闭可提速但崩溃可能丢最近提交。
	Fsync bool

	// CloseTimeout 是单个期间结账锁的等待超时，0 表示不等待。
	CloseTimeout time.Duration

	// DefaultRules 在启动且规则簿为空时发布；为空则发布一条 0% 税率的兜底规则。
	DefaultRules []Rule

	// RecoverOrphanClose 为 true 时，重启发现 "closing" 孤儿状态会自动回滚到
	// 未结账/已重开状态并写审计；为 false 时保留 closing 状态等待显式干预。
	RecoverOrphanClose bool
}

// DefaultConfig 返回可直接使用的默认配置：临时目录、每次提交 fsync、
// 结账锁超时 5 秒、自动恢复孤儿结账。
func DefaultConfig() Config {
	return Config{
		DataDir:            "", // New 会在为空时分配临时目录；纯内存可用纯内存配置
		Fsync:              true,
		CloseTimeout:       5 * time.Second,
		RecoverOrphanClose: true,
	}
}

// InMemoryConfig 返回不写本地磁盘的纯内存配置（用于测试）。
func InMemoryConfig() Config {
	return Config{
		DataDir:            "",
		Fsync:              false,
		CloseTimeout:       5 * time.Second,
		RecoverOrphanClose: true,
	}
}
