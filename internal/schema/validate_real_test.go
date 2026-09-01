package schema

import "testing"

// 回归：requiredSchema 曾对 ExecClientMessage 多列了不存在的 edit_result 字段，
// 导致 Validate 失败、从源码构建的服务无法启动（线上跑的是旧二进制才没暴露）。
// 用真实 FDS 全量过一遍，防止清单与 schema 双向漂移
// （schema 缺字段 → 运行期 panic；清单多列 → 启动即失败）。
func TestValidateRealSchema(t *testing.T) {
	reg, err := Load("../../schema/cursor_fds.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
