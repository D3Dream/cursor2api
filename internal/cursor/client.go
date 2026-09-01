// 全双工 Run 客户端：事件循环、exec/KV 自动回包、心跳。
package cursor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"cursor2api/internal/schema"

	"golang.org/x/net/http2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// EventKind 事件类型。
type EventKind int

// debug 诊断日志开关（CURSOR2API_DEBUG=1）。
var debug = os.Getenv("CURSOR2API_DEBUG") != ""

func dlog(format string, args ...any) {
	if debug {
		log.Printf("[cursor] "+format, args...)
	}
}

const (
	EventText       EventKind = iota // 文本增量
	EventThinking                    // 思考增量
	EventToolCall                    // 模型请求调用 MCP 工具（需 RespondTool 回应）
	EventTurnEnd                     // 回合结束（含 token 统计）
	EventCheckpoint                  // 对话状态检查点（续聊凭据）
	EventDone                        // 流正常结束
	EventError                       // 错误
)

// maxFrameSize 单个 connect 帧的防御上限（正常帧远小于此，超限视为流损坏）。
const maxFrameSize = 64 << 20

// maxDecompressedFrameSize gzip 解压后上限（zip bomb 防御，正常解压帧远小于此）。
const maxDecompressedFrameSize = 256 << 20

// maxShellTimeoutMs shell 执行超时钳制上限。
// 必须低于调用方 runTurn 的 120s 停滞兜底：超时帧要先于 stall 判定到达，
// 否则长命令会以"上游静默杀 run"的形式报错，真实原因（命令超时）被掩盖。
const maxShellTimeoutMs = 110_000

// Event 服务端事件。
type Event struct {
	Kind EventKind
	Text string // Text/Thinking 增量

	// ToolCall 事件
	ToolCallID   string
	ToolName     string
	ToolArgsJSON string
	// ExecName is the original Cursor exec field (for example read_args or
	// shell_stream_args). It is needed when a downstream agent returns the
	// result so the proxy can build the matching protobuf response.
	ExecName  string
	ExecID    uint32 // RespondTool 配对 id
	ExecIDStr string

	Usage      *Usage
	Checkpoint *Checkpoint
	Err        error
}

// Usage token 统计。
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	ReasoningTokens int64
}

// Checkpoint 对话状态检查点（多轮续聊凭据）。
type Checkpoint struct {
	RootPromptBlobs  [][]byte
	TurnBlobs        [][]byte
	PendingToolCalls []string
	// blob id -> blob data（续聊时经 pre_fetched_blobs 带回）
	Blobs map[string][]byte
}

// HistoryMessage 对话历史消息。
type HistoryMessage struct {
	Role       string // user | assistant | tool
	Text       string
	ToolCallID string
	ToolName   string
	ArgsJSON   string // assistant 工具调用的参数 JSON
	IsError    bool   // tool 结果是否错误
}

// ToolDef 自定义工具定义（注入为 MCP 工具）。
type ToolDef struct {
	Name        string
	Description string
	InputSchema string // JSON Schema 文本
}

// RunOptions 一次运行的参数。
type RunOptions struct {
	Model          string
	SystemPrompt   string
	History        []HistoryMessage
	Prompt         string
	Tools          []ToolDef
	ConversationID string // 空则生成
	Mode           int32  // AgentMode：1=AGENT 2=ASK 3=PLAN，默认 2
	// 续聊状态（来自上一轮的 EventCheckpoint）。
	// 工具续接没有独立的 resume 动作：checkpoint 重放后服务端会重新发起
	// 挂起的工具调用，客户端用 RespondTool 提交结果即可推进会话。
	State *Checkpoint
}

// Client Cursor AgentService 客户端。
type Client struct {
	reg      *schema.Registry
	tokens   *TokenProvider
	endpoint string
	version  string
	http     *http.Client
}

// NewClient 创建客户端。
func NewClient(reg *schema.Registry, tokens *TokenProvider, endpoint, clientVersion string) *Client {
	return &Client{
		reg:      reg,
		tokens:   tokens,
		endpoint: endpoint,
		version:  clientVersion,
		http:     &http.Client{Transport: &http2.Transport{}},
	}
}

// Run 一次 agent 运行。
type Run struct {
	events  chan Event
	closeCh chan struct{}

	systemPrompt string
	lastExecID   uint32
	execIDMu     sync.RWMutex

	blobs     map[string][]byte
	blobBytes int // blobs 总量（字节），上限 maxBlobCacheBytes
	blobsMu   sync.Mutex

	writeMu   sync.Mutex
	pw        *io.PipeWriter
	reg       *schema.Registry
	heartbeat *dynamicpb.Message

	execSem   chan struct{} // 异步 exec 并发上限（防上游异常推送打爆 goroutine/子进程）
	closeOnce sync.Once
	respBody  io.Closer
}

// maxConcurrentExec 同一 run 内异步执行的工具调用上限。
const maxConcurrentExec = 16

// maxBlobCacheBytes 单 run blob 缓存总量上限（长会话状态防无界增长）。
const maxBlobCacheBytes = 256 << 20

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// errUnauthorized 上游 401（token 过期/失效），用于触发一次透明重试。
var errUnauthorized = errors.New("cursor: 401 unauthorized")

