// dynamicpb 消息构造辅助。
package cursor

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func fdOf(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fd := md.Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		panic("no field " + name + " in " + string(md.FullName()))
	}
	return fd
}

// sub 创建并挂载子消息。
func sub(parent *dynamicpb.Message, fieldName string) *dynamicpb.Message {
	fd := fdOf(parent.Descriptor(), fieldName)
	m := dynamicpb.NewMessage(fd.Message())
	parent.Set(fd, protoreflect.ValueOfMessage(m))
	return m
}

// appendSub 向 repeated message 字段追加一个元素。
func appendSub(parent *dynamicpb.Message, fieldName string) *dynamicpb.Message {
	fd := fdOf(parent.Descriptor(), fieldName)
	list := parent.Mutable(fd).List()
	m := dynamicpb.NewMessage(fd.Message())
	list.Append(protoreflect.ValueOfMessage(m))
	return m
}

func setStr(msg *dynamicpb.Message, fieldName, value string) {
	if value == "" {
		return
	}
	msg.Set(fdOf(msg.Descriptor(), fieldName), protoreflect.ValueOfString(value))
}

func setInt(msg *dynamicpb.Message, fieldName string, value int32) {
	msg.Set(fdOf(msg.Descriptor(), fieldName), protoreflect.ValueOfInt32(value))
}

func setUint(msg *dynamicpb.Message, fieldName string, value uint32) {
	msg.Set(fdOf(msg.Descriptor(), fieldName), protoreflect.ValueOfUint32(value))
}

func setBool(msg *dynamicpb.Message, fieldName string, value bool) {
	msg.Set(fdOf(msg.Descriptor(), fieldName), protoreflect.ValueOfBool(value))
}

// get 读取子消息（不存在返回 nil,false）。
func get(msg *dynamicpb.Message, fieldName string) (*dynamicpb.Message, bool) {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil || !msg.Has(fd) {
		return nil, false
	}
	return msg.Get(fd).Message().(*dynamicpb.Message), true
}

func getStr(msg *dynamicpb.Message, fieldName string) string {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil {
		return ""
	}
	return msg.Get(fd).String()
}

// getUint 读取整数字段为 uint32。
// 按 kind 分派转换：Value.Uint() 在非 uint kind 上直接 panic，
// schema 漂移（重提取后字段类型变化）会把一次统计读取变成整个 run 的崩溃。
func getUint(msg *dynamicpb.Message, fieldName string) uint32 {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil {
		return 0
	}
	v := msg.Get(fd)
	switch fd.Kind() {
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return uint32(v.Uint())
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		return uint32(v.Int())
	case protoreflect.EnumKind:
		return uint32(v.Enum())
	}
	return 0
}

// getInt64 读取整数字段为 int64（同 getUint 的 kind 容忍理由）。
func getInt64(msg *dynamicpb.Message, fieldName string) int64 {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil {
		return 0
	}
	v := msg.Get(fd)
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint())
	case protoreflect.EnumKind:
		return int64(v.Enum())
	}
	return 0
}

// hasNonZero 字段存在且被设置。
func has(msg *dynamicpb.Message, fieldName string) bool {
	fd := msg.Descriptor().Fields().ByName(protoreflect.Name(fieldName))
	return fd != nil && msg.Has(fd)
}
