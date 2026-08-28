// CLI 身份头组（上游逆向知识，一字不改）。
//
// 位置说明：审查记录 P2 #1——此前定义在 internal/oauth/apikey.go，与 GO_PORT.md §1/§18.1
// （internal/gateway/client_headers.go）矛盾；本包是"纯协议常量"的既定归属，现归位。
package gateway

import "net/http"

// ClientHeaders 是伪造官方 CLI 身份头组（PROTOCOL.md §2.2 / PROTOCOL_SCHEMA.md §16）。
// 与 node 版 CLIENT_HEADERS 逐项一致。注：node 版在 codely-auth.js 与 codely-proxy.js 各维护一份，
// Go 收敛为一处（见 GO_PORT.md §2.1）。
var ClientHeaders = http.Header{
	"User-Agent":                  {"codely-cli/1.0.0-release.41 (win32; x64)"},
	"X-Stainless-Lang":            {"js"},
	"X-Stainless-Package-Version": {"5.11.0"},
	"X-Stainless-OS":              {"Windows"},
	"X-Stainless-Arch":            {"x64"},
	"X-Stainless-Runtime":         {"node"},
	"X-Stainless-Runtime-Version": {"v24.3.0"},
	"X-Stainless-Retry-Count":     {"0"},
}