// Run 启动一次运行。
func (c *Client) Run(ctx context.Context, opts RunOptions) (*Run, error) {
	if opts.Mode == 0 {
		opts.Mode = 2 // ASK：不使用内置工具，只认 MCP 工具和文本
	}
	if opts.ConversationID == "" {
		opts.ConversationID = uuid()
	}

	acm, err := c.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return nil, err
	}
	rr := sub(acm, "run_request")
	cs := sub(rr, "conversation_state")
	if opts.State != nil {
		for _, b := range opts.State.RootPromptBlobs {
			fdB := fdOf(cs.Descriptor(), "root_prompt_messages_json")
			cs.Mutable(fdB).List().Append(protoreflect.ValueOfBytes(b))
		}
		for _, b := range opts.State.TurnBlobs {
			fdB := fdOf(cs.Descriptor(), "turns")
			cs.Mutable(fdB).List().Append(protoreflect.ValueOfBytes(b))
		}
	}
	action := sub(rr, "action")
	uma := sub(action, "user_message_action")
	um := sub(uma, "user_message")
	setStr(um, "text", opts.Prompt)
	setStr(um, "message_id", uuid())
	setInt(um, "mode", opts.Mode)

	// 历史
	if len(opts.History) > 0 {
		hist := sub(uma, "conversation_history")
		for _, h := range opts.History {
			m := appendSub(hist, "messages")
			switch h.Role {
			case "user":
				u := sub(m, "user")
				c := appendSub(u, "content")
				t := sub(c, "text")
				setStr(t, "text", h.Text)
			case "assistant":
				a := sub(m, "assistant")
				c := appendSub(a, "content")
				if h.ToolName != "" {
					tc := sub(c, "tool_call")
					setStr(tc, "tool_call_id", h.ToolCallID)
					setStr(tc, "tool_name", h.ToolName)
					setStr(tc, "args_json", h.ArgsJSON)
				} else {
					t := sub(c, "text")
					setStr(t, "text", h.Text)
				}
			case "tool":
				tm := sub(m, "tool")
				setStr(tm, "tool_call_id", h.ToolCallID)
				setStr(tm, "tool_name", h.ToolName)
				if h.IsError {
					setBool(tm, "is_error", true)
				}
				c := appendSub(tm, "content")
				t := sub(c, "text")
				setStr(t, "text", h.Text)
			}
		}
	}

	if opts.Model != "" {
		rm := sub(rr, "requested_model")
		setStr(rm, "model_id", opts.Model)
	}
	// 注意：custom_system_prompt 字段会导致服务端直接终止 run（已实测），
	// 系统提示改经 request_context 的 rules 注入（见 replyRequestContext）。
	// 工具注入
	mt := sub(rr, "mcp_tools")
	for _, t := range opts.Tools {
		td := appendSub(mt, "mcp_tools")
		setStr(td, "name", t.Name)
		setStr(td, "tool_name", t.Name)
		setStr(td, "description", t.Description)
		setStr(td, "input_schema_json", t.InputSchema)
		setStr(td, "provider_identifier", "cursor2api")
	}
	setStr(rr, "conversation_id", opts.ConversationID)
	setStr(rr, "conversation_group_id", opts.ConversationID)
	if opts.State != nil {
		for id, val := range opts.State.Blobs {
			pb := appendSub(rr, "pre_fetched_blobs")
			fdID := fdOf(pb.Descriptor(), "id")
			pb.Set(fdID, protoreflect.ValueOfBytes([]byte(id)))
			fdVal := fdOf(pb.Descriptor(), "value")
			pb.Set(fdVal, protoreflect.ValueOfBytes(val))
		}
	}

	r := &Run{
		events:       make(chan Event, 64),
		closeCh:      make(chan struct{}),
		reg:          c.reg,
		blobs:        make(map[string][]byte),
		execSem:      make(chan struct{}, maxConcurrentExec),
		systemPrompt: opts.SystemPrompt,
	}
	// 续聊：把上一轮带回的 blob 灌入缓存，供 get_blob 应答
	if opts.State != nil {
		for k, v := range opts.State.Blobs {
			r.blobs[k] = v
			r.blobBytes += len(v)
		}
	}
	r.heartbeat, _ = c.reg.New("agent.v1.AgentClientMessage")
	sub(r.heartbeat, "client_heartbeat")

	dlog("run start conv=%s model=%s mode=%d prompt=%dB history=%d tools=%d",
		opts.ConversationID, opts.Model, opts.Mode, len(opts.Prompt), len(opts.History), len(opts.Tools))

	resp, err := c.runAttempt(ctx, r, acm)
	if errors.Is(err, errUnauthorized) && !c.tokens.FromEnv() {
		// token 过期：runAttempt 内已 Invalidate，重取 token 透明重试一次。
		// env token 不走缓存，Invalidate 无效——重试必然再 401，直接报真实原因
		dlog("run: 401, retrying with fresh token")
		resp, err = c.runAttempt(ctx, r, acm)
	}
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			if c.tokens.FromEnv() {
				return nil, fmt.Errorf("run: 401 unauthorized (CURSOR_API_KEY/CURSOR_ACCESS_TOKEN is invalid or expired; env tokens cannot be refreshed, update the env var)")
			}
			return nil, fmt.Errorf("run: 401 unauthorized (token refresh did not help)")
		}
		return nil, err
	}
	r.respBody = resp.Body
	go r.readLoop(resp.Body)
	go r.heartbeatLoop()
	return r, nil
}

