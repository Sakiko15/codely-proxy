package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codely-proxy/internal/oauth"
)

// setup 建临时数据目录 + 空注册表，返回清理函数。
func setup(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	oldDataDir := DataDir
	SetDataDir(dir)
	t.Cleanup(func() { SetDataDir(oldDataDir) })
	r := NewRegistry()
	return r
}

// fakeCreds 造一份测试凭据。
func fakeCreds(userID, teamName string) *oauth.Creds {
	exp := time.Now().UnixMilli() + 3600*1000
	return &oauth.Creds{
		AccessToken:  "tok-" + userID,
		RefreshToken: "ref-" + userID,
		UserID:       oauth.FlexString(userID),
		TeamID:       "team-" + userID,
		TeamName:     teamName,
		ExpiryDate:   &exp,
	}
}

// noopPool 空实现 PoolReloader（测试用）。
type noopPool struct{}

func (noopPool) ReloadPool() {}

// ------------- slugify / autoName -------------

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My Team", "my-team"},
		{"Alice Studio", "alice-studio"},
		{"aB_c.D-e", "ab_c.d-e"},
		{"  你好  ", ""},             // 非 ASCII → 全替换为 '-' → 去空
		{"!!!", ""},                 // 非法
		{"", ""},                    // 空
		{"-leading", "leading"},     // 去首尾 '-'
		{"trailing-", "trailing"},   // 去首尾 '-'
		{"a", "a"},                  // 单字符
		{"0123456789012345678901234567890123456789012345678901234567890123X", ""}, // 超 64
		{"con", ""},  // 审查记录 P2 #13：Windows 保留设备名
		{"COM1", ""}, // 大小写不敏感（Slugify 已 lowercase）
		{"lpt9", ""},
		{"nul", ""},
		{"con-x", "con-x"}, // 非保留名不受影响
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAutoName(t *testing.T) {
	if got := AutoName(fakeCreds("10001", "Alice Studio")); got != "alice-studio" {
		t.Fatalf("应优先 team_name: %q", got)
	}
	// team_name 含可 slug 化字符时仍走 team_name（"not valid" → "not-valid"）
	if got := AutoName(fakeCreds("10001", "!!! not valid !!!")); got != "not-valid" {
		t.Fatalf("合法 slug 化后应保留 team_name: %q", got)
	}
	// team_name 纯非 ASCII（slugify 后为空）→ 回退 user_id
	if got := AutoName(fakeCreds("10001", "团队名称")); got != "user-10001" {
		t.Fatalf("team_name 不可用时应回退 user_id: %q", got)
	}
	// 无 team/user → account-<hex>
	c := &oauth.Creds{AccessToken: "x"}
	got := AutoName(c)
	if len(got) != len("account-")+4 {
		t.Fatalf("随机名格式: %q", got)
	}
}

// ------------- 保存 / 列表 / 读取 -------------

func TestSaveListActivate(t *testing.T) {
	r := setup(t)

	// 保存两个账号（第二个激活）
	_, _, err := r.SaveAccount("dev-alice", fakeCreds("10001", "Alice Studio"), true, noopPool{})
	if err != nil {
		t.Fatalf("SaveAccount alice: %v", err)
	}
	_, _, err = r.SaveAccount("dev-bob", fakeCreds("20002", "Bob Inc"), false, noopPool{})
	if err != nil {
		t.Fatalf("SaveAccount bob: %v", err)
	}

	// 列表：两个，alice 当前
	list := r.ListAccounts()
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	cur := r.GetCurrentName()
	if cur != "dev-alice" {
		t.Fatalf("current = %q, want dev-alice", cur)
	}
	if !list[0].IsCurrent {
		t.Fatalf("alice 应为当前账号")
	}

	// 读取凭据
	creds := r.LoadAccountCreds("dev-bob")
	if creds == nil || creds.UserID != "20002" {
		t.Fatalf("loadAccountCreds(bob) 失败: %+v", creds)
	}

	// 激活 bob → 切过去
	acct, key, err := r.ActivateAccount("dev-bob", noopPool{})
	if err != nil {
		t.Fatalf("ActivateAccount(bob): %v", err)
	}
	if acct.Name != "dev-bob" {
		t.Fatalf("激活账号 = %q, want dev-bob", acct.Name)
	}
	_ = key // 预取密钥可能失败（无网络），不阻塞
	if r.GetCurrentName() != "dev-bob" {
		t.Fatalf("激活后 current 应为 dev-bob")
	}
	// 激活后 codely-creds.json 应指向 bob
	creds2 := oauth.LoadCreds()
	if creds2 == nil || creds2.UserID != "20002" {
		t.Fatalf("激活后 codely-creds.json 应为 bob: %+v", creds2)
	}
}

