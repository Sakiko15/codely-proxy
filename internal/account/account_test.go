package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		UserID:       userID,
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

// jsonDecode 辅助（编译用）
var _ = json.Marshal

func TestSlugifyReserveIndex(t *testing.T) {
	// 稳定性审计 F5：slug "index" 会与注册表文件 accounts/index.json 同名互覆 → 预留拒绝
	if got := Slugify("index"); got != "" {
		t.Fatalf(`Slugify("index") 应为空（预留字），got %q`, got)
	}
	if Slugify("normal-name") == "" {
		t.Fatalf("普通名不应被拒绝")
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