// runAttempt 发起一次 Run 请求：取 token、新建 pipe/请求、首发 run_request、等响应头。
// pipe 请求体是一次性的，所以每次尝试都新建；失败后调用方可安全重试。
func (c *Client) runAttempt(ctx context.Context, r *Run, acm proto.Message) (*http.Response, error) {
	token, err := c.tokens.Token()
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/agent.v1.AgentService/Run", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", "application/connect+proto")
	req.Header.Set("connect-protocol-version", "1")
	// 只声明 gzip：readLoop 只实现了 gzip 解码。
	// 声明 br 会让上游有权选 brotli，届时每帧都解压失败被静默丢弃（停滞超时收场）。
	req.Header.Set("connect-accept-encoding", "gzip")
	req.Header.Set("user-agent", "connect-es/1.6.1")
	req.Header.Set("x-cursor-client-type", "cli")
	req.Header.Set("x-cursor-client-version", c.version)
	req.Header.Set("x-ghost-mode", "false")
	req.Header.Set("x-blob-encryption-key", randHex(32))
	req.Header.Set("x-request-id", uuid())

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := c.http.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	// 首发 run_request（pipe 需要 reader，必须在 Do 启动后）
	r.pw = pw
	if err := r.send(acm); err != nil {
		_ = pw.Close()
		// Do 的 goroutine 可能随后才把 resp 送进缓冲 channel，
		// 没人接收 body 就永远不会关闭——起个清扫 goroutine 兜底（同 ctx.Done 分支）
		go func() {
			select {
			case resp := <-respCh:
				resp.Body.Close()
			case <-errCh:
			}
		}()
		return nil, err
	}

	select {
	case err := <-errCh:
		_ = pw.Close()
		return nil, fmt.Errorf("connect: %w", err)
	case resp := <-respCh:
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			_ = pw.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				// token 过期：作废缓存，重试时会强制重取
				c.tokens.Invalidate()
				return nil, errUnauthorized
			}
			return nil, fmt.Errorf("run: %s: %s", resp.Status, body)
		}
		return resp, nil
	case <-ctx.Done():
		_ = pw.Close()
		// Do 的 goroutine 可能随后才把 resp 送进缓冲 channel，
		// 没人接收 body 就永远不会关闭——起个清扫 goroutine 兜底
		go func() {
			select {
			case resp := <-respCh:
				resp.Body.Close()
			case <-errCh:
			}
		}()
		return nil, ctx.Err()
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Events 事件通道。
func (r *Run) Events() <-chan Event {
	return r.events
}

// emit 投递事件；Close 后不再阻塞（消费端退出、缓冲满时防止 goroutine 泄漏）。
func (r *Run) emit(ev Event) {
	select {
	case r.events <- ev:
	case <-r.closeCh:
	}
}

// RespondTool 回应工具调用（mcp_args → mcp_result）。
func (r *Run) RespondTool(execID uint32, execIDStr, resultText string, isError bool) error {
	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return err
	}
	ecm := sub(acm, "exec_client_message")
	setUint(ecm, "id", execID)
	if execIDStr != "" {
		setStr(ecm, "exec_id", execIDStr)
	}
	mr := sub(ecm, "mcp_result")
	if isError {
		me := sub(mr, "error")
		setStr(me, "error", resultText)
	} else {
		ms := sub(mr, "success")
		item := appendSub(ms, "content")
		txt := sub(item, "text")
		setStr(txt, "text", resultText)
	}
	return r.send(acm)
}

// RespondExec sends a result for a forwarded Cursor built-in exec. The actual
// command/file operation has already happened in the downstream agent; this
// method only adapts its textual result back to Cursor's native response oneof.
func (r *Run) RespondExec(execName, argsJSON string, execID uint32, execIDStr, resultText string, isError bool) error {
	if execName == "mcp_args" || execName == "" {
		return r.RespondTool(execID, execIDStr, resultText, isError)
	}
	base := strings.TrimSuffix(execName, "_args")
	if base == "shell_stream" {
		return r.respondShellStream(execID, resultText, isError)
	}

	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return err
	}
	ecm := sub(acm, "exec_client_message")
	setUint(ecm, "id", execID)
	if execIDStr != "" {
		setStr(ecm, "exec_id", execIDStr)
	}

	resultField := base + "_result"
	fdResult := ecm.Descriptor().Fields().ByName(protoreflect.Name(resultField))
	if fdResult == nil || fdResult.Kind() != protoreflect.MessageKind {
		return r.RespondTool(execID, execIDStr, resultText, isError)
	}
	result := ecm.Mutable(fdResult).Message().(*dynamicpb.Message)
	argPath := jsonArgString(argsJSON, "file_path")
	if argPath == "" {
		argPath = jsonArgString(argsJSON, "path")
	}
	if isError {
		if !setResultVariant(result, "error", resultText) {
			if !setResultVariant(result, "failure", resultText) {
				setResultVariant(result, "rejected", resultText)
			}
		}
	} else {
		switch base {
		case "read":
			ok := sub(result, "success")
			setStrIf(ok, "path", argPath)
			setStrIf(ok, "content", resultText)
		case "write":
			ok := sub(result, "success")
			setStrIf(ok, "path", argPath)
			setStrIf(ok, "file_content_after_write", resultText)
		case "edit":
			ok := sub(result, "success")
			setStrIf(ok, "path", argPath)
			setStrIf(ok, "message", resultText)
		case "shell":
			ok := sub(result, "success")
			setStrIf(ok, "stdout", resultText)
			setIntIf(ok, "exit_code", 0)
		case "delete":
			ok := sub(result, "success")
			setStrIf(ok, "path", argPath)
		case "grep":
			ok := sub(result, "success")
			setStrIf(ok, "pattern", jsonArgString(argsJSON, "pattern"))
			setStrIf(ok, "path", jsonArgString(argsJSON, "path"))
			setStrIf(ok, "output_mode", jsonArgString(argsJSON, "output_mode"))
		case "ls":
			ok := sub(result, "success")
			root := sub(ok, "directory_tree_root")
			setStrIf(root, "abs_path", argPath)
		default:
			// For structured read-only results, put the agent's textual output in
			// the protocol's content branch. Native clients can still continue;
			// richer fields are optional in the protobuf schema.
			ok := sub(result, "success")
			setStrIf(ok, "path", argPath)
		}
	}
	if err := r.send(acm); err != nil {
		return err
	}
	// Cursor's native exec protocol is two-part: the typed result carries the
	// payload, while stream_close marks that exec ID as complete. Without the
	// close frame the upstream accepts read_result but keeps waiting forever.
	return r.sendStreamClose(execID)
}

