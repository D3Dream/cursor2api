// 会话管理：把 Anthropic 请求映射到 Cursor 服务端会话。
//
// 原理：Claude Code 每轮发送完整历史。我们保存"上次返回的 assistant 响应"的指纹，
// 下一轮请求里找到相同指纹的 assistant 消息，即认为是同一会话的延续：
//   - 延续：相同 conversation_id + checkpoint blob 重放，只处理增量消息
//   - 工具续接：服务端重放挂起的工具调用，我们提交 tool_result
//   - 未命中：新会话
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"cursor2api/internal/cursor"
)

// newUUID 生成 RFC 4122 v4 随机 ID（与 cursor 包内 uuid() 一致：
// 设置 version/variant 位，服务端若校验版本位也不会拒）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// shortConvID 日志用短 ID，对空串/短串安全。
func shortConvID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// ensureConversationID 在调用 cursor.Run 前补齐会话 ID。
// Run 按值接收 opts，其内部生成的 ID 回传不到调用方，
// 会导致 finishTurn 存下空 ID 会话、续接时 panic（切片越界）。
func ensureConversationID(opts *cursor.RunOptions) {
	if opts.ConversationID == "" {
		opts.ConversationID = newUUID()
	}
}

// PendingTool 挂起的工具调用（等客户端 tool_result）。
type PendingTool struct {
	ToolUseID string // 给客户端的 anthropic toolu_ id
	Name      string
	Input     string // 规范化 JSON
	// Cursor exec identity is kept only in memory so a later tool_result can
	// be written back to the still-live upstream Run.
	ExecName  string
	ExecID    uint32
	ExecIDStr string
}

// Conversation 一个进行中的 Cursor 会话。
type Conversation struct {
	ID         string
	Checkpoint *cursor.Checkpoint
	// LastRespHash 复合键 "命名空间:响应指纹"——
	// 命名空间 = 首条 user 消息的哈希，防止不同会话产出相同 assistant
	// 文本（"好的"/"Done."）时指纹碰撞、错拿别的会话的 checkpoint。
	LastRespHash string
	PendingTools []PendingTool
	LastUsed     time.Time
}

// ConversationStore 会话存储（按响应指纹索引，TTL 过期）。
type ConversationStore struct {
	mu     sync.Mutex
	byHash map[string]*Conversation
	locks  map[string]chan struct{} // conversation id → 在飞标记（并发复用互斥）
	ttl    time.Duration
	stamp  time.Time // monotonic wall-clock fallback for coarse Windows timers
}

func NewConversationStore(ttl time.Duration) *ConversationStore {
	return &ConversationStore{byHash: make(map[string]*Conversation), locks: make(map[string]chan struct{}), ttl: ttl}
}

// FindByRespHash 按 assistant 响应指纹找会话。
func (s *ConversationStore) FindByRespHash(hash string) *Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	c := s.byHash[hash]
	if c != nil {
		c.LastUsed = s.nextStamp()
	}
	return c
}

// FindByPendingToolID resolves the exact conversation state that emitted a
// tool_use. Tool IDs survive client-side streaming/text normalization, making
// them a stronger continuation anchor than the full assistant fingerprint.
func (s *ConversationStore) FindByPendingToolID(namespace, toolID string) *Conversation {
	if toolID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	prefix := namespace + ":"
	var found *Conversation
	for _, c := range s.byHash {
		if !strings.HasPrefix(c.LastRespHash, prefix) {
			continue
		}
		for _, pending := range c.PendingTools {
			if pending.ToolUseID == toolID && (found == nil || c.LastUsed.After(found.LastUsed)) {
				found = c
			}
		}
	}
	if found != nil {
		found.LastUsed = s.nextStamp()
	}
	return found
}

// maxConversations 会话存储上限：每个会话携带 Checkpoint（含 blobs，
// 单 run 上限 256MB），只按 TTL 清理会被大量会话撑爆内存。
// 超限按 LastUsed 淘汰最久未用（LRU）。
const maxConversations = 256

// Save 保存会话。
func (s *ConversationStore) Save(c *Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge()
	if _, ok := s.byHash[c.LastRespHash]; !ok && len(s.byHash) >= maxConversations {
		var oldestKey string
		var oldest time.Time
		for k, v := range s.byHash {
			if oldestKey == "" || v.LastUsed.Before(oldest) {
				oldestKey, oldest = k, v.LastUsed
			}
		}
		delete(s.byHash, oldestKey)
	}
	c.LastUsed = s.nextStamp()
	s.byHash[c.LastRespHash] = c
}

