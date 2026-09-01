package cursor

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// getUint/getInt64 对 kind 不符的字段必须容忍转换而不是 panic：
// Value.Uint()/.Int() 在 kind 不匹配时直接 panic，schema 漂移（重提取后
// 字段类型变化）会把一次统计读取变成整个 run 的崩溃。
func TestGettersKindTolerant(t *testing.T) {
	reg := testRegistry(t)

	// head_limit 是 int32：getUint 曾在此 panic
	args := newGrepArgs(t, reg, map[string]any{"head_limit": int32(42)})
	if got := getUint(args, "head_limit"); got != 42 {
		t.Fatalf("getUint(int32 field) = %d, want 42", got)
	}
	if got := getInt64(args, "head_limit"); got != 42 {
		t.Fatalf("getInt64(int32 field) = %d, want 42", got)
	}

	// 非整数 kind（bool）：返回零值而不是 panic
	fd := args.Descriptor().Fields().ByName("case_insensitive")
	args.Set(fd, protoreflect.ValueOfBool(true))
	if got := getUint(args, "case_insensitive"); got != 0 {
		t.Fatalf("getUint(bool field) = %d, want 0", got)
	}
	if got := getInt64(args, "case_insensitive"); got != 0 {
		t.Fatalf("getInt64(bool field) = %d, want 0", got)
	}
}