func jsonArgString(raw, key string) string {
	if raw == "" || raw == "{}" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return ""
	}
	if value, ok := obj[key].(string); ok {
		return value
	}
	return ""
}

func setStrIf(msg *dynamicpb.Message, name, value string) bool {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || fd.Kind() != protoreflect.StringKind {
		return false
	}
	msg.Set(fd, protoreflect.ValueOfString(value))
	return true
}

func setIntIf(msg *dynamicpb.Message, name string, value int32) bool {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		return false
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		msg.Set(fd, protoreflect.ValueOfInt32(value))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		msg.Set(fd, protoreflect.ValueOfInt64(int64(value)))
	default:
		return false
	}
	return true
}

func setUintIf(msg *dynamicpb.Message, name string, value uint32) bool {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		return false
	}
	switch fd.Kind() {
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		msg.Set(fd, protoreflect.ValueOfUint32(value))
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		msg.Set(fd, protoreflect.ValueOfUint64(uint64(value)))
	default:
		return false
	}
	return true
}

func setResultVariant(result *dynamicpb.Message, variant, message string) bool {
	fd := result.Descriptor().Fields().ByName(protoreflect.Name(variant))
	if fd == nil || fd.Kind() != protoreflect.MessageKind {
		return false
	}
	v := result.Mutable(fd).Message().(*dynamicpb.Message)
	if setStrIf(v, "error", message) || setStrIf(v, "abort_reason", message) || setStrIf(v, "stderr", message) {
		return true
	}
	return true
}

func (r *Run) respondShellStream(execID uint32, resultText string, isError bool) error {
	sendStream := func(fill func(*dynamicpb.Message)) error {
		acm, err := r.reg.New("agent.v1.AgentClientMessage")
		if err != nil {
			return err
		}
		ecm := sub(acm, "exec_client_message")
		setUint(ecm, "id", execID)
		ss := sub(ecm, "shell_stream")
		fill(ss)
		return r.send(acm)
	}
	if err := sendStream(func(ss *dynamicpb.Message) {
		st := sub(ss, "start")
		sp := sub(st, "sandbox_policy")
		setInt(sp, "type", 1)
	}); err != nil {
		return err
	}
	if resultText != "" {
		if err := sendStream(func(ss *dynamicpb.Message) {
			if isError {
				se := sub(ss, "stderr")
				setStr(se, "data", resultText)
			} else {
				so := sub(ss, "stdout")
				setStr(so, "data", resultText)
			}
		}); err != nil {
			return err
		}
	}
	if err := sendStream(func(ss *dynamicpb.Message) {
		ex := sub(ss, "exit")
		if isError {
			setUintIf(ex, "code", 1)
		} else {
			setUintIf(ex, "code", 0)
		}
	}); err != nil {
		return err
	}
	return r.sendStreamClose(execID)
}

// Close 终止运行。
func (r *Run) Close() {
	r.closeOnce.Do(func() {
		close(r.closeCh)
		_ = r.pw.Close()
		if r.respBody != nil {
			_ = r.respBody.Close()
		}
	})
}

func (r *Run) send(msg proto.Message) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	frame := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(b)))
	copy(frame[5:], b)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err = r.pw.Write(frame)
	return err
}

func (r *Run) heartbeatLoop() {
	// 同上：fdOf panic 不能带出 goroutine
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[cursor] heartbeatLoop panic: %v", rec)
		}
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.closeCh:
			return
		case <-t.C:
			_ = r.send(r.heartbeat)
			// exec 心跳：对齐 CLI，报告最新 exec id
			r.execIDMu.RLock()
			id := r.lastExecID
			r.execIDMu.RUnlock()
			if id > 0 {
				acm, err := r.reg.New("agent.v1.AgentClientMessage")
				if err == nil {
					ctrl := sub(acm, "exec_client_control_message")
					hb := sub(ctrl, "heartbeat")
					setUint(hb, "id", id)
					_ = r.send(acm)
				}
			}
		}
	}
}

