package server

import (
	"errors"
	"testing"
	"time"

	"work2api/internal/provider"
)

func mi(ids ...string) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, provider.ModelInfo{ID: id, Name: id})
	}
	return out
}

func modelIDs(list []provider.ModelInfo) string {
	out := ""
	for i, m := range list {
		if i > 0 {
			out += ","
		}
		out += m.ID
	}
	return out
}

// 还没有上游结果时用静态兜底表，并报告「非实时」。
func TestRuntimeModelsFallsBackToStatic(t *testing.T) {
	rt := &Runtime{StaticModels: mi("a", "b")}
	list, live := rt.Models()
	if live {
		t.Fatal("没有上游结果时 live 应为 false")
	}
	if modelIDs(list) != "a,b" {
		t.Fatalf("got %q，期望静态兜底表", modelIDs(list))
	}
}

// 有上游结果时以上游为准，静态表被覆盖。
func TestRuntimeModelsPrefersLive(t *testing.T) {
	rt := &Runtime{StaticModels: mi("a", "b")}
	at := time.Now()
	rt.SetModels(mi("x", "y", "z"), at, nil)

	list, live := rt.Models()
	if !live {
		t.Fatal("有上游结果时 live 应为 true")
	}
	if modelIDs(list) != "x,y,z" {
		t.Fatalf("got %q，期望上游结果", modelIDs(list))
	}
	live2, n, gotAt, errMsg := rt.ModelsInfo()
	if !live2 || n != 3 || !gotAt.Equal(at) || errMsg != "" {
		t.Fatalf("ModelsInfo = (%v,%d,%v,%q)", live2, n, gotAt, errMsg)
	}
}

// 关键：某次拉取失败不能把已有结果清空、也不能退回兜底表 ——
// 上游偶发 500 不该让面板的模型列表忽然变样。
func TestRuntimeKeepsLastGoodOnFailure(t *testing.T) {
	rt := &Runtime{StaticModels: mi("static1")}
	at := time.Now()
	rt.SetModels(mi("live1", "live2"), at, nil)

	rt.SetModels(nil, time.Now(), errors.New("上游 500"))

	list, live := rt.Models()
	if !live {
		t.Fatal("拉取失败后仍应保留上一次的上游结果")
	}
	if modelIDs(list) != "live1,live2" {
		t.Fatalf("got %q，期望保留上一次结果", modelIDs(list))
	}
	_, n, gotAt, errMsg := rt.ModelsInfo()
	if n != 2 || !gotAt.Equal(at) {
		t.Fatalf("失败不应覆盖时间戳/条数：n=%d at=%v", n, gotAt)
	}
	if errMsg == "" {
		t.Fatal("失败原因应被记录，供面板展示")
	}
}

// 首次就失败时退回静态兜底，并带出错误信息。
func TestRuntimeFirstFetchFailureUsesStatic(t *testing.T) {
	rt := &Runtime{StaticModels: mi("static1")}
	rt.SetModels(nil, time.Now(), errors.New("网络不通"))

	list, live := rt.Models()
	if live {
		t.Fatal("从未成功拉取时 live 应为 false")
	}
	if modelIDs(list) != "static1" {
		t.Fatalf("got %q，期望静态兜底表", modelIDs(list))
	}
	_, n, _, errMsg := rt.ModelsInfo()
	if n != 1 || errMsg == "" {
		t.Fatalf("ModelsInfo = (n=%d, err=%q)", n, errMsg)
	}
}
