package service

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"claude2api/internal/config"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 动态模型发现（二开）：
// 原版 /v1/models 只返回源码里写死的 supportedModels 清单，上游新模型永远刷不出来。
// 改为用号池里某个可用账号的会话去读 claude.ai 的 bootstrap 响应，
// 把其中的 model_selector_config（网页端模型选择器的数据源）里出现的模型 id
// 提取出来，再按免费计划过滤（见 FilterFreePlanModels）后暴露出去。
//
// 兜底原则：拿不到上游清单时（无可用账号、请求失败、解析为空）返回 nil，
// 调用方回落到原版静态清单，保证 /v1/models 永不空、永不挂。
// 缓存 30 分钟，避免客户端频繁刷 /v1/models 时反复打上游；
// 失败后 2 分钟内不重试，避免上游异常时被放大成请求风暴。

const (
	dynModelsTTL     = 30 * time.Minute
	dynModelsBackoff = 2 * time.Minute
)

var (
	dynModelsMu      sync.Mutex
	dynModelsCache   []string
	dynModelsAt      time.Time
	dynModelsTriedAt time.Time
)

// DynamicModels 返回从上游 bootstrap 读到的模型 id 列表；不可用时返回 nil。
func DynamicModels() []string {
	dynModelsMu.Lock()
	defer dynModelsMu.Unlock()

	now := time.Now()
	if len(dynModelsCache) > 0 && now.Sub(dynModelsAt) < dynModelsTTL {
		return dynModelsCache
	}
	if now.Sub(dynModelsTriedAt) < dynModelsBackoff {
		return dynModelsCache
	}
	dynModelsTriedAt = now

	acct := pickAPIAccount()
	if acct == nil {
		return dynModelsCache
	}
	lease := clientFor(acct, config.Get().Proxy)
	lease.Lock()
	ids, err := lease.ClaudeAI.FetchBootstrapModels()
	lease.Unlock()
	if err != nil {
		slog.Warn("[Models] 读取上游模型清单失败，继续使用缓存/静态清单", "email", acct.Email, "err", err)
		return dynModelsCache
	}
	kept := FilterFreePlanModels(ids)
	if len(kept) == 0 {
		slog.Warn("[Models] 上游清单过滤后为空，继续使用缓存/静态清单", "email", acct.Email, "upstream_count", len(ids))
		return dynModelsCache
	}
	dynModelsCache = kept
	dynModelsAt = now
	slog.Info("[Models] 已从上游 bootstrap 同步模型清单", "email", acct.Email, "count", len(kept), "upstream_count", len(ids), "models", kept)
	return dynModelsCache
}

// FilterFreePlanModels 只保留免费计划能用的模型。
// bootstrap 返回的是全量产品目录（含 Opus/Fable 等付费型号），而号池里是免费账号：
// 按 Anthropic 的套餐划分，免费计划只有 Sonnet 与 Haiku 两家，Opus 需 Pro 起，
// Fable 需 Pro 付点数或 Max。这里按家族过滤而不是写死具体型号，
// 这样新款 Sonnet/Haiku 发布后仍能自动流入清单，Opus/Fable 不会再混进来。
func FilterFreePlanModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if isFreePlanModel(id) {
			out = append(out, id)
		}
	}
	return out
}

func isFreePlanModel(id string) bool {
	return strings.HasPrefix(id, "claude-sonnet-") || strings.Contains(id, "-sonnet-") ||
		strings.HasPrefix(id, "claude-haiku-") || strings.Contains(id, "-haiku-")
}

// FetchBootstrapModels 拉取 bootstrap 响应并提取模型 id。
// 依次尝试网页端与桌面端两个已知入口，任一成功且解析出模型即返回。
func (claudeAI *ClaudeAI) FetchBootstrapModels() ([]string, error) {
	candidates := []string{
		claudeAIBaseURL + "/api/bootstrap",
		claudeAIBaseURL + "/edge-api/bootstrap",
	}
	if claudeAI.orgUUID != "" {
		candidates = append(candidates,
			claudeAIBaseURL+"/edge-api/bootstrap/"+claudeAI.orgUUID+"/app_start")
	}

	var lastErr error
	for _, target := range candidates {
		ids, err := claudeAI.fetchBootstrapModelsFrom(target)
		if err == nil && len(ids) > 0 {
			return ids, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("bootstrap 中未找到 model_selector_config 模型条目")
	}
	return nil, lastErr
}

func (claudeAI *ClaudeAI) fetchBootstrapModelsFrom(target string) ([]string, error) {
	req, err := claudeAI.request(fhttp.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 bootstrap 请求失败: %w", err)
	}
	req.Header.Del("content-type")
	req.Header.Set("accept", "application/json")

	resp, err := claudeAI.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 bootstrap 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 bootstrap 响应失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return nil, fmt.Errorf("bootstrap HTTP %d", resp.StatusCode)
	}

	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("解析 bootstrap 响应失败: %w", err)
	}
	return ExtractModelIDs(root), nil
}

// ExtractModelIDs 从 bootstrap JSON 中提取模型 id。
// 优先只扫 model_selector_config 子树（模型选择器的权威数据源）；
// 该字段缺失时退回全文档扫描。条目里的 id / model / model_id 三种键名都认，
// 值必须形如 claude-* 的模型 id，去重并保留首次出现顺序。
func ExtractModelIDs(root any) []string {
	scope := root
	if m, ok := root.(map[string]any); ok {
		if cfg, ok := m["model_selector_config"]; ok {
			scope = cfg
		}
	}
	seen := map[string]bool{}
	out := []string{}
	collectModelIDs(scope, seen, &out)
	return out
}

func collectModelIDs(v any, seen map[string]bool, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for _, key := range []string{"id", "model", "model_id"} {
			if s, ok := t[key].(string); ok && isModelID(s) && !seen[s] {
				seen[s] = true
				*out = append(*out, s)
			}
		}
		for _, child := range t {
			collectModelIDs(child, seen, out)
		}
	case []any:
		for _, child := range t {
			collectModelIDs(child, seen, out)
		}
	}
}

// isModelID 粗筛模型 id：claude- 前缀、无空白与路径字符，挡掉组织/会话等 UUID 噪声。
func isModelID(s string) bool {
	if !strings.HasPrefix(s, "claude-") {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n/")
}