func (r *Run) readLoop(body io.Reader) {
	// fdOf 在 schema 缺字段时 panic；readLoop 死了整个 run 静默挂死，
	// recover 后转成错误事件让调用方能感知并回收。
	// 必须是一个 defer：拆成两个时 LIFO 会先 close(events) 再 emit，
	// send on closed channel 二次 panic 直接炸掉进程（防护反而变成引信）。
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[cursor] readLoop panic: %v", rec)
			r.emit(Event{Kind: EventError, Err: fmt.Errorf("read loop panic: %v", rec)})
		}
		close(r.events)
	}()
	reader := bufio.NewReader(body)
	for {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(reader, hdr); err != nil {
			select {
			case <-r.closeCh:
				dlog("readLoop: closed locally")
			default:
				if err == io.EOF {
					// 帧边界上的干净关闭：实测上游正常结束就是裸 EOF（不发 EndStream 帧）
					dlog("readLoop: upstream EOF (clean, at frame boundary)")
					r.emit(Event{Kind: EventDone})
				} else {
					// ErrUnexpectedEOF（帧头/帧体读一半）或连接 reset：
					// 流被截断，半截回答不能当完整答案交付
					r.emit(Event{Kind: EventError, Err: fmt.Errorf("upstream stream terminated mid-frame: %w", err)})
				}
			}
			return
		}
		flags := hdr[0]
		l := binary.BigEndian.Uint32(hdr[1:5])
		if l > maxFrameSize {
			r.emit(Event{Kind: EventError, Err: fmt.Errorf("frame too large: %d bytes (corrupt stream?)", l)})
			return
		}
		data := make([]byte, l)
		if _, err := io.ReadFull(reader, data); err != nil {
			dlog("readLoop: frame read error: %v", err)
			r.emit(Event{Kind: EventError, Err: fmt.Errorf("read frame: %w", err)})
			return
		}
		if flags&0x80 != 0 {
			// EndStream frame（JSON，可能含错误）
			if len(data) > 2 && !bytes.Equal(data, []byte("{}")) {
				dlog("readLoop: EndStream with payload: %s", data)
				r.emit(Event{Kind: EventError, Err: fmt.Errorf("stream end: %s", data)})
			} else {
				dlog("readLoop: EndStream (clean)")
				r.emit(Event{Kind: EventDone})
			}
			return
		}
		if flags&0x01 != 0 {
			gz, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				dlog("readLoop: drop frame (%dB): gzip init: %v", l, err)
				continue
			}
			// 上限防 zip bomb：maxFrameSize 只约束压缩后大小，解压不设限会被膨胀帧打爆内存
			data, err = io.ReadAll(io.LimitReader(gz, maxDecompressedFrameSize))
			gz.Close()
			if err != nil {
				dlog("readLoop: drop frame (%dB): gunzip: %v", l, err)
				continue
			}
		}
		msg, err := r.reg.Unmarshal("agent.v1.AgentServerMessage", data)
		if err != nil {
			dlog("readLoop: drop frame (%dB): unmarshal: %v", len(data), err)
			continue
		}
		r.handle(msg)
	}
}

func (r *Run) handle(msg *dynamicpb.Message) {
	if iu, ok := get(msg, "interaction_update"); ok {
		r.handleInteraction(iu)
		return
	}
	if esm, ok := get(msg, "exec_server_message"); ok {
		dlog("recv exec_server_message")
		r.handleExec(esm)
		return
	}
	if kv, ok := get(msg, "kv_server_message"); ok {
		r.handleKV(kv)
		return
	}
	if cp, ok := get(msg, "conversation_checkpoint_update"); ok {
		dlog("recv checkpoint_update")
		r.handleCheckpoint(cp)
		return
	}
	// 其他：打印顶层字段名，便于发现未知消息（如安全围栏通知）。
	// 已知但未处理的类型：exec_server_control_message（服务端取消/中止 exec 的
	// 控制通道）与 interaction_query——当前无对应协议语义实现，只记录不动作。
	md := msg.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if msg.Has(fd) {
			log.Printf("[cursor] recv unhandled server message: %s", fd.Name())
		}
	}
}

func (r *Run) handleCheckpoint(cp *dynamicpb.Message) {
	ck := &Checkpoint{}
	blobList := func(name string) [][]byte {
		fd := cp.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return nil
		}
		list := cp.Get(fd).List()
		out := make([][]byte, 0, list.Len())
		for i := 0; i < list.Len(); i++ {
			out = append(out, list.Get(i).Bytes())
		}
		return out
	}
	ck.RootPromptBlobs = blobList("root_prompt_messages_json")
	ck.TurnBlobs = blobList("turns")
	fdP := cp.Descriptor().Fields().ByName("pending_tool_calls")
	if fdP != nil {
		list := cp.Get(fdP).List()
		for i := 0; i < list.Len(); i++ {
			ck.PendingToolCalls = append(ck.PendingToolCalls, list.Get(i).String())
		}
	}
	r.blobsMu.Lock()
	if len(r.blobs) > 0 {
		ck.Blobs = make(map[string][]byte, len(r.blobs))
		for k, v := range r.blobs {
			ck.Blobs[k] = v
		}
	}
	r.blobsMu.Unlock()
	r.emit(Event{Kind: EventCheckpoint, Checkpoint: ck})
}