// ------------- 删除 / 级联激活 / 全部删光 -------------

func TestRemoveAccountCascade(t *testing.T) {
	r := setup(t)
	_, _, _ = r.SaveAccount("dev-alice", fakeCreds("10001", "Alice Studio"), true, noopPool{})
	_, _, _ = r.SaveAccount("dev-bob", fakeCreds("20002", "Bob Inc"), false, noopPool{})
	_, _, _ = r.SaveAccount("dev-carol", fakeCreds("30003", "Carol LLC"), false, noopPool{})

	// 删当前（alice）→ 自动激活剩下的第一个（按字母序 bob）
	removed, next, err := r.RemoveAccount("dev-alice", noopPool{})
	if err != nil || !removed {
		t.Fatalf("RemoveAccount(alice): removed=%v err=%v", removed, err)
	}
	if next != "dev-bob" {
		t.Fatalf("级联激活 next = %q, want dev-bob", next)
	}
	if r.GetCurrentName() != "dev-bob" {
		t.Fatalf("删除后 current 应为 dev-bob")
	}
	// alice 文件被删
	if _, err := os.Stat(filepath.Join(AccountsDir, "dev-alice.json")); !os.IsNotExist(err) {
		t.Fatalf("alice 文件应被删除")
	}

	// 全部删光 → 回到未登录
	_, _, _ = r.RemoveAccount("dev-bob", noopPool{})
	_, _, _ = r.RemoveAccount("dev-carol", noopPool{})
	if r.GetCurrentName() != "" {
		t.Fatalf("全部删光后 current 应为空")
	}
	if _, err := os.Stat(oauth.CredsFile); !os.IsNotExist(err) {
		t.Fatalf("全部删光后 codely-creds.json 应被清空")
	}
}

// ------------- 首用自愈（老版本单账号导入） -------------

