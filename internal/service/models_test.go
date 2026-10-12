package service

import (
	"encoding/json"
	"testing"
)

// 合成一份形似 claude.ai bootstrap 的响应：model_selector_config 下按 surface 分组，
// 每组 models 里是带 id/name 的条目；同时混入组织 UUID 等噪声，验证不会被误收。
const bootstrapSample = `{
  "account": {"uuid": "11111111-2222-3333-4444-555555555555", "email_address": "a@b.c"},
  "model_selector_config": {
    "chat": {
      "models": [
        {"id": "claude-sonnet-5", "name": "Sonnet 5"},
        {"id": "claude-haiku-5-5-20261001", "name": "Haiku 5.5"},
        {"id": "claude-opus-4-8", "name": "Opus 4.8"}
      ],
      "default_model": "claude-sonnet-5"
    },
    "code": {
      "models": [
        {"model": "claude-sonnet-5", "name": "Sonnet 5"},
        {"model_id": "claude-haiku-4-5-20251001", "name": "Haiku 4.5"}
      ]
    }
  },
  "organization": {"id": "99999999-8888-7777-6666-555555555555"}
}`

func TestExtractModelIDs(t *testing.T) {
	var root any
	if err := json.Unmarshal([]byte(bootstrapSample), &root); err != nil {
		t.Fatal(err)
	}
	got := ExtractModelIDs(root)
	want := []string{
		"claude-sonnet-5",
		"claude-haiku-5-5-20261001",
		"claude-opus-4-8",
		"claude-haiku-4-5-20251001",
	}
	if len(got) != len(want) {
		t.Fatalf("数量不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个不符: got %v want %v", i, got, want)
		}
	}
}

func TestExtractModelIDsEmpty(t *testing.T) {
	var root any
	if err := json.Unmarshal([]byte(`{"account":{"uuid":"x"}}`), &root); err != nil {
		t.Fatal(err)
	}
	if got := ExtractModelIDs(root); len(got) != 0 {
		t.Fatalf("无模型字段时应返回空, got %v", got)
	}
}

// 用 2026-10-11 真号实测同步到的 16 个上游模型做回归样本：
// 免费计划只能保留 Sonnet/Haiku 两家，Opus/Fable 必须全部滤掉且顺序不变。
func TestFilterFreePlanModels(t *testing.T) {
	in := []string{
		"claude-fable-5-1", "claude-opus-5-5", "claude-opus-4-5-20251101",
		"claude-opus-4-1-20250805-claude-ai", "claude-sonnet-5-5", "claude-haiku-5-5",
		"claude-fable-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7",
		"claude-opus-4-6", "claude-3-opus-20240229", "claude-sonnet-5",
		"claude-sonnet-4-6", "claude-haiku-4-5-20251001", "claude-sonnet-4-5-20250929",
	}
	want := []string{
		"claude-sonnet-5-5", "claude-haiku-5-5", "claude-sonnet-5",
		"claude-sonnet-4-6", "claude-haiku-4-5-20251001", "claude-sonnet-4-5-20250929",
	}
	got := FilterFreePlanModels(in)
	if len(got) != len(want) {
		t.Fatalf("数量不符: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个不符: got %v want %v", i, got, want)
		}
	}
	if empty := FilterFreePlanModels(nil); len(empty) != 0 {
		t.Fatalf("空输入应返回空, got %v", empty)
	}
}

func TestIsModelID(t *testing.T) {
	if !isModelID("claude-haiku-5-5-20261001") {
		t.Fatal("合法模型 id 被拒绝")
	}
	for _, bad := range []string{"gpt-5", "claude", "claude x", "11111111-2222-3333-4444-555555555555"} {
		if isModelID(bad) {
			t.Fatalf("非法值被接受: %q", bad)
		}
	}
}