func (r *Run) handleInteraction(iu *dynamicpb.Message) {
	md := iu.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if !iu.Has(fd) || fd.Kind() != 11 { // message
			continue
		}
		sub := iu.Get(fd).Message().(*dynamicpb.Message)
		switch string(fd.Name()) {
		case "text_delta":
			r.emit(Event{Kind: EventText, Text: getStr(sub, "text")})
		case "thinking_delta":
			r.emit(Event{Kind: EventThinking, Text: getStr(sub, "text")})
		case "turn_ended":
			dlog("recv turn_ended")
			r.emit(Event{Kind: EventTurnEnd, Usage: &Usage{
				InputTokens:     getInt64(sub, "input_tokens"),
				OutputTokens:    getInt64(sub, "output_tokens"),
				CacheReadTokens: getInt64(sub, "cache_read_tokens"),
				ReasoningTokens: getInt64(sub, "reasoning_tokens"),
			}})
		default:
			// 高频事件（token_delta 每 token 一条、heartbeat 每 10s）不打日志：
			// debug 模式下磁盘 I/O 与日志体积都会爆炸，有用信号反被淹没
			switch string(fd.Name()) {
			case "token_delta", "heartbeat", "thinking_delta", "partial_tool_call":
			default:
				dlog("recv interaction_update.%s", fd.Name())
			}
		}
	}
}

func (r *Run) handleExec(esm *dynamicpb.Message) {
	execID := getUint(esm, "id")
	execIDStr := getStr(esm, "exec_id")
	if execID > 0 {
		r.execIDMu.Lock()
		r.lastExecID = execID
		r.execIDMu.Unlock()
	}

	md := esm.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if !esm.Has(fd) || fd.Kind() != 11 {
			continue
		}
		name := string(fd.Name())
		args := esm.Get(fd).Message().(*dynamicpb.Message)
		dlog("exec dispatch id=%d name=%s", execID, name)
		switch name {
		case "request_context_args":
			r.replyRequestContext(execID, execIDStr)
		case "mcp_args":
			r.emit(Event{
				Kind:         EventToolCall,
				ToolCallID:   getStr(args, "tool_call_id"),
				ToolName:     getStr(args, "tool_name"),
				ToolArgsJSON: mcpArgsJSON(args),
				ExecName:     "mcp_args",
				ExecID:       execID,
				ExecIDStr:    execIDStr,
			})
		case "span_context":
			// 追踪上下文，无需回包
		case "shell_stream_args":
			// Forward to the downstream agent. The VPS must never execute shell
			// commands, including Cursor's streaming shell variant.
			r.emit(Event{
				Kind:         EventToolCall,
				ToolName:     downstreamToolName(name),
				ToolArgsJSON: downstreamToolArgs(name, args),
				ExecName:     name,
				ExecID:       execID,
				ExecIDStr:    execIDStr,
			})
		default:
			// The VPS is a protocol broker only. Never execute Cursor built-in
			// tools locally: forward every exec request to the downstream agent,
			// which owns the workspace and shell.
			r.emit(Event{
				Kind:         EventToolCall,
				ToolName:     downstreamToolName(name),
				ToolArgsJSON: downstreamToolArgs(name, args),
				ExecName:     name,
				ExecID:       execID,
				ExecIDStr:    execIDStr,
			})
		}
	}
}

func downstreamToolName(execName string) string {
	base := strings.TrimSuffix(execName, "_args")
	switch base {
	case "shell", "shell_stream":
		return "Bash"
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "grep":
		return "Grep"
	case "ls":
		return "Glob"
	case "delete":
		return "Delete"
	default:
		return base
	}
}

func downstreamToolArgs(execName string, args *dynamicpb.Message) string {
	// Use proto field names so the JSON can be consumed by standard agent
	// tools. A few Cursor names differ from Claude/Codex conventions.
	b, err := (&protojson.MarshalOptions{UseProtoNames: true}).Marshal(args)
	if err != nil {
		return "{}"
	}
	var obj map[string]any
	if jsonErr := json.Unmarshal(b, &obj); jsonErr != nil {
		return string(b)
	}
	if v, ok := obj["path"]; ok {
		switch strings.TrimSuffix(execName, "_args") {
		case "read", "write":
			obj["file_path"] = v
			delete(obj, "path")
		case "edit":
			obj["file_path"] = v
			delete(obj, "path")
		}
	}
	if v, ok := obj["file_text"]; ok {
		obj["content"] = v
		delete(obj, "file_text")
	}
	// Cursor uses this only to correlate its native exec internally. The proxy
	// keeps ExecID/ExecIDStr separately; exposing it makes strict downstream
	// Read/Write tool schemas reject otherwise valid calls.
	delete(obj, "tool_call_id")
	out, err := json.Marshal(obj)
	if err != nil {
		return string(b)
	}
	return string(out)
}

// dispatchExec is retained for isolated worker experiments. Production Run
// handling forwards execs directly and never invokes this local dispatcher.
func (r *Run) dispatchExec(name string, fn func()) {
	select {
	case r.execSem <- struct{}{}:
	case <-r.closeCh:
		return
	}
	go func() {
		defer func() { <-r.execSem }()
		r.guarded(name, fn)
	}()
}

// guarded 在异步 exec goroutine 外套 recover：fdOf 在 schema 缺字段时 panic，
// 没有 recover 会直接炸掉整个进程；单个 exec 失败不应拖垮服务。
func (r *Run) guarded(name string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[cursor] exec %s panic: %v", name, rec)
		}
	}()
	fn()
}