func TestEnsureRegistryImportsLegacy(t *testing.T) {
	dir := t.TempDir()
	old := DataDir
	SetDataDir(dir)
	t.Cleanup(func() { SetDataDir(old) })

	// 只写 codely-creds.json（老版本单账号），注册表为空
	creds := fakeCreds("55555", "Legacy Co")
	if err := creds.SaveCreds(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry() // ensure 触发导入
	if r.GetCurrentName() == "" {
		t.Fatalf("首用自愈应导入老账号为当前")
	}
	list := r.ListAccounts()
	if len(list) != 1 || list[0].Name != "legacy-co" {
		t.Fatalf("导入结果 = %+v", list)
	}
}

// ------------- 凭据指纹 -------------

func TestCredFingerprint(t *testing.T) {
	a := CredFingerprint(fakeCreds("1", "Team A"))
	b := CredFingerprint(fakeCreds("1", "Team A")) // 同账号同指纹
	c := CredFingerprint(fakeCreds("2", "Team A")) // 不同 user 不同指纹
	if a != b {
		t.Fatalf("同账号指纹应一致")
	}
	if a == c {
		t.Fatalf("不同账号指纹应不同")
	}
}

// ------------- 设备码登录（mock 上游） -------------

func TestDeviceLoginFlow(t *testing.T) {
	// mock 官方端点
	var initiateCalls, exchangeCalls, meCalls, teamsCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			initiateCalls++
			_, _ = w.Write([]byte(`{"auth_request_token":"art-1","verification_uri_complete":"https://codely.tuanjie.cn/auth?code=abc","user_code":"ABC-123","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"authcode-9"}`))
		case "/auth/device/exchange":
			exchangeCalls++
			_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`))
		case "/auth/external/me":
			meCalls++
			_, _ = w.Write([]byte(`{"id":77777}`)) // number → FlexString
		case "/api/teams":
			teamsCalls++
			_, _ = w.Write([]byte(`{"current_team_id":"team-7","teams":[{"team_id":"team-7","team_name":"New Org","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()

	// 替换 oauth.Base 指向 mock
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	flow := NewLoginFlow(r)

	verURI, userCode, exp, iv, err := flow.Start("new-org")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if verURI == "" || userCode != "ABC-123" || exp != 600 || iv != 2 {
		t.Fatalf("Start 返回异常: %q %q %d %d", verURI, userCode, exp, iv)
	}

	// 轮询 → authorized
	status := flow.Poll()
	if status.Status != "authorized" {
		t.Fatalf("Poll status = %q, want authorized (err=%q)", status.Status, status.Error)
	}
	if status.Account == nil || status.Account.Name != "new-org" {
		t.Fatalf("authorized 账号 = %+v, want new-org", status.Account)
	}

	// 账号已登记并激活
	if r.GetCurrentName() != "new-org" {
		t.Fatalf("登录后 current 应为 new-org")
	}
	if initiateCalls != 1 || exchangeCalls != 1 || meCalls != 1 || teamsCalls != 1 {
		t.Fatalf("调用次数异常: init=%d exch=%d me=%d teams=%d", initiateCalls, exchangeCalls, meCalls, teamsCalls)
	}
}

func TestDeviceLoginSameAccount(t *testing.T) {
	// 授权账号 == 当前激活账号 → 不重复添加（same 语义：返回当前，不新增）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art-2","verification_uri_complete":"https://x","user_code":"CODE","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"authcode-2"}`))
		case "/auth/device/exchange":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/auth/external/me":
			_, _ = w.Write([]byte(`{"id":424242}`))
		case "/api/teams":
			_, _ = w.Write([]byte(`{"current_team_id":"team-x","teams":[{"team_id":"team-x","team_name":"Same Org","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	// 预先激活一个同 user 的账号
	_, _, _ = r.SaveAccount("same-org", fakeCreds("424242", "Same Org"), true, noopPool{})

	flow := NewLoginFlow(r)
	_, _, _, _, _ = flow.Start("")
	st := flow.Poll()
	if st.Status != "authorized" || st.Account == nil {
		t.Fatalf("Poll = %+v", st)
	}
	// 同账号：不新增，仍只有 1 个账号
	if len(r.ListAccounts()) != 1 {
		t.Fatalf("同账号登录不应新增，list = %d", len(r.ListAccounts()))
	}
}

// TestPollNoLogin 验证无登录时 idle。
func TestPollNoLogin(t *testing.T) {
	r := setup(t)
	flow := NewLoginFlow(r)
	if st := flow.Poll(); st.Status != "idle" {
		t.Fatalf("无登录时应 idle, got %+v", st)
	}
}

// TestDeviceLoginPollThrottleAndMessage 验证（授权后无限等待修复）：
// 1) 非 2xx 折叠为 pending 但携带状态码与上游错误体摘要；
// 2) 上游 poll 按 initiate 的 interval 节流——前端高频询问不再每次都打上游；
// 3) 节流窗口内回传最近一次异常说明（此前被前端死文案掩盖）。
func TestDeviceLoginPollThrottleAndMessage(t *testing.T) {
	var pollCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"CODE","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			pollCalls++
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	flow := NewLoginFlow(r)
	if _, _, _, _, err := flow.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := flow.Poll()
	if st.Status != "pending" || !strings.Contains(st.Message, "429") {
		t.Fatalf("非 2xx 应 pending 且带状态码, got %+v", st)
	}
	// 节流窗口内的第二次询问：不打上游、回传上次异常说明
	st2 := flow.Poll()
	if st2.Status != "pending" || !strings.Contains(st2.Message, "429") {
		t.Fatalf("节流窗口内应 pending 并回传 message, got %+v", st2)
	}
	if pollCalls != 1 {
		t.Fatalf("节流应挡住第二次上游 poll, pollCalls=%d", pollCalls)
	}
}

// TestDeviceLoginSlowDownBackoff 验证 RFC 8628 退避：slow_down → interval +5（封顶 30）。
// initiate interval=2 → 7；新节奏写进 message，节流窗口内不再打上游。
func TestDeviceLoginSlowDownBackoff(t *testing.T) {
	var pollCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"CODE","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			pollCalls++
			if pollCalls == 1 {
				_, _ = w.Write([]byte(`{"status":"slow_down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"pending"}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	flow := NewLoginFlow(r)
	if _, _, _, _, err := flow.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	st := flow.Poll()
	if st.Status != "pending" || st.Progress != 2 || !strings.Contains(st.Message, "7s") {
		t.Fatalf("slow_down 应 progress=2 且消息含新节奏 7s, got %+v", st)
	}
	st2 := flow.Poll()
	if pollCalls != 1 || st2.Status != "pending" || !strings.Contains(st2.Message, "放慢") {
		t.Fatalf("退避后节流应生效且回传原因, pollCalls=%d st2=%+v", pollCalls, st2)
	}
}

// TestDeviceLoginStalePollNoComplete 验证 CAS：poll 在途时用户重新 Start，
// 迟到的 authorized 不得触发 complete（授权码一次性，会登记用户已放弃的账号），
// 且新登录 slot 不被迟到终态误杀。
func TestDeviceLoginStalePollNoComplete(t *testing.T) {
	pollEntered := make(chan struct{})
	release := make(chan struct{})
	var pollCalls, exchangeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"CODE","interval":2,"expires_in":600}`))
		case "/auth/device/poll":
			pollCalls++
			if pollCalls == 1 {
				close(pollEntered)
				<-release // 第一拍挂住，制造"poll 在途"
			}
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"authcode"}`))
		case "/auth/device/exchange":
			exchangeCalls++
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/auth/external/me":
			_, _ = w.Write([]byte(`{"id":1}`))
		case "/api/teams":
			_, _ = w.Write([]byte(`{"current_team_id":"t","teams":[{"team_id":"t","team_name":"T","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	flow := NewLoginFlow(r)
	if _, _, _, _, err := flow.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	type pollResult struct{ st LoginStatus }
	done := make(chan pollResult, 1)
	go func() { done <- pollResult{flow.Poll()} }()

	<-pollEntered // 第一拍已进入上游 poll（挂住中）
	// 重新发起 → slot 被第二个登录覆盖
	if _, _, _, _, err := flow.Start(""); err != nil {
		t.Fatalf("Start2: %v", err)
	}
	close(release)

	st := (<-done).st
	if st.Status != "idle" {
		t.Fatalf("迟到的 authorized 应 CAS 失败返回 idle, got %+v", st)
	}
	if exchangeCalls != 0 {
		t.Fatalf("过期登录不得触发 exchange, got %d", exchangeCalls)
	}
	if flow.GetInfo() == nil {
		t.Fatalf("新登录的 slot 应保留（不被迟到终态误杀）")
	}
}

// jsonDecode 辅助（编译用）
var _ = json.Marshal

func TestDeviceLoginCollisionEmptyUserID(t *testing.T) {
	// 逻辑审查 P2：userId 为空串不得命中"同属该 user 重建"分支——同名登录应走 -2 后缀
	// 而非静默覆盖既有账号（F5 修复的残留缺口）
	meBody := []byte(`{"id":77777}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"ABC-123","interval":1,"expires_in":600}`))
		case "/auth/device/poll":
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"authcode"}`))
		case "/auth/device/exchange":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/auth/external/me":
			_, _ = w.Write(meBody)
		case "/api/teams":
			_, _ = w.Write([]byte(`{"current_team_id":"team-7","teams":[{"team_id":"team-7","team_name":"New Org","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)

	flow1 := NewLoginFlow(r)
	if _, _, _, _, err := flow1.Start("new-org"); err != nil {
		t.Fatalf("Start1: %v", err)
	}
	if st := flow1.Poll(); st.Status != "authorized" || st.Account == nil || st.Account.Name != "new-org" {
		t.Fatalf("第一个团队应落为 new-org, got %+v", st)
	}

	// userId 为空串的同名登录 → 必须落为 new-org-2（不覆盖）
	meBody = []byte(`{"id":""}`)
	flow2 := NewLoginFlow(r)
	if _, _, _, _, err := flow2.Start("new-org"); err != nil {
		t.Fatalf("Start2: %v", err)
	}
	if st := flow2.Poll(); st.Status != "authorized" || st.Account == nil || st.Account.Name != "new-org-2" {
		t.Fatalf("空 userId 同名登录应落为 new-org-2, got %+v", st)
	}
	if n := len(r.ListAccounts()); n != 2 {
		t.Fatalf("应有 2 个账号, got %d", n)
	}
}

func TestAutoNameUpperUserID(t *testing.T) {
	// 逻辑审查 P2：user-+UserID 必须过 Slugify——大写 UserID 在 Linux 上
	// 落盘与查找大小写不一致会导致账号无法激活/刷新
	if got := AutoName(&oauth.Creds{UserID: "AB12"}); got != "user-ab12" {
		t.Fatalf("大写 UserID 应小写化, got %q", got)
	}
	if got := AutoName(&oauth.Creds{TeamName: "My Org", UserID: "AB12"}); got != "my-org" {
		t.Fatalf("teamName 优先路径不应受影响, got %q", got)
	}
}

func TestRemoveAccountRemovesSidecarFiles(t *testing.T) {
	// 逻辑审查 P2：删除账号必须清理 <slug>.key/.session 伴生文件——
	// 残留的 sk- key 会被同名重建的新账号静默复用
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if err := os.MkdirAll(AccountsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(AccountsDir+"/a.key", []byte("sk-a"), 0o600); err != nil {
		t.Fatalf("写 key: %v", err)
	}
	if err := os.WriteFile(AccountsDir+"/a.session", []byte("sid-a"), 0o600); err != nil {
		t.Fatalf("写 session: %v", err)
	}

	removed, _, err := r.RemoveAccount("a", nil)
	if err != nil || !removed {
		t.Fatalf("RemoveAccount: removed=%v err=%v", removed, err)
	}
	for _, f := range []string{"a.key", "a.session"} {
		if _, statErr := os.Stat(AccountsDir + "/" + f); !os.IsNotExist(statErr) {
			t.Fatalf("伴生文件 %s 应被清理", f)
		}
	}
}

func TestRemoveAccountFileRemoveWarn(t *testing.T) {
	// 审查记录 P2 #15：主文件删除失败（占用/权限）并入 warning——半删除态不得静默
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	// accounts/a.json → 非空目录（内含文件，os.Remove 必败）
	if err := os.Remove(AccountsDir + "/a.json"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(AccountsDir+"/a.json", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(AccountsDir+"/a.json/inner", []byte("x"), 0o600); err != nil {
		t.Fatalf("write inner: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(AccountsDir + "/a.json") })

	removed, _, err := r.RemoveAccount("a", nil)
	if !removed {
		t.Fatalf("删除已成立，removed 不得为 false（谎报）")
	}
	if err == nil {
		t.Fatalf("主文件删除失败应并入 warning")
	}
	if got := r.GetCurrentName(); got == "a" {
		t.Fatalf("index 中该账号应已移除")
	}
}

func TestRemoveAccountCascadeSwitchFail(t *testing.T) {
	// 逻辑审查 P1：删除当前账号且级联激活失败——删除已成立（不得谎报 removed=false），
	// 且指向已删账号的 codely-creds.json 必须清除（不能继续用已删账号凭据承载 /v1）
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	if _, _, err := r.SaveAccount("b", fakeCreds("2", "B"), false, nil); err != nil {
		t.Fatalf("SaveAccount b: %v", err)
	}
	// 破坏 b 的凭据文件 → 级联激活 ActivateAccount(b) 失败
	if err := os.Remove(AccountsDir + "/b.json"); err != nil {
		t.Fatalf("remove b.json: %v", err)
	}
	if _, err := os.Stat(oauth.CredsFile); err != nil {
		t.Fatalf("前置：a 为激活账号，codely-creds.json 应存在")
	}

	removed, next, err := r.RemoveAccount("a", nil)
	if !removed {
		t.Fatalf("删除已成立，removed 不应为 false（谎报）")
	}
	if err == nil {
		t.Fatalf("级联切换失败应返回 warning 错误")
	}
	if _, statErr := os.Stat(oauth.CredsFile); !os.IsNotExist(statErr) {
		t.Fatalf("指向已删账号的激活凭据应被清除")
	}
	if next == "" {
		t.Fatalf("nextCurrent 应指向剩余账号")
	}
}

// ------------- 凭据一致性（审查记录 P1-1/2/4/5） -------------

func TestSaveAccountActivateOrdering(t *testing.T) {
	// P1-1：SaveAccount(activate) 必须先写激活凭据再提交 index——SaveCreds 失败时
	// index.current 不得前移（否则 "index 指向新账号而激活凭据仍是旧账号" 且无自愈）
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	// 把 codely-creds.json 替换为目录 → SaveCreds 必失败（注入写盘故障）
	if err := os.Remove(oauth.CredsFile); err != nil {
		t.Fatalf("remove creds: %v", err)
	}
	if err := os.MkdirAll(oauth.CredsFile, 0o755); err != nil {
		t.Fatalf("mkdir creds: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(oauth.CredsFile) })

	if _, _, err := r.SaveAccount("b", fakeCreds("2", "B"), true, nil); err == nil {
		t.Fatalf("SaveCreds 失败应上报错误")
	}
	if got := r.GetCurrentName(); got != "a" {
		t.Fatalf("SaveCreds 失败后 index.current 不应前移，got %q", got)
	}
}

func TestSyncActivationCreds(t *testing.T) {
	// P1-2：回写激活文件前必须核验仍是当前账号（持锁原子），切换后拒绝（防串号）
	r := setup(t)
	oldBase := oauth.Base
	oauth.Base = "http://127.0.0.1:1" // ActivateAccount 预取指向不可达地址（快速失败，不触真实上游）
	t.Cleanup(func() { oauth.Base = oldBase })

	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	if _, _, err := r.SaveAccount("b", fakeCreds("2", "B"), false, nil); err != nil {
		t.Fatalf("SaveAccount b: %v", err)
	}

	rotated := fakeCreds("1", "A")
	rotated.RefreshToken = "rotated-rt"
	if err := r.SyncActivationCreds("a", rotated); err != nil {
		t.Fatalf("当前账号应允许回写: %v", err)
	}
	if cur := oauth.LoadCreds(); cur == nil || cur.RefreshToken != "rotated-rt" {
		t.Fatalf("回写未生效: %+v", cur)
	}

	// 切到 b → a 的回写必须被拒且不影响 b 的激活凭据
	if _, _, err := r.ActivateAccount("b", nil); err != nil {
		t.Fatalf("ActivateAccount b: %v", err)
	}
	if err := r.SyncActivationCreds("a", fakeCreds("1", "A")); err == nil {
		t.Fatalf("已切换后应拒绝回写")
	}
	if cur := oauth.LoadCreds(); cur == nil || cur.UserID != "2" {
		t.Fatalf("串号防护失效: %+v", cur)
	}
}

func TestSyncCurrentFromActivation(t *testing.T) {
	// P1-4 反向：全局轮换后激活库同步回 per-slug 库；已一致则不动
	r := setup(t)
	stale := fakeCreds("1", "A")
	stale.RefreshToken = "stale-rt"
	if _, _, err := r.SaveAccount("a", stale, true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	// 模拟全局链已轮换激活库（a.json 仍陈旧）
	fresh := fakeCreds("1", "A")
	fresh.RefreshToken = "fresh-rt"
	if err := fresh.SaveCreds(); err != nil {
		t.Fatalf("写激活库: %v", err)
	}

	r.SyncCurrentFromActivation()
	got := r.LoadAccountCreds("a")
	if got == nil || got.RefreshToken != "fresh-rt" {
		t.Fatalf("反向同步失败: %+v", got)
	}
	// 已一致 → 不再改写（防覆盖池路径的更新）
	r.SyncCurrentFromActivation()
	if got := r.LoadAccountCreds("a"); got == nil || got.RefreshToken != "fresh-rt" {
		t.Fatalf("幂等性破坏: %+v", got)
	}
}

func TestSyncCredsByIdentity(t *testing.T) {
	// P1-5 救援路径：激活已切换时轮换凭据按身份落到所属 per-slug 文件（防账号刷废）
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	rotated := fakeCreds("1", "A")
	rotated.RefreshToken = "rotated-rt"
	if err := r.SyncCredsByIdentity(rotated); err != nil {
		t.Fatalf("按身份回写失败: %v", err)
	}
	if got := r.LoadAccountCreds("a"); got == nil || got.RefreshToken != "rotated-rt" {
		t.Fatalf("回写未生效: %+v", got)
	}
	if err := r.SyncCredsByIdentity(fakeCreds("99", "X")); err == nil {
		t.Fatalf("未匹配应报错")
	}
}

func TestRestoreActivationLocked(t *testing.T) {
	// 复审 P1-1 回滚本体：prevCurrent 非空 → 恢复其凭据；空 → 删除激活文件
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	// 模拟分歧态：激活文件已被写成 b 的凭据（a.json 仍是 A）
	if err := fakeCreds("2", "B").SaveCreds(); err != nil {
		t.Fatalf("写激活库: %v", err)
	}
	r.mu.Lock()
	r.restoreActivationLocked("a")
	r.mu.Unlock()
	cur := oauth.LoadCreds()
	if cur == nil || cur.UserID != "1" {
		t.Fatalf("应回滚为 a: %+v", cur)
	}

	// prevCurrent 为空（此前无激活账号）→ 删除激活文件回到"未激活"一致态
	r.mu.Lock()
	r.restoreActivationLocked("")
	r.mu.Unlock()
	if _, err := os.Stat(oauth.CredsFile); !os.IsNotExist(err) {
		t.Fatalf("空 prevCurrent 应删除激活文件")
	}
}

func TestActivateAccountRollbackOnIndexFailure(t *testing.T) {
	// 复审 P1-1：ActivateAccount 的 saveIndex 失败分支必须走到回滚
	//（prevCurrent="" 场景 → 删除激活文件；否则留下"激活文件=B、index=A"分歧）
	r := setup(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("blocker: %v", err)
	}
	oldIndex := IndexFile
	IndexFile = filepath.Join(blocker, "sub", "index.json") // MkdirAll 撞文件 → saveIndex 必败
	t.Cleanup(func() { IndexFile = oldIndex })

	// 预置 b 凭据文件（直落磁盘，不经 index——绕过 loadIndex 的空 idx 判定）
	if err := os.MkdirAll(AccountsDir, 0o755); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := writeJSON(AccountsDir+"/b.json", fakeCreds("2", "B")); err != nil {
		t.Fatalf("预置 b.json: %v", err)
	}

	if _, _, err := r.ActivateAccount("b", nil); err == nil {
		t.Fatalf("saveIndex 失败应上报错误")
	}
	if _, err := os.Stat(oauth.CredsFile); !os.IsNotExist(err) {
		t.Fatalf("回滚应删除激活文件（prevCurrent 为空）")
	}
}

func TestSyncCurrentFromActivationIdentityGuard(t *testing.T) {
	// 复审 P1-1 第二道防线：激活库与 per-slug 凭据身份不一致时拒绝覆盖
	//（防历史分歧残留被放大成跨账号凭据损毁）
	r := setup(t)
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), true, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	// 激活库被写成 B（模拟分歧残留），a.json 仍是 A
	if err := fakeCreds("2", "B").SaveCreds(); err != nil {
		t.Fatalf("写激活库: %v", err)
	}

	r.SyncCurrentFromActivation()
	got := r.LoadAccountCreds("a")
	if got == nil || got.UserID != "1" {
		t.Fatalf("身份不一致时不得覆盖 per-slug 凭据: %+v", got)
	}
}

func TestSlugifyReserveIndex(t *testing.T) {
	// 稳定性审计 F5：slug "index" 会与注册表文件 accounts/index.json 同名互覆 → 预留拒绝
	if got := Slugify("index"); got != "" {
		t.Fatalf(`Slugify("index") 应为空（预留字），got %q`, got)
	}
	if Slugify("normal-name") == "" {
		t.Fatalf("普通名不应被拒绝")
	}
}

func TestListSlugsCache(t *testing.T) {
	// 性能审计 P2：accounts 目录 mtime 缓存——增删账号后缓存失效仍正确
	r := setup(t)
	if got := r.ListSlugs(); len(got) != 0 {
		t.Fatalf("空注册表应无 slug, got %v", got)
	}
	if _, _, err := r.SaveAccount("a", fakeCreds("1", "A"), false, nil); err != nil {
		t.Fatalf("SaveAccount a: %v", err)
	}
	_ = r.ListSlugs() // 填充缓存
	if _, _, err := r.SaveAccount("b", fakeCreds("2", "B"), false, nil); err != nil {
		t.Fatalf("SaveAccount b: %v", err)
	}
	got := r.ListSlugs()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("新增账号后应失效缓存, got %v", got)
	}
	removed, _, err := r.RemoveAccount("b", nil)
	if err != nil || !removed {
		t.Fatalf("RemoveAccount b: removed=%v err=%v", removed, err)
	}
	got = r.ListSlugs()
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("删除账号后应失效缓存, got %v", got)
	}
}

func TestDeviceLoginSameNameCollision(t *testing.T) {
	// 稳定性审计 F5：碰撞检查必须在 slug 域——两个同名（不同 user）团队应落为
	// new-org 与 new-org-2，而非后者静默覆盖前者
	meBody := []byte(`{"id":77777}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/initiate":
			_, _ = w.Write([]byte(`{"auth_request_token":"art","verification_uri_complete":"https://x/a","user_code":"ABC-123","interval":1,"expires_in":600}`))
		case "/auth/device/poll":
			_, _ = w.Write([]byte(`{"status":"authorized","authorization_code":"authcode"}`))
		case "/auth/device/exchange":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/auth/external/me":
			_, _ = w.Write(meBody)
		case "/api/teams":
			_, _ = w.Write([]byte(`{"current_team_id":"team-7","teams":[{"team_id":"team-7","team_name":"New Org","is_current":true}]}`))
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)

	flow1 := NewLoginFlow(r)
	if _, _, _, _, err := flow1.Start("new-org"); err != nil {
		t.Fatalf("Start1: %v", err)
	}
	if st := flow1.Poll(); st.Status != "authorized" || st.Account == nil || st.Account.Name != "new-org" {
		t.Fatalf("第一个团队应落为 new-org, got %+v", st)
	}

	// 同名团队、不同 user
	meBody = []byte(`{"id":88888}`)
	flow2 := NewLoginFlow(r)
	if _, _, _, _, err := flow2.Start("new-org"); err != nil {
		t.Fatalf("Start2: %v", err)
	}
	if st := flow2.Poll(); st.Status != "authorized" || st.Account == nil || st.Account.Name != "new-org-2" {
		t.Fatalf("同名第二团队应落为 new-org-2, got %+v", st)
	}
	if n := len(r.ListAccounts()); n != 2 {
		t.Fatalf("应有 2 个账号, got %d", n)
	}
}

func TestActivateAccountPrefetchOutsideLock(t *testing.T) {
	// 稳定性审计 F3：sk- 密钥预取走网络（最长 30s），不得持 r.mu——
	// 否则一次切号会阻塞 /healthz 与免调度模式的每条 /v1 请求
	var keyCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/api-token/cli-api-key" {
			atomic.AddInt32(&keyCalls, 1)
			time.Sleep(1 * time.Second) // 放大持锁窗口（若仍在锁内）
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_key":"sk-prefetch"}`))
			return
		}
		http.Error(w, "nf", 404)
	}))
	defer srv.Close()
	oldBase := oauth.Base
	oauth.Base = srv.URL
	t.Cleanup(func() { oauth.Base = oldBase })

	r := setup(t)
	exp := time.Now().UnixMilli() + 3600*1000
	creds := fakeCreds("1", "Org")
	creds.ExpiryDate = &exp
	if _, _, err := r.SaveAccount("org", creds, false, nil); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := r.ActivateAccount("org", nil); err != nil {
			t.Errorf("ActivateAccount: %v", err)
		}
	}()
	time.Sleep(100 * time.Millisecond) // 让激活进入预取阶段

	located := make(chan struct{})
	go func() {
		_ = r.GetCurrentMeta() // 持 r.mu 的读路径（/healthz 与免调度 Pick 同款）
		close(located)
	}()
	select {
	case <-located:
		// 未被预取阻塞 ✓
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("GetCurrentMeta 被锁外的预取网络调用阻塞（F3 未修复）")
	}
	<-done
	if atomic.LoadInt32(&keyCalls) != 1 {
		t.Fatalf("预取应恰好 1 次, got %d", atomic.LoadInt32(&keyCalls))
	}
}

// TestLoadIndexCorruptedFile（稳定性审计 E）：index.json 半写/损坏 → loadIndex
// 返回空注册表且不 panic（损坏现在会记入日志；半写本身由 atomicfile 从根上杜绝）。
func TestLoadIndexCorruptedFile(t *testing.T) {
	r := setup(t)
	if err := os.MkdirAll(AccountsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(IndexFile, []byte(`{"current":`), 0o600); err != nil { // 模拟半写
		t.Fatalf("写损坏 index: %v", err)
	}
	idx := r.loadIndex()
	if idx == nil || len(idx.Accounts) != 0 {
		t.Fatalf("损坏 index 应返回空注册表, got %+v", idx)
	}
}