// P4 测试：/v1/models 虚标 max_model_len 覆写（OverrideModelsMeta）+ handler 接线。
package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOverrideModelsMeta(t *testing.T) {
	coreBody := `{"object":"list","data":[{"id":"codely-core","object":"model","created":1700000000,"owned_by":"codely","max_model_len":1048576},{"id":"codely-flash","is_alias":true},{"id":"codely-other","max_model_len":999999}]}`
	out := OverrideModelsMeta([]byte(coreBody))
	var j struct {
		Data []struct {
			ID          string `json:"id"`
			Created     int64  `json:"created"`
			MaxModelLen *int   `json:"max_model_len"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &j) != nil {
		t.Fatalf("解析失败: %s", out)
	}
	if len(j.Data) != 3 {
		t.Fatalf("条目数应不变: %s", out)
	}
	if j.Data[0].ID != "codely-core" || j.Data[0].MaxModelLen == nil || *j.Data[0].MaxModelLen != 131072 {
		t.Fatalf("core 应覆写为 131072: %s", out)
	}
	if j.Data[0].Created != 1700000000 {
		t.Fatalf("未触字段值应保留: %s", out)
	}
	if j.Data[1].ID != "codely-flash" && j.Data[1].MaxModelLen != nil {
		t.Fatalf("未声明字段的条目不应新增: %s", out)
	}
	if j.Data[2].MaxModelLen == nil || *j.Data[2].MaxModelLen != 999999 {
		t.Fatalf("表外 alias 不应改动: %s", out)
	}
}

func TestOverrideModelsMetaNoHit(t *testing.T) {
	// 已是真实值 → 原样返回（不重排字节）
	ok := `{"data":[{"id":"codely-core","max_model_len":131072}]}`
	if got := OverrideModelsMeta([]byte(ok)); string(got) != ok {
		t.Fatalf("已正确值应原样: %s", got)
	}
	// 无 max_model_len 关键字（预检命中即返）
	noKey := `{"data":[{"id":"codely-core"}]}`
	if got := OverrideModelsMeta([]byte(noKey)); string(got) != noKey {
		t.Fatalf("无字段应原样: %s", got)
	}
	// 无 data / 畸形
	for _, b := range []string{`{"object":"list"}`, `{not-json`} {
		if got := OverrideModelsMeta([]byte(b)); string(got) != b {
			t.Fatalf("应原样: %s -> %s", b, got)
		}
	}
}

func TestHandlerModelsOverride(t *testing.T) {
	// 端到端：GET /v1/models 200 → 虚标覆写；POST 不覆写
	mock := `{"object":"list","data":[{"id":"codely-core","max_model_len":1048576}]}`
	h, _, _, cleanup := buildHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mock))
	})
	defer cleanup()

	rw := doReq(t, h, "GET", "/v1/models", "", nil)
	if rw.Code != 200 {
		t.Fatalf("应 200: %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), `"max_model_len":131072`) {
		t.Fatalf("GET 应覆写: %s", rw.Body.String())
	}

	// 非 GET（POST）同路径不覆写
	rw = doReq(t, h, "POST", "/v1/models", "", nil)
	if strings.Contains(rw.Body.String(), `"max_model_len":131072`) {
		t.Fatalf("非 GET 不应覆写: %s", rw.Body.String())
	}
}