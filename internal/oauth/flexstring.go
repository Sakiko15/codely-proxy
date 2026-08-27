package oauth

import (
	"encoding/json"
	"fmt"
)

// FlexString 兼容 string / number / null 三态的字段（如 user_id、quota_points）。
//
// 背景（PROTOCOL_SCHEMA.md §0）：JS 现实现里 `login.js` 写 `String(me.id)` 存 string，
// 但 e2e 测试直接传 number `10001`，且上游接口（usage/summary 等）同样混用两种形态。
// 上游/既有数据不确定性高 → Go 侧不擅自收紧，用 FlexString 兼容两种。
type FlexString string

// UnmarshalJSON 兼容 string（直接）、number（转十进制字符串）、null（空串）。
func (f *FlexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*f = FlexString(n.String())
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	return fmt.Errorf("FlexString: 无法解析 %s", b)
}

// String 返回 string 值（兼容 String() 语义）。
func (f FlexString) String() string { return string(f) }