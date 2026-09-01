// Package schema 加载从 cursor-agent bundle 提取的 FileDescriptorSet，
// 提供 dynamicpb 消息构造能力。
package schema

import (
	"fmt"
	"os"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"
)

// Registry 消息描述符注册表。
type Registry struct {
	Files *protoregistry.Files
}

// Load 从 FDS JSON 文件构建注册表。
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fds descriptorpb.FileDescriptorSet
	if err := protojson.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("parse fds: %w", err)
	}
	files := new(protoregistry.Files)
	var regErr error
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		// 同名文件跳过（FDS 若内联 google/protobuf/*.proto 会与全局撞名，
		// 静默吞掉会让后续排查摸不着头脑——冲突在下方 FDS 注册时显式报错）
		if err := files.RegisterFile(fd); err != nil {
			regErr = err
			return false
		}
		return true
	})
	if regErr != nil {
		return nil, fmt.Errorf("seed global files: %w", regErr)
	}
	pending := fds.GetFile()
	var lastErr error
	for len(pending) > 0 {
		var next []*descriptorpb.FileDescriptorProto
		progress := false
		for _, fdp := range pending {
			fd, err := protodesc.NewFile(fdp, files)
			if err != nil {
				lastErr = fmt.Errorf("build %s: %w", fdp.GetName(), err)
				next = append(next, fdp)
				continue
			}
			if err := files.RegisterFile(fd); err != nil {
				return nil, fmt.Errorf("register %s: %w", fdp.GetName(), err)
			}
			progress = true
		}
		if !progress {
			return nil, lastErr
		}
		pending = next
	}
	return &Registry{Files: files}, nil
}

// Message 获取消息描述符。
func (r *Registry) Message(name string) (protoreflect.MessageDescriptor, error) {
	d, err := r.Files.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, err
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a message", name)
	}
	return md, nil
}

// New 创建指定类型的新 dynamicpb 消息。
func (r *Registry) New(name string) (*dynamicpb.Message, error) {
	md, err := r.Message(name)
	if err != nil {
		return nil, err
	}
	return dynamicpb.NewMessage(md), nil
}