func (s *ConversationStore) nextStamp() time.Time {
	now := time.Now()
	if !now.After(s.stamp) {
		now = s.stamp.Add(time.Nanosecond)
	}
	s.stamp = now
	return now
}

// LockConv 获取会话级在飞互斥锁：同一 conversation 的并发复用（客户端超时重试
// 与慢首个请求竞争）会并发推进服务端会话状态、互相污染 checkpoint。
// 已在飞时排队等待；ctx 取消则返回 nil（调用方按 ctx 错误路径处理）。
func (s *ConversationStore) LockConv(id string, ctx context.Context) (release func()) {
	if id == "" {
		return func() {}
	}
	s.mu.Lock()
	for {
		ch, ok := s.locks[id]
		if !ok {
			ch = make(chan struct{})
			s.locks[id] = ch
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				delete(s.locks, id)
				close(ch)
				s.mu.Unlock()
			}
		}
		s.mu.Unlock()
		select {
		case <-ch: // 前一个 run 完成，重新竞争
		case <-ctx.Done():
			return nil
		}
		s.mu.Lock()
	}
}

func (s *ConversationStore) purge() {
	if s.ttl <= 0 {
		return
	}
	now := time.Now()
	for k, c := range s.byHash {
		if now.Sub(c.LastUsed) > s.ttl {
			delete(s.byHash, k)
		}
	}
}

// ---- 指纹 ----

// normalizeJSON 解析后重新编码（key 排序），消除 JSON 格式差异。
// 空输入归一为 "{}"：json.RawMessage("") 会让整体 Marshal 失败（200 空响应体）。
func normalizeJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(b)
}

// hashBlocks 计算 assistant 消息内容指纹。
func hashBlocks(blocks []contentBlock) string {
	h := sha256.New()
	for _, b := range blocks {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", b.Type, b.Text, b.Name, normalizeJSON(string(b.Input)))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// tailUserBlocks 提取 lastAssistant 之后的所有 user 消息 block。
// 注意只取 user：续接命中更早 assistant 时，其间被改写的 assistant 消息
// 会被忽略（语义=从命中点分叉，服务端状态里没有那些响应）。
func tailUserBlocks(messages []anthropicMessage, afterIdx int) []contentBlock {
	var out []contentBlock
	for i := afterIdx + 1; i < len(messages); i++ {
		if messages[i].Role == "user" {
			out = append(out, parseContent(messages[i].Content)...)
		}
	}
	return out
}

// firstUserText 提取首条 user 消息的文本（会话命名空间）。
// 客户端每轮都带完整历史，所以续接请求里能稳定重算出同一个命名空间。
// 无文本（纯图片/纯 tool_result 开场）时退回 block 结构指纹：
// 否则所有这类会话共享 hashText("") 命名空间，碰撞保护形同虚设。
func firstUserText(messages []anthropicMessage) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		blocks := parseContent(m.Content)
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		if len(blocks) > 0 {
			// 结构指纹：类型 + 关联 id + 内容，稳定且跨会话可区分
			// （不用 hashBlocks：它不覆盖 tool_result 的 ToolUseID/Content）
			h := sha256.New()
			for _, b := range blocks {
				fmt.Fprintf(h, "%s\x00%s\x00%s\x00%v\x00", b.Type, b.ToolUseID, b.Name, b.Content)
			}
			return "\x00blocks:" + hex.EncodeToString(h.Sum(nil))[:32]
		}
	}
	return ""
}

// embedHistoryAsText 把历史渲染为 <conversation_history> 文本块。
// conversation_history 协议字段会被服务端忽略（实测），文本嵌入保证上下文可达。
func embedHistoryAsText(history []cursor.HistoryMessage) string {
	if len(history) == 0 {
		return ""
	}
	var hb strings.Builder
	hb.WriteString("<conversation_history>\n")
	for _, h := range history {
		switch {
		case h.Role == "user":
			fmt.Fprintf(&hb, "user: %s\n", h.Text)
		case h.Role == "assistant" && h.ToolName != "":
			fmt.Fprintf(&hb, "assistant called tool %s(%s)\n", h.ToolName, h.ArgsJSON)
		case h.Role == "assistant":
			fmt.Fprintf(&hb, "assistant: %s\n", h.Text)
		case h.Role == "tool":
			fmt.Fprintf(&hb, "tool result [%s]: %s\n", h.ToolCallID, h.Text)
		}
	}
	hb.WriteString("</conversation_history>\n\n")
	return hb.String()
}
