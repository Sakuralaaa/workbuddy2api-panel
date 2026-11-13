// signins.go panel-v3：导入文件的第二种形态——WorkBuddy 桌面端 .info（{auth, account}）与
// 登录 token JSON（{accessToken/refreshToken/…}，单个 / 列表 / 嵌套在 map 里）。
// 解析规则移植自 WorkBuddy-plugins v0.3.5 internal/wb/signin.go SignInsIn（不向上游发任何请求）。
// 解析结果统一转换成 cockpitAccount，走 importCockpit 既有的落盘 / 入池 / 签到链路。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errSealedSignIn 桌面端加密保存的凭证（$wbEncrypted），无法直接导入。
var errSealedSignIn = errors.New("文件里的凭证被桌面端加密（$wbEncrypted），无法导入；请改用浏览器登录")

func siStr(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%.0f", t)
	}
	return ""
}

func siNum(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		var f float64
		_, _ = fmt.Sscan(t, &f)
		return f
	}
	return 0
}

func siSealed(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	x, has := m["$wbEncrypted"]
	if !has || x == nil {
		return false
	}
	if b, ok := x.(bool); ok {
		return b
	}
	return true
}

func siFirst(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// siAtMS 到期时刻（毫秒）：绝对值优先；秒级时间戳自动换算；否则 now + 有效期秒数。
func siAtMS(at, in any, now time.Time) int64 {
	if n := siNum(at); n > 0 {
		if n < 1e11 { // 秒
			return int64(n * 1000)
		}
		return int64(n)
	}
	if n := siNum(in); n > 0 {
		return now.UnixMilli() + int64(n*1000)
	}
	return 0
}

// signInsAsCockpit 从任意 JSON 里找出 WorkBuddy 登录凭证。
func signInsAsCockpit(raw []byte) ([]cockpitAccount, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("不是 JSON：%w", err)
	}
	now := time.Now()
	var out []cockpitAccount
	sealed := false
	var walk func(o any, depth int)
	walk = func(o any, depth int) {
		if depth > 4 {
			return
		}
		switch t := o.(type) {
		case []any:
			for _, x := range t {
				walk(x, depth+1)
			}
		case map[string]any:
			if a, ok := t["auth"].(map[string]any); ok {
				_, hasA := a["accessToken"]
				_, hasR := a["refreshToken"]
				if hasA || hasR {
					if siSealed(a["accessToken"]) || siSealed(a["refreshToken"]) {
						sealed = true
						return
					}
					c := cockpitAccount{
						AccessToken: siStr(a["accessToken"]), RefreshToken: siStr(a["refreshToken"]),
						ExpiresAt: siAtMS(a["expiresAt"], a["expiresIn"], now), Domain: siStr(a["domain"]),
					}
					if acct, ok := t["account"].(map[string]any); ok {
						c.UID = siStr(acct["uid"])
						if !siSealed(acct["nickname"]) {
							c.Nickname = siStr(acct["nickname"])
						}
					}
					out = append(out, c)
					return
				}
			}
			access := siFirst(t, "access", "accessToken", "access_token")
			refresh := siFirst(t, "refresh", "refreshToken", "refresh_token")
			if siSealed(access) || siSealed(refresh) {
				sealed = true
				return
			}
			if siStr(access) != "" || siStr(refresh) != "" {
				c := cockpitAccount{
					AccessToken: siStr(access), RefreshToken: siStr(refresh),
					ExpiresAt: siAtMS(siFirst(t, "expires", "expiresAt", "expires_at"), siFirst(t, "expiresIn", "expires_in"), now),
					Domain:    siStr(t["domain"]), UID: siStr(t["uid"]), Nickname: siStr(t["nickname"]),
				}
				if c.Nickname == "" {
					c.Nickname = siStr(t["accountId"])
				}
				out = append(out, c)
				return
			}
			for _, x := range t {
				walk(x, depth+1)
			}
		}
	}
	walk(v, 0)
	if len(out) == 0 {
		if sealed {
			return nil, errSealedSignIn
		}
		return nil, errors.New("文件里没有 WorkBuddy 登录凭证")
	}
	return out, nil
}

// looksCockpit 判断解析出的数组是否真是 cockpit tools 导出格式（至少一项带 token）。
func looksCockpit(list []cockpitAccount) bool {
	for _, a := range list {
		if a.AccessToken != "" || a.RefreshToken != "" {
			return true
		}
	}
	return false
}