// Unmarshal 解码指定类型的消息。
func (r *Registry) Unmarshal(name string, data []byte) (*dynamicpb.Message, error) {
	msg, err := r.New(name)
	if err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// requiredSchema 运行期依赖的关键消息与字段。
// fdOf 缺字段时 panic，启动时一次性校验可以把配置错误暴露在启动阶段，
// 而不是某个请求跑到一半 panic。
// 注意：只列代码实际构造/读取的字段，且必须真实存在——
// 多列（如曾被写入的 ExecClientMessage.edit_result）会让服务无法启动。
var requiredSchema = map[string][]string{
	// ---- 顶层消息 ----
	"agent.v1.AgentClientMessage": {
		"run_request", "client_heartbeat", "exec_client_message",
		"kv_client_message", "exec_client_control_message",
	},
	"agent.v1.AgentServerMessage": {
		"interaction_update", "exec_server_message",
		"kv_server_message", "conversation_checkpoint_update",
	},
	// 注意：当前 schema 的 ExecClientMessage 没有 edit_result 字段
	//（编辑经 write/shell 上报），exec.go 的 edit_args 分支已做防御。
	"agent.v1.ExecClientMessage": {
		"id", "exec_id", "mcp_result", "shell_stream",
		"grep_result", "read_result", "ls_result", "shell_result",
		"write_result", "delete_result",
		"request_context_result", "local_execution_time_ms",
	},
	"agent.v1.GetUsableModelsRequest":  {},
	"agent.v1.GetUsableModelsResponse": {"models"},

	// ---- Run 构造路径（client.go Run，handler goroutine，panic 无防护）----
	"agent.v1.AgentRunRequest": {
		"conversation_state", "action", "requested_model", "mcp_tools",
		"conversation_id", "conversation_group_id", "pre_fetched_blobs",
	},
	"agent.v1.ConversationStateStructure":          {"root_prompt_messages_json", "turns"},
	"agent.v1.ConversationAction":                  {"user_message_action"},
	"agent.v1.UserMessageAction":                   {"user_message", "conversation_history"},
	"agent.v1.UserMessage":                         {"text", "message_id", "mode"},
	"agent.v1.ConversationHistory":                 {"messages"},
	"agent.v1.ConversationHistoryMessage":          {"user", "assistant", "tool"},
	"agent.v1.ConversationHistoryUserMessage":      {"content"},
	"agent.v1.ConversationHistoryAssistantMessage": {"content"},
	"agent.v1.ConversationHistoryToolMessage": {
		"tool_call_id", "tool_name", "is_error", "content",
	},
	"agent.v1.ConversationHistoryTextContent": {"text"},
	"agent.v1.ConversationHistoryToolCall":    {"tool_call_id", "tool_name", "args_json"},
	"agent.v1.RequestedModel":                 {"model_id"},
	"agent.v1.McpToolDefinition": {
		"name", "tool_name", "description", "input_schema_json", "provider_identifier",
	},
	"agent.v1.BlobEntry": {"id", "value"},

	// ---- 工具应答 / 心跳 / KV / 上下文回包路径 ----
	"agent.v1.McpResult":                {"success", "error"},
	"agent.v1.McpSuccess":               {"content"},
	"agent.v1.McpToolResultContentItem": {"text"},
	"agent.v1.McpTextContent":           {"text"},
	"agent.v1.McpError":                 {"error"},
	"agent.v1.ExecClientControlMessage": {"heartbeat", "stream_close"},
	"agent.v1.ExecClientHeartbeat":      {"id"},
	"agent.v1.ExecClientStreamClose":    {"id"},
	"agent.v1.KvClientMessage":          {"id", "set_blob_result", "get_blob_result"},
	"agent.v1.GetBlobResult":            {"blob_data"},
	"agent.v1.RequestContextResult":     {"success"},
	"agent.v1.RequestContextSuccess":    {"request_context"},
	"agent.v1.RequestContext":           {"rules"},
	"agent.v1.CursorRule":               {"content", "full_path", "type"},
	"agent.v1.CursorRuleType":           {"global"},
	"agent.v1.ShellStream":              {"start", "stdout", "stderr", "exit"},
	"agent.v1.ShellStreamStart":         {"sandbox_policy"},
	"agent.v1.SandboxPolicy":            {"type"},
	"agent.v1.ShellStreamStdout":        {"data"},
	"agent.v1.ShellStreamStderr":        {"data"},
	"agent.v1.ShellStreamExit":          {"code", "cwd", "local_execution_time_ms"},

	// ---- 内置工具结果路径（exec.go，guarded 防护但提前暴露更好）----
	"agent.v1.GrepResult":        {"success", "error"},
	"agent.v1.GrepSuccess":       {"pattern", "path", "output_mode", "workspace_results"},
	"agent.v1.GrepError":         {"error"},
	"agent.v1.GrepUnionResult":   {"files", "count", "content"},
	"agent.v1.GrepFilesResult":   {"files", "total_files"},
	"agent.v1.GrepCountResult":   {"counts", "total_matches", "total_files"},
	"agent.v1.GrepFileCount":     {"file", "count"},
	"agent.v1.GrepContentResult": {"matches", "total_lines", "total_matched_lines", "client_truncated", "head_limit_applied"},
	"agent.v1.GrepFileMatch":     {"file", "matches"},
	"agent.v1.GrepContentMatch":  {"line_number", "content", "content_truncated", "is_context_line"},
	"agent.v1.ReadResult":        {"success", "error", "file_not_found"},
	"agent.v1.ReadSuccess": {
		"path", "content", "total_lines", "file_size", "truncated", "range_applied",
	},
	"agent.v1.ReadError":        {"path", "error"},
	"agent.v1.ReadFileNotFound": {"path"},
	"agent.v1.LsResult":         {"success", "error"},
	"agent.v1.LsError":          {"path", "error"},
	"agent.v1.LsSuccess":        {"directory_tree_root"},
	"agent.v1.LsDirectoryTreeNode": {
		"abs_path", "children_dirs", "children_files", "num_files", "children_were_processed",
	},
	"agent.v1.ShellResult": {"success", "failure", "timeout"},
	"agent.v1.ShellSuccess": {
		"command", "working_directory", "exit_code", "stdout", "stderr", "interleaved_output",
	},
	"agent.v1.ShellFailure": {
		"command", "working_directory", "exit_code", "stdout", "stderr", "interleaved_output",
	},
	"agent.v1.ShellTimeout":       {"command", "working_directory", "timeout_ms"},
	"agent.v1.WriteResult":        {"success", "error"},
	"agent.v1.WriteSuccess":       {"path", "lines_created", "file_size", "file_content_after_write"},
	"agent.v1.WriteError":         {"path", "error"},
	"agent.v1.DeleteResult":       {"success", "error", "file_not_found", "not_file"},
	"agent.v1.DeleteSuccess":      {"path", "deleted_file", "file_size"},
	"agent.v1.DeleteError":        {"path", "error"},
	"agent.v1.DeleteFileNotFound": {"path"},
	"agent.v1.DeleteNotFile":      {"path"},
	"agent.v1.EditResult":         {"success", "error"},
	"agent.v1.EditSuccess": {
		"path", "lines_added", "lines_removed",
		"before_full_file_content", "after_full_file_content",
	},
	"agent.v1.EditError": {"path", "error", "model_visible_error"},
}

// Validate 校验关键消息与字段存在，启动时调用。
func (r *Registry) Validate() error {
	for msgName, fields := range requiredSchema {
		md, err := r.Message(msgName)
		if err != nil {
			return fmt.Errorf("schema: %v", err)
		}
		for _, f := range fields {
			if md.Fields().ByName(protoreflect.Name(f)) == nil {
				return fmt.Errorf("schema: no field %s in %s", f, msgName)
			}
		}
	}
	// kind/cardinality 校验：字段名不变但类型变了（重提取 schema 漂移）时，
	// 只查名字会全绿通过，运行期 sub()/setUint() 才在请求中途 panic。
	for msgName, fields := range requiredFieldKinds {
		md, err := r.Message(msgName)
		if err != nil {
			return fmt.Errorf("schema: %v", err)
		}
		for f, want := range fields {
			fd := md.Fields().ByName(protoreflect.Name(f))
			if fd == nil {
				return fmt.Errorf("schema: no field %s in %s", f, msgName)
			}
			if fd.Kind() != want {
				return fmt.Errorf("schema: field %s in %s: kind = %v, want %v", f, msgName, fd.Kind(), want)
			}
		}
	}
	for msgName, fields := range requiredRepeatedFields {
		md, err := r.Message(msgName)
		if err != nil {
			return fmt.Errorf("schema: %v", err)
		}
		for _, f := range fields {
			fd := md.Fields().ByName(protoreflect.Name(f))
			if fd == nil {
				return fmt.Errorf("schema: no field %s in %s", f, msgName)
			}
			if !fd.IsList() {
				return fmt.Errorf("schema: field %s in %s: not repeated (append/List path would panic)", f, msgName)
			}
		}
	}
	return nil
}

// requiredFieldKinds 数值字段的期望 kind（setUint/setInt 路径：kind 不符 Set 会 panic）。
var requiredFieldKinds = map[string]map[string]protoreflect.Kind{
	"agent.v1.ExecServerMessage":     {"id": protoreflect.Uint32Kind},
	"agent.v1.ExecClientMessage":     {"id": protoreflect.Uint32Kind, "local_execution_time_ms": protoreflect.Int32Kind},
	"agent.v1.ExecClientHeartbeat":   {"id": protoreflect.Uint32Kind},
	"agent.v1.ExecClientStreamClose": {"id": protoreflect.Uint32Kind},
	"agent.v1.KvServerMessage":       {"id": protoreflect.Uint32Kind},
	"agent.v1.KvClientMessage":       {"id": protoreflect.Uint32Kind},
	"agent.v1.ShellStreamExit":       {"code": protoreflect.Uint32Kind, "local_execution_time_ms": protoreflect.Int32Kind},
	"agent.v1.UserMessage":           {"mode": protoreflect.Int32Kind},
	"agent.v1.SandboxPolicy":         {"type": protoreflect.Int32Kind},
	"agent.v1.ReadSuccess":           {"file_size": protoreflect.Int64Kind},
	"agent.v1.DeleteSuccess":         {"file_size": protoreflect.Int64Kind},
	"agent.v1.GrepContentMatch":      {"line_number": protoreflect.Int32Kind},
	"agent.v1.GrepFileCount":         {"count": protoreflect.Int32Kind},
	"agent.v1.ShellTimeout":          {"timeout_ms": protoreflect.Int32Kind},
	"agent.v1.LsDirectoryTreeNode":   {"num_files": protoreflect.Int32Kind},
	"agent.v1.ShellSuccess":          {"exit_code": protoreflect.Int32Kind},
	"agent.v1.ShellFailure":          {"exit_code": protoreflect.Int32Kind},
	"agent.v1.WriteSuccess":          {"lines_created": protoreflect.Int32Kind, "file_size": protoreflect.Int32Kind},
	"agent.v1.EditSuccess":           {"lines_added": protoreflect.Int32Kind, "lines_removed": protoreflect.Int32Kind},
	"agent.v1.GrepFilesResult":       {"total_files": protoreflect.Int32Kind},
	"agent.v1.GrepCountResult":       {"total_matches": protoreflect.Int32Kind, "total_files": protoreflect.Int32Kind},
	"agent.v1.GrepContentResult":     {"total_lines": protoreflect.Int32Kind, "total_matched_lines": protoreflect.Int32Kind, "head_limit_applied": protoreflect.Int32Kind},
}

// requiredRepeatedFields 必须是 repeated 的字段（appendSub/Mutable().List() 路径）。
var requiredRepeatedFields = map[string][]string{
	"agent.v1.ConversationStateStructure": {"root_prompt_messages_json", "turns", "pending_tool_calls"},
	"agent.v1.ConversationHistory":        {"messages"},
	"agent.v1.LsDirectoryTreeNode":        {"children_dirs", "children_files"},
	"agent.v1.GrepFilesResult":            {"files"},
	"agent.v1.GrepCountResult":            {"counts"},
	"agent.v1.GrepContentResult":          {"matches"},
	"agent.v1.GrepFileMatch":              {"matches"},
}