func mcpArgsJSON(args *dynamicpb.Message) string {
	// McpArgs.args 是 map<string, google.protobuf.Value>
	fd := args.Descriptor().Fields().ByName("args")
	if fd == nil {
		return "{}"
	}
	m := args.Get(fd).Map()
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&buf, "%q:", k.String())
		if msg := v.Message(); msg != nil && msg.IsValid() {
			if b, err := protojson.Marshal(msg.Interface()); err == nil {
				buf.Write(b)
				return true
			}
		}
		fmt.Fprintf(&buf, "%q", v.String())
		return true
	})
	buf.WriteByte('}')
	return buf.String()
}

func (r *Run) replyRequestContext(execID uint32, execIDStr string) {
	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return
	}
	ecm := sub(acm, "exec_client_message")
	setUint(ecm, "id", execID)
	if execIDStr != "" {
		setStr(ecm, "exec_id", execIDStr)
	}
	rcr := sub(ecm, "request_context_result")
	succ := sub(rcr, "success")
	rc := sub(succ, "request_context")
	if r.systemPrompt != "" {
		rule := appendSub(rc, "rules")
		setStr(rule, "content", r.systemPrompt)
		setStr(rule, "full_path", "cursor2api/system.mdc")
		ty := sub(rule, "type")
		sub(ty, "global") // CursorRuleTypeGlobal：始终生效
	}
	for _, f := range []string{
		"rules_info_complete", "env_info_complete", "repository_info_complete",
		"custom_subagents_info_complete", "agent_skills_info_complete",
		"mcp_file_system_info_complete", "git_status_info_complete",
		"mcp_info_complete", "git_repo_info_complete",
	} {
		fd := rc.Descriptor().Fields().ByName(protoreflect.Name(f))
		if fd != nil {
			rc.Set(fd, protoreflect.ValueOfBool(true))
		}
	}
	_ = r.send(acm)
	_ = r.sendStreamClose(0)
}

// replyExecRejected 拒绝不支持的 exec 类型。
// 尽量按命名约定在对应 "<prefix>_result" 里塞 error，让上游立刻看到拒绝原因；
// schema 里没有匹配结构时退化为只回 id（上游等结果直到停滞超时，至少客户端能感知）。
func (r *Run) replyExecRejected(execID uint32, execIDStr, name string) {
	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return
	}
	ecm := sub(acm, "exec_client_message")
	setUint(ecm, "id", execID)
	if execIDStr != "" {
		setStr(ecm, "exec_id", execIDStr)
	}
	fillExecError(ecm, name, "rejected by cursor2api: unsupported exec "+name)
	_ = r.send(acm)
}

// fillExecError 按 "<prefix>_result" 约定找结果字段，填 error/failure/rejected。
// 全程 ByName 防御性查找：schema 结构不符时不填也不 panic（本就在 recover 外的异步 goroutine）。
func fillExecError(ecm *dynamicpb.Message, name, msg string) {
	resultField := strings.TrimSuffix(name, "_args") + "_result"
	fd := ecm.Descriptor().Fields().ByName(protoreflect.Name(resultField))
	if fd == nil || fd.Kind() != protoreflect.MessageKind {
		return
	}
	res := ecm.Mutable(fd).Message()
	for _, errField := range []string{"error", "failure", "rejected"} {
		ef := res.Descriptor().Fields().ByName(protoreflect.Name(errField))
		if ef == nil || ef.Kind() != protoreflect.MessageKind {
			continue
		}
		em := res.Mutable(ef).Message()
		// error/failure 结构里找 string 字段塞原因；rejected 这类标记型只设置存在性
		for _, tf := range []string{"error", "abort_reason", "stderr"} {
			sf := em.Descriptor().Fields().ByName(protoreflect.Name(tf))
			if sf != nil && sf.Kind() == protoreflect.StringKind {
				em.Set(sf, protoreflect.ValueOfString(msg))
				break
			}
		}
		return
	}
}

func (r *Run) handleKV(kv *dynamicpb.Message) {
	kvID := getUint(kv, "id")
	if args, ok := get(kv, "set_blob_args"); ok {
		idFd := args.Descriptor().Fields().ByName("blob_id")
		dataFd := args.Descriptor().Fields().ByName("blob_data")
		if idFd != nil && dataFd != nil {
			data := args.Get(dataFd).Bytes()
			r.blobsMu.Lock()
			// 总量上限：长会话 blob 单调累积，防内存无界增长；
			// 超限拒存（后续 get_blob 未命中，语义安全）
			if r.blobBytes+len(data) <= maxBlobCacheBytes {
				key := string(args.Get(idFd).Bytes())
				r.blobBytes += len(data) - len(r.blobs[key])
				r.blobs[key] = data
			} else {
				dlog("blob cache full (%d bytes), dropping set_blob", r.blobBytes)
			}
			r.blobsMu.Unlock()
		}
	}
	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return
	}
	kcm := sub(acm, "kv_client_message")
	setUint(kcm, "id", kvID)
	if has(kv, "set_blob_args") {
		sub(kcm, "set_blob_result")
	} else if args, ok := get(kv, "get_blob_args"); ok {
		res := sub(kcm, "get_blob_result")
		idFd := args.Descriptor().Fields().ByName("blob_id")
		if idFd != nil {
			id := args.Get(idFd).Bytes()
			r.blobsMu.Lock()
			data, found := r.blobs[string(id)]
			r.blobsMu.Unlock()
			// 注：proto3 空 bytes 不序列化——"存过空 blob"与"未存过"在 wire 上
			// 编码相同，无法真正区分；上游若依赖显式空语义需注意
			if found {
				fdData := fdOf(res.Descriptor(), "blob_data")
				res.Set(fdData, protoreflect.ValueOfBytes(data))
			}
		}
	}
	_ = r.send(acm)
}

