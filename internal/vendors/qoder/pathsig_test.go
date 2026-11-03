package qoder

import "testing"

// PathSigFrom 只剥掉开头一次 /algo，其余路径原样保留（签名依赖它，行为必须稳定）。
func TestPathSigFrom(t *testing.T) {
	cases := map[string]string{
		"https://api.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?x=1": "/api/v2/service/pro/sse/agent_chat_generation",
		"https://api.qoder.sh/api/v1/user":                                           "/api/v1/user",
		"https://api.qoder.sh/algo/algo/api":                                         "/algo/api",
		"https://api.qoder.sh/algorithm/data":                                        "rithm/data",
		"https://api.qoder.sh":                                                       "",
		"https://api.qoder.sh/api/algo/in/middle":                                    "/api/algo/in/middle",
	}
	for raw, want := range cases {
		got, err := PathSigFrom(raw)
		if err != nil || got != want {
			t.Fatalf("PathSigFrom(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := PathSigFrom("://bad"); err == nil {
		t.Fatal("invalid URL must return an error")
	}
}
