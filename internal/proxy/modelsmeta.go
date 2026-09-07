// modelsmeta：/v1/models 响应的元数据可信化（P4，2026-09-07 实测新增）。
//
// 上游 /v1/models 声明的 max_model_len 不可信——codely-core 虚标 1M（1048576），真实
// 后端是 GLM-5 系 128K（PROTOCOL.md §4.0：窗口以 backend-probe 实测为准）。客户端按
// 该字段估算可发长度会被误导（超发即截断/报错）。代理按 alias→真实窗口静态表覆写
// 200 响应中的该字段；表与 oauth.BackendMeta（internal/oauth/apikey.go，后端→窗口静态
// 知识）同源，上游若改 alias→后端映射需同步两处。
package proxy

import (
	"bytes"
	"encoding/json"
)

// modelAliasContextWindow alias → 真实上下文窗口（值取自 oauth.BackendMeta 的后端窗口：
// core=GLM-5 系 131072；flash/air/basic=deepseek-v4-flash 1048576；vl=qwen3.5 131072，
// 见 PROTOCOL.md §4.0 映射表）。
var modelAliasContextWindow = map[string]int{
	"codely-core":  131072,
	"codely-vl":    131072,
	"codely-flash": 1048576,
	"codely-air":   1048576,
	"codely-basic": 1048576,
}

// OverrideModelsMeta 覆写 GET /v1/models 200 响应中的虚标 max_model_len。仅改动表中
// alias 且已携带该字段的条目（最小干预：修虚标，不新增字段）；解析失败/无 data/无命中
// 原样返回（可能同一切片）。
func OverrideModelsMeta(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"max_model_len"`)) { // 廉价预检，误报无害
		return body
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	rawData, ok := top["data"]
	if !ok {
		return body
	}
	var entries []json.RawMessage
	if json.Unmarshal(rawData, &entries) != nil {
		return body
	}
	changed := false
	for i, er := range entries {
		var em map[string]json.RawMessage
		if json.Unmarshal(er, &em) != nil {
			continue
		}
		rawID, ok := em["id"]
		if !ok {
			continue
		}
		var id string
		if json.Unmarshal(rawID, &id) != nil {
			continue
		}
		win, ok := modelAliasContextWindow[id]
		if !ok {
			continue
		}
		if _, has := em["max_model_len"]; !has {
			continue // 未声明的条目不新增字段（最小干预）
		}
		nb, err := json.Marshal(win)
		if err != nil {
			continue
		}
		if bytes.Equal(nb, em["max_model_len"]) {
			continue // 已是真实值，不重排字节
		}
		em["max_model_len"] = nb
		if ner, err := json.Marshal(em); err == nil {
			entries[i] = ner
			changed = true
		}
	}
	if !changed {
		return body
	}
	top["data"], _ = json.Marshal(entries)
	res, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return res
}