// execShellStream 执行 shell_stream_args：start → stdout → exit 帧序列。
// 在独立 goroutine 中运行（handleExec 异步派发），不阻塞事件循环。
func (r *Run) execShellStream(execID uint32, execIDStr string, args *dynamicpb.Message) {
	command := getStr(args, "command")
	workDir := getStr(args, "working_directory")
	if workDir == "" {
		workDir = "."
	}
	timeoutMs := getInt64(args, "timeout")
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	if timeoutMs > maxShellTimeoutMs {
		dlog("shell_stream: clamp timeout %dms -> %dms", timeoutMs, maxShellTimeoutMs)
		timeoutMs = maxShellTimeoutMs
	}

	sendStream := func(fill func(ss *dynamicpb.Message)) {
		acm, err := r.reg.New("agent.v1.AgentClientMessage")
		if err != nil {
			return
		}
		ecm := sub(acm, "exec_client_message")
		setUint(ecm, "id", execID)
		// 注意：CLI 的 shell_stream 回包不带 exec_id，带了服务端不认
		fdT := fdOf(ecm.Descriptor(), "local_execution_time_ms")
		ecm.Set(fdT, protoreflect.ValueOfInt32(500))
		ss := sub(ecm, "shell_stream")
		fill(ss)
		_ = r.send(acm)
	}

	// start（带 sandbox policy，服务端需要）
	// 注：type=1 是对齐真实 CLI 行为的协议常量（不带此帧服务端不确认执行）。
	// 桥接层本身不实现文件系统沙箱——agent 模式的定位即"API key = 本机 shell
	// 权限"（见 main.go 启动警告），审批/围栏由上层客户端（Claude Code）承担。
	sendStream(func(ss *dynamicpb.Message) {
		st := sub(ss, "start")
		sp := sub(st, "sandbox_policy")
		setInt(sp, "type", 1)
	})

	// 执行（超时强杀；Close 时也强杀，避免 run 结束后留下孤儿进程）
	cmd := shellCommand(command)
	cmd.Dir = workDir
	setProcGroup(cmd)
	// WaitDelay 兜底：后台孙进程继承输出管道时 Wait 不会被管道卡住，
	// 进程死后最多再等 waitDelay 强制返回（timeout/Close 回收路径不被楔死）。
	const waitDelay = 5 * time.Second
	cmd.WaitDelay = waitDelay
	// 输出限量收集：`yes` 类命令在超时窗口内能攒出 GB 级输出
	stdoutBuf := &cappedWriter{limit: maxToolOutputBytes}
	stderrBuf := &cappedWriter{limit: maxToolOutputBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf
	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Run() }()
	exitCode := uint32(0)
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			exitCode = 1
			if ee, ok := err.(*exec.ExitError); ok {
				// 信号致死时 ExitCode() 返回 -1，uint32 转换会溢出成 4294967295
				if code := ee.ExitCode(); code >= 0 {
					exitCode = uint32(code)
				}
			} else {
				// 非退出类错误（chdir 失败、可执行文件缺失等）：把真实原因交给模型，
				// 否则模型看到"exit 1 且无任何输出"，无法自愈
				stderrBuf.WriteString(err.Error() + "\n")
			}
		}
	case <-timer.C:
		killTree(cmd)
		<-errCh
		exitCode = 124
	case <-r.closeCh:
		killTree(cmd)
		<-errCh
		return
	}

	// 节奏对齐 CLI：start 与结果之间留出处理时间，避免服务端竞态丢帧
	// （Close 时立即返回，不等节拍——run 已死，后续帧反正发不出去）
	select {
	case <-time.After(300 * time.Millisecond):
	case <-r.closeCh:
		return
	}
	if out := stdoutBuf.String(); out != "" {
		sendStream(func(ss *dynamicpb.Message) {
			so := sub(ss, "stdout")
			setStr(so, "data", out)
		})
	}
	if out := stderrBuf.String(); out != "" {
		sendStream(func(ss *dynamicpb.Message) {
			se := sub(ss, "stderr")
			setStr(se, "data", out)
		})
	}
	select {
	case <-time.After(200 * time.Millisecond):
	case <-r.closeCh:
		return
	}
	sendStream(func(ss *dynamicpb.Message) {
		ex := sub(ss, "exit")
		fdC := fdOf(ex.Descriptor(), "code")
		ex.Set(fdC, protoreflect.ValueOfUint32(exitCode))
		setStr(ex, "cwd", workDir)
		fdL := fdOf(ex.Descriptor(), "local_execution_time_ms")
		ex.Set(fdL, protoreflect.ValueOfInt32(500))
	})

	// exec 完成信号（CLI 实测：streamClose 带 exec id 才会被服务端确认完成）
	_ = r.sendStreamClose(execID)
}

// sendStreamClose 发送 exec 关闭信号（id=0 表示无 id，用于 request_context 等无 id exec）。
func (r *Run) sendStreamClose(id uint32) error {
	acm, err := r.reg.New("agent.v1.AgentClientMessage")
	if err != nil {
		return err
	}
	ctrl := sub(acm, "exec_client_control_message")
	sc := sub(ctrl, "stream_close")
	if id > 0 {
		setUint(sc, "id", id)
	}
	return r.send(acm)
}
