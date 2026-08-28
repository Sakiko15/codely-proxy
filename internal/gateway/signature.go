// Package gateway 承载对 Codely 上游网关的纯协议常量与算法（不发起网络请求）。
//
// 权威来源：docs/PROTOCOL.md §2.4（X-Codely-Signature 签名方案，逆向自官方 CLI bundle）。
// 本实现与 codely-auth.js 的 codelySignature / signRequest 逐字节一致（golden test 对拍）。
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"time"
)

// signingSecret 是官方 CLI 内置的签名密钥（bundle 明文，无隐私性，见 PROTOCOL.md §2.4）。
const signingSecretHex = "406f00f74768ba0cb0cd30f097ec6c2bdacb89c61a38b7dd140838bbd0e98018"

var signingSecret = mustHex(signingSecretHex)

// codelySigningLabel 是 HMAC 派生密钥用的固定 label（官方 `VCt` fetch 包装器）。
const codelySigningLabel = "codely-signing-v1"

func mustHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		hi, lo := unhex(s[2*i]), unhex(s[2*i+1])
		if hi == 0xff || lo == 0xff {
			// 逻辑审查 P2：unhex 对非法字符返回 0xff——原 hi<0 恒假（byte 无负值），守卫曾是死代码
			panic("gateway: 非法 hex 常量 " + signingSecretHex)
		}
		b[i] = hi<<4 | lo
	}
	return b
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0xff
}

// hmacSHA256 返回 HMAC-SHA256(key, msg) 的原始字节。
func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// signingK1 派生密钥第一层：HMAC(SECRET, label)。仅依赖两个常量，init 计算一次
//（审查记录 P2 #2：此前每请求重算；A2 禁的是绑定 key/path/time 的签名缓存，此层不涉及）。
var signingK1 = hmacSHA256(signingSecret, []byte(codelySigningLabel))

// CodelySignature 计算指定时刻的签名头值（供 golden test 与 signRequest 复用）。
// 算法（PROTOCOL.md §2.4）：
//
//	k1         = HMAC-SHA256(SECRET, "codely-signing-v1")
//	signingKey = HMAC-SHA256(k1, <sk- 密钥>)
//	sig        = HMAC-SHA256(signingKey, "v1\n<pathname>\n<tsSec>") → base64url
//	返回      = "v1.<tsSec>.<sig>"
//
// tsSec 为 unix 秒字符串。返回头值形如 "v1.1780000000.FkYX0UcxY-..."。
func CodelySignature(apiKey, pathname, tsSec string) string {
	signingKey := hmacSHA256(signingK1, []byte(apiKey))
	sig := hmacSHA256(signingKey, []byte("v1\n"+pathname+"\n"+tsSec))
	return "v1." + tsSec + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// SignRequest 生成"当前时刻"的签名头值。
// ⚠️ 每次请求必须现场生成、不可缓存：签名绑定时间/路径/密钥，网关有新鲜度窗口
//（跨路径复用无效、换 sk- 密钥必须重签，见 PROTOCOL.md §2.4）。
// pathname 必须是不含 query 的纯路径（如 /v1/chat/completions），与 JS 版
// `new URL(upPath, 'http://x').pathname` 的行为一致（见 GO_PORT.md §17.11）。
func SignRequest(apiKey, pathname string) string {
	return CodelySignature(apiKey, pathname, strconv.FormatInt(time.Now().Unix(), 10))
